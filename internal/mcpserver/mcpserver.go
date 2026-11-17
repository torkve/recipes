// Package mcpserver exposes the cookbook to MCP clients (Claude, ChatGPT) over
// Streamable HTTP: tools to browse categories, list, search and read recipes.
//
// The package does no authentication; the caller mounts Handler behind a bearer
// token check.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"recipes/internal/cattree"
	"recipes/internal/models"
	"recipes/internal/search"
	"recipes/internal/stepsmd"
	"recipes/internal/store"
)

const (
	defaultListLimit   = 50
	maxListLimit       = 100
	defaultSearchLimit = 20
	maxSearchLimit     = 50
)

const instructions = `Family cookbook. Recipes are mostly written in Russian.
Use search_recipes to find recipes by dish, ingredient or description (it matches meaning, not only words), list_categories and list_recipes to browse, and get_recipe for the full recipe. Recipe steps are Markdown.`

// Deps are the services the tools read from.
type Deps struct {
	Store  *store.Store
	Search *search.Service
	// BaseURL is the public site URL (scheme://host) used for recipe and image
	// links.
	BaseURL string
}

// New builds the MCP server with the cookbook tools registered.
func New(d Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "recipes",
		Title:   "Семейная кулинарная книга",
		Version: "1.0.0",
	}, &mcp.ServerOptions{Instructions: instructions})

	t := &tools{d: d}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_categories",
		Title:       "List categories",
		Description: "List all recipe categories with their hierarchy path and recipe counts.",
		Annotations: readOnly("List categories"),
	}, t.listCategories)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_recipes",
		Title:       "List recipes",
		Description: "List recipes newest first, optionally within a category (including its subcategories). Paginated: pass next_cursor back as cursor to get the next page.",
		Annotations: readOnly("List recipes"),
	}, t.listRecipes)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_recipes",
		Title:       "Search recipes",
		Description: "Search recipes by free text: dish name, ingredients or a description of what to cook. Combines keyword and semantic matching; results are ordered by relevance.",
		Annotations: readOnly("Search recipes"),
	}, t.searchRecipes)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_recipe",
		Title:       "Get recipe",
		Description: "Get a full recipe: ingredients grouped in blocks, steps as Markdown, image URLs, and its current revision.",
		Annotations: readOnly("Get recipe"),
	}, t.getRecipe)
	return s
}

// Handler serves s over stateless Streamable HTTP with JSON responses.
func Handler(s *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
}

func readOnly(title string) *mcp.ToolAnnotations {
	closed := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closed}
}

type tools struct{ d Deps }

// --- list_categories ---

type listCategoriesIn struct{}

type category struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Path        string `json:"path" jsonschema:"full category path from the top level, e.g. Выпечка / Пироги"`
	ParentID    *int64 `json:"parent_id,omitempty"`
	RecipeCount int    `json:"recipe_count" jsonschema:"recipes directly in this category, not counting subcategories"`
}

type listCategoriesOut struct {
	Categories []category `json:"categories"`
}

func (t *tools) listCategories(ctx context.Context, _ *mcp.CallToolRequest, _ listCategoriesIn) (*mcp.CallToolResult, listCategoriesOut, error) {
	cats, err := t.d.Store.ListCategories(ctx)
	if err != nil {
		return nil, listCategoriesOut{}, err
	}
	paths := categoryPaths(cats)
	out := listCategoriesOut{Categories: make([]category, 0, len(cats))}
	for _, c := range cats {
		out.Categories = append(out.Categories, category{
			ID: c.ID, Name: c.Name, Path: paths[c.ID], ParentID: c.ParentID, RecipeCount: c.RecipeCount,
		})
	}
	return nil, out, nil
}

// --- list_recipes / search_recipes ---

type recipeSummary struct {
	ID         int64  `json:"id"`
	Title      string `json:"title"`
	CategoryID int64  `json:"category_id"`
	Category   string `json:"category" jsonschema:"category path"`
	URL        string `json:"url" jsonschema:"web page of the recipe"`
}

type listRecipesIn struct {
	CategoryID int64  `json:"category_id,omitempty" jsonschema:"only recipes in this category and its subcategories"`
	Limit      int    `json:"limit,omitempty" jsonschema:"page size, 1-100 (default 50)"`
	Cursor     string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

type listRecipesOut struct {
	Recipes    []recipeSummary `json:"recipes"`
	NextCursor string          `json:"next_cursor,omitempty" jsonschema:"present when more recipes follow"`
}

func (t *tools) listRecipes(ctx context.Context, _ *mcp.CallToolRequest, in listRecipesIn) (*mcp.CallToolResult, listRecipesOut, error) {
	limit := clamp(in.Limit, defaultListLimit, maxListLimit)
	offset := 0
	if in.Cursor != "" {
		n, err := strconv.Atoi(in.Cursor)
		if err != nil || n < 0 {
			return nil, listRecipesOut{}, errors.New("invalid cursor: pass next_cursor from a previous list_recipes result")
		}
		offset = n
	}
	cats, err := t.d.Store.ListCategories(ctx)
	if err != nil {
		return nil, listRecipesOut{}, err
	}
	filter, err := categoryFilter(cats, in.CategoryID)
	if err != nil {
		return nil, listRecipesOut{}, err
	}
	recs, err := t.d.Store.ListRecipes(ctx, filter, limit+1, offset)
	if err != nil {
		return nil, listRecipesOut{}, err
	}
	out := listRecipesOut{}
	if len(recs) > limit {
		recs = recs[:limit]
		out.NextCursor = strconv.Itoa(offset + limit)
	}
	out.Recipes = t.summaries(recs, categoryPaths(cats))
	return nil, out, nil
}

type searchRecipesIn struct {
	Query      string `json:"query" jsonschema:"what to look for: dish, ingredients or a description"`
	CategoryID int64  `json:"category_id,omitempty" jsonschema:"only recipes in this category and its subcategories"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum results, 1-50 (default 20)"`
}

type searchRecipesOut struct {
	Recipes      []recipeSummary `json:"recipes"`
	TotalMatches int             `json:"total_matches" jsonschema:"matches before applying limit"`
}

func (t *tools) searchRecipes(ctx context.Context, _ *mcp.CallToolRequest, in searchRecipesIn) (*mcp.CallToolResult, searchRecipesOut, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, searchRecipesOut{}, errors.New("query is required; use list_recipes to browse")
	}
	limit := clamp(in.Limit, defaultSearchLimit, maxSearchLimit)
	cats, err := t.d.Store.ListCategories(ctx)
	if err != nil {
		return nil, searchRecipesOut{}, err
	}
	filter, err := categoryFilter(cats, in.CategoryID)
	if err != nil {
		return nil, searchRecipesOut{}, err
	}
	recs, err := t.d.Search.Search(ctx, query, filter)
	if err != nil {
		return nil, searchRecipesOut{}, err
	}
	out := searchRecipesOut{TotalMatches: len(recs)}
	if len(recs) > limit {
		recs = recs[:limit]
	}
	out.Recipes = t.summaries(recs, categoryPaths(cats))
	return nil, out, nil
}

// --- get_recipe ---

type getRecipeIn struct {
	ID int64 `json:"id" jsonschema:"recipe id"`
}

type ingredientBlock struct {
	Subtitle string   `json:"subtitle,omitempty" jsonschema:"block heading, e.g. Тесто; empty for the main list"`
	Items    []string `json:"items"`
}

type recipe struct {
	ID            int64             `json:"id"`
	Title         string            `json:"title"`
	CategoryID    int64             `json:"category_id"`
	Category      string            `json:"category" jsonschema:"category path"`
	Ingredients   []ingredientBlock `json:"ingredients"`
	StepsMarkdown string            `json:"steps_markdown"`
	ImageURLs     []string          `json:"image_urls,omitempty"`
	URL           string            `json:"url" jsonschema:"web page of the recipe"`
	Revision      int64             `json:"revision" jsonschema:"content revision; changes on every edit"`
	UpdatedAt     string            `json:"updated_at" jsonschema:"RFC 3339 time of the last change"`
}

func (t *tools) getRecipe(ctx context.Context, _ *mcp.CallToolRequest, in getRecipeIn) (*mcp.CallToolResult, recipe, error) {
	rec, err := t.d.Store.GetRecipe(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, recipe{}, fmt.Errorf("recipe %d not found", in.ID)
	}
	if err != nil {
		return nil, recipe{}, err
	}
	cats, err := t.d.Store.ListCategories(ctx)
	if err != nil {
		return nil, recipe{}, err
	}
	return nil, t.recipeOut(rec, categoryPaths(cats)), nil
}

func (t *tools) recipeOut(rec *models.Recipe, paths map[int64]string) recipe {
	out := recipe{
		ID:            rec.ID,
		Title:         rec.Title,
		CategoryID:    rec.CategoryID,
		Category:      paths[rec.CategoryID],
		Ingredients:   make([]ingredientBlock, 0, len(rec.Ingredients)),
		StepsMarkdown: stepsmd.ToMarkdown(rec.StepsHTML, t.d.BaseURL),
		URL:           t.recipeURL(rec.ID),
		Revision:      rec.Revision,
		UpdatedAt:     rec.UpdatedAt.UTC().Format(time.RFC3339),
	}
	for _, b := range rec.Ingredients {
		items := b.Items
		if items == nil {
			items = []string{}
		}
		out.Ingredients = append(out.Ingredients, ingredientBlock{Subtitle: b.Subtitle, Items: items})
	}
	for _, img := range rec.Images {
		out.ImageURLs = append(out.ImageURLs, t.d.BaseURL+"/uploads/"+img.Filename)
	}
	return out
}

// --- helpers ---

func (t *tools) summaries(recs []models.Recipe, paths map[int64]string) []recipeSummary {
	out := make([]recipeSummary, 0, len(recs))
	for _, r := range recs {
		out = append(out, recipeSummary{
			ID: r.ID, Title: r.Title, CategoryID: r.CategoryID, Category: paths[r.CategoryID], URL: t.recipeURL(r.ID),
		})
	}
	return out
}

func (t *tools) recipeURL(id int64) string {
	return t.d.BaseURL + "/recipes/" + strconv.FormatInt(id, 10)
}

// categoryPaths maps each category id to its "Root / Child" path.
func categoryPaths(cats []models.Category) map[int64]string {
	out := make(map[int64]string, len(cats))
	for _, c := range cats {
		chain := cattree.Path(cats, c.ID)
		names := make([]string, len(chain))
		for i, p := range chain {
			names[i] = p.Name
		}
		out[c.ID] = strings.Join(names, " / ")
	}
	return out
}

// categoryFilter returns the category subtree ids for id, nil for id 0, or an
// error when id names no category.
func categoryFilter(cats []models.Category, id int64) ([]int64, error) {
	if id == 0 {
		return nil, nil
	}
	for _, c := range cats {
		if c.ID == id {
			return cattree.DescendantIDs(cats, id), nil
		}
	}
	return nil, fmt.Errorf("category %d not found; use list_categories", id)
}

// clamp returns def for n <= 0, else n capped at max.
func clamp(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
