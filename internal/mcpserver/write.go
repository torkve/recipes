package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"recipes/internal/cookbook"
	"recipes/internal/models"
	"recipes/internal/stepsmd"
	"recipes/internal/store"
)

var errReadOnly = errors.New("this connection has read-only access to the cookbook; reconnect the app and allow editing recipes")

type createRecipeIn struct {
	Title         string            `json:"title" jsonschema:"dish name"`
	CategoryID    int64             `json:"category_id,omitempty" jsonschema:"existing category id (from list_categories)"`
	Category      string            `json:"category,omitempty" jsonschema:"category name or full path (e.g. Супы / Холодные); a new top-level category is created if no category matches. Used when category_id is not given"`
	Ingredients   []ingredientBlock `json:"ingredients,omitempty"`
	StepsMarkdown string            `json:"steps_markdown,omitempty" jsonschema:"cooking steps in Markdown"`
}

func (t *tools) createRecipe(ctx context.Context, _ *mcp.CallToolRequest, in createRecipeIn) (*mcp.CallToolResult, recipe, error) {
	if !t.canWrite(ctx) {
		return nil, recipe{}, errReadOnly
	}
	cat, err := t.categoryRef(ctx, in.CategoryID, in.Category)
	if err != nil {
		return nil, recipe{}, err
	}
	steps, err := stepsmd.ToHTML(in.StepsMarkdown, t.d.BaseURL)
	if err != nil {
		return nil, recipe{}, err
	}
	d := cookbook.Draft{
		Title:       in.Title,
		Category:    cat,
		Ingredients: ingredientsIn(in.Ingredients),
		StepsHTML:   steps,
	}
	if t.d.UserID != nil {
		if id, ok := t.d.UserID(ctx); ok {
			d.OwnerID = &id
		}
	}
	rec, err := t.d.Cookbook.Create(ctx, d)
	if err != nil {
		return nil, recipe{}, writeError(err, 0)
	}
	return t.saved(ctx, rec)
}

type updateRecipeIn struct {
	ID               int64              `json:"id" jsonschema:"recipe id"`
	ExpectedRevision int64              `json:"expected_revision" jsonschema:"revision returned by get_recipe; the update fails if the recipe changed since"`
	Title            *string            `json:"title,omitempty" jsonschema:"new dish name"`
	CategoryID       int64              `json:"category_id,omitempty" jsonschema:"move to this existing category"`
	Category         string             `json:"category,omitempty" jsonschema:"move to the category with this name or path, creating a top-level one if none matches"`
	Ingredients      *[]ingredientBlock `json:"ingredients,omitempty" jsonschema:"replaces all ingredient blocks"`
	StepsMarkdown    *string            `json:"steps_markdown,omitempty" jsonschema:"replaces the steps (Markdown)"`
}

func (t *tools) updateRecipe(ctx context.Context, _ *mcp.CallToolRequest, in updateRecipeIn) (*mcp.CallToolResult, recipe, error) {
	if !t.canWrite(ctx) {
		return nil, recipe{}, errReadOnly
	}
	var p cookbook.Patch
	p.Title = in.Title
	if in.CategoryID != 0 || strings.TrimSpace(in.Category) != "" {
		cat, err := t.categoryRef(ctx, in.CategoryID, in.Category)
		if err != nil {
			return nil, recipe{}, err
		}
		p.Category = &cat
	}
	if in.Ingredients != nil {
		ings := ingredientsIn(*in.Ingredients)
		p.Ingredients = &ings
	}
	if in.StepsMarkdown != nil {
		steps, err := stepsmd.ToHTML(*in.StepsMarkdown, t.d.BaseURL)
		if err != nil {
			return nil, recipe{}, err
		}
		p.StepsHTML = &steps
	}
	rev := in.ExpectedRevision
	rec, err := t.d.Cookbook.Update(ctx, in.ID, p, &rev)
	if err != nil {
		return nil, recipe{}, writeError(err, in.ID)
	}
	return t.saved(ctx, rec)
}

func (t *tools) canWrite(ctx context.Context) bool {
	return t.d.CanWrite != nil && t.d.CanWrite(ctx)
}

// categoryRef resolves the category arguments of a write tool: an id must
// exist; a name matching a category's full path selects it, any other name is
// passed on to be matched by name or created.
func (t *tools) categoryRef(ctx context.Context, id int64, name string) (cookbook.CategoryRef, error) {
	name = strings.TrimSpace(name)
	if id != 0 {
		return cookbook.CategoryRef{ID: id}, nil
	}
	if name == "" {
		return cookbook.CategoryRef{}, nil
	}
	cats, err := t.d.Store.ListCategories(ctx)
	if err != nil {
		return cookbook.CategoryRef{}, err
	}
	want := strings.ToLower(strings.Join(strings.Fields(name), " "))
	for cid, path := range categoryPaths(cats) {
		if strings.ToLower(path) == want {
			return cookbook.CategoryRef{ID: cid}, nil
		}
	}
	if strings.Contains(name, "/") {
		return cookbook.CategoryRef{}, fmt.Errorf("no category has path %q; use list_categories, or pass a plain name to create a top-level category", name)
	}
	return cookbook.CategoryRef{Name: name}, nil
}

// ingredientsIn trims ingredient input and drops empty items and blocks.
func ingredientsIn(blocks []ingredientBlock) []models.IngredientBlock {
	out := make([]models.IngredientBlock, 0, len(blocks))
	for _, b := range blocks {
		ib := models.IngredientBlock{Subtitle: strings.TrimSpace(b.Subtitle)}
		for _, it := range b.Items {
			if it = strings.TrimSpace(it); it != "" {
				ib.Items = append(ib.Items, it)
			}
		}
		if ib.Subtitle != "" || len(ib.Items) > 0 {
			out = append(out, ib)
		}
	}
	return out
}

// writeError turns a cookbook or store error into a message the model can act
// on. id is the recipe being updated (0 on create).
func writeError(err error, id int64) error {
	switch {
	case errors.Is(err, cookbook.ErrTitleRequired):
		return errors.New("title is required")
	case errors.Is(err, cookbook.ErrCategoryRequired):
		return errors.New("a category is required: pass an existing category_id or a category name")
	case errors.Is(err, cookbook.ErrForeignImage):
		return errors.New("the steps reference an image that belongs to another recipe; remove it")
	case errors.Is(err, cookbook.ErrMissingImage):
		return errors.New("an image in the steps is no longer available; upload it again with upload_image")
	case errors.Is(err, store.ErrConflict):
		return fmt.Errorf("recipe %d was changed since expected_revision; call get_recipe again and reapply your change", id)
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("recipe %d not found", id)
	}
	return err
}

func (t *tools) saved(ctx context.Context, rec *models.Recipe) (*mcp.CallToolResult, recipe, error) {
	cats, err := t.d.Store.ListCategories(ctx)
	if err != nil {
		return nil, recipe{}, err
	}
	return nil, t.recipeOut(rec, categoryPaths(cats)), nil
}
