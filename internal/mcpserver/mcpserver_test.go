package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"recipes/internal/models"
	"recipes/internal/search"
	"recipes/internal/store"
)

const base = "https://recipes.example.com"

type fixture struct {
	cs                  *mcp.ClientSession
	soups, cold, baking *models.Category
	borscht, gazpacho   *models.Recipe
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fixture{}
	f.soups, _ = st.GetOrCreateCategory(ctx, "Супы", models.SourceManual)
	f.cold, _ = st.GetOrCreateCategory(ctx, "Холодные", models.SourceManual)
	if err := st.SetCategoryParent(ctx, f.cold.ID, &f.soups.ID); err != nil {
		t.Fatal(err)
	}
	f.baking, _ = st.GetOrCreateCategory(ctx, "Выпечка", models.SourceManual)

	mk := func(title string, cat int64, steps string) *models.Recipe {
		r, err := st.CreateRecipe(ctx, store.RecipeInput{
			Title: title, CategoryID: cat, StepsHTML: steps,
			Ingredients: []models.IngredientBlock{{Items: []string{"вода"}}, {Subtitle: "Заправка", Items: []string{"сметана"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	f.borscht = mk("Борщ", f.soups.ID, `<p>Варить <strong>2 часа</strong>.</p><p><img src="/uploads/a.png" alt=""></p>`)
	f.gazpacho = mk("Гаспачо", f.cold.ID, "<p>Смешать.</p>")
	mk("Пирог", f.baking.ID, "<p>Печь.</p>")
	if _, err := st.AddImage(ctx, f.borscht.ID, "a.png", "image/png"); err != nil {
		t.Fatal(err)
	}

	srv := New(Deps{Store: st, Search: search.New(st, nil, 0), BaseURL: base})
	ts := httptest.NewServer(Handler(srv))
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	f.cs = cs
	return f
}

// call invokes a tool and decodes its structured output into out. It returns
// the result's error text when the tool reported an error.
func (f *fixture) call(t *testing.T, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := f.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var msgs []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msgs = append(msgs, tc.Text)
			}
		}
		return strings.Join(msgs, "; ")
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: decode %s: %v", name, b, err)
	}
	return ""
}

func titles(rs []recipeSummary) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Title
	}
	return out
}

func TestToolsAreReadOnly(t *testing.T) {
	f := setup(t)
	res, err := f.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"list_categories": true, "list_recipes": true, "search_recipes": true, "get_recipe": true}
	if len(res.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(res.Tools), len(want))
	}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s: not annotated read-only", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s: no output schema", tool.Name)
		}
	}
}

func TestListCategories(t *testing.T) {
	f := setup(t)
	var out listCategoriesOut
	if msg := f.call(t, "list_categories", nil, &out); msg != "" {
		t.Fatal(msg)
	}
	byID := map[int64]category{}
	for _, c := range out.Categories {
		byID[c.ID] = c
	}
	if got := byID[f.cold.ID]; got.Path != "Супы / Холодные" || got.ParentID == nil || *got.ParentID != f.soups.ID || got.RecipeCount != 1 {
		t.Errorf("cold soups = %+v", got)
	}
}

func TestListRecipesPaginationAndFilter(t *testing.T) {
	f := setup(t)
	var p1 listRecipesOut
	if msg := f.call(t, "list_recipes", map[string]any{"limit": 2}, &p1); msg != "" {
		t.Fatal(msg)
	}
	if len(p1.Recipes) != 2 || p1.NextCursor == "" {
		t.Fatalf("page 1 = %v cursor %q, want 2 recipes and a cursor", titles(p1.Recipes), p1.NextCursor)
	}
	var p2 listRecipesOut
	if msg := f.call(t, "list_recipes", map[string]any{"limit": 2, "cursor": p1.NextCursor}, &p2); msg != "" {
		t.Fatal(msg)
	}
	if len(p2.Recipes) != 1 || p2.NextCursor != "" {
		t.Fatalf("page 2 = %v cursor %q, want 1 recipe and no cursor", titles(p2.Recipes), p2.NextCursor)
	}

	var soups listRecipesOut
	if msg := f.call(t, "list_recipes", map[string]any{"category_id": f.soups.ID}, &soups); msg != "" {
		t.Fatal(msg)
	}
	got := strings.Join(titles(soups.Recipes), ",")
	if !strings.Contains(got, "Борщ") || !strings.Contains(got, "Гаспачо") || strings.Contains(got, "Пирог") {
		t.Errorf("soups subtree = %s, want Борщ and Гаспачо only", got)
	}
	for _, r := range soups.Recipes {
		if r.Title == "Гаспачо" && (r.Category != "Супы / Холодные" || r.URL != base+"/recipes/"+itoa(f.gazpacho.ID)) {
			t.Errorf("gazpacho summary = %+v", r)
		}
	}

	if msg := f.call(t, "list_recipes", map[string]any{"category_id": 9999}, &soups); !strings.Contains(msg, "not found") {
		t.Errorf("unknown category: error %q, want not found", msg)
	}
	if msg := f.call(t, "list_recipes", map[string]any{"cursor": "x"}, &soups); !strings.Contains(msg, "invalid cursor") {
		t.Errorf("bad cursor: error %q", msg)
	}
}

func TestSearchRecipes(t *testing.T) {
	f := setup(t)
	var out searchRecipesOut
	if msg := f.call(t, "search_recipes", map[string]any{"query": "борщ"}, &out); msg != "" {
		t.Fatal(msg)
	}
	if len(out.Recipes) != 1 || out.Recipes[0].ID != f.borscht.ID || out.TotalMatches != 1 {
		t.Fatalf("search борщ = %+v", out)
	}
	if msg := f.call(t, "search_recipes", map[string]any{"query": "  "}, &out); !strings.Contains(msg, "query is required") {
		t.Errorf("blank query: error %q", msg)
	}
}

func TestGetRecipe(t *testing.T) {
	f := setup(t)
	var out recipe
	if msg := f.call(t, "get_recipe", map[string]any{"id": f.borscht.ID}, &out); msg != "" {
		t.Fatal(msg)
	}
	if out.Title != "Борщ" || out.Category != "Супы" || out.Revision != 0 {
		t.Errorf("recipe header = %+v", out)
	}
	if !strings.Contains(out.StepsMarkdown, "**2 часа**") || !strings.Contains(out.StepsMarkdown, "]("+base+"/uploads/a.png)") {
		t.Errorf("steps_markdown = %q", out.StepsMarkdown)
	}
	if len(out.ImageURLs) != 1 || out.ImageURLs[0] != base+"/uploads/a.png" {
		t.Errorf("image_urls = %v", out.ImageURLs)
	}
	if len(out.Ingredients) != 2 || out.Ingredients[1].Subtitle != "Заправка" || out.Ingredients[1].Items[0] != "сметана" {
		t.Errorf("ingredients = %+v", out.Ingredients)
	}
	if msg := f.call(t, "get_recipe", map[string]any{"id": 9999}, &out); !strings.Contains(msg, "not found") {
		t.Errorf("missing recipe: error %q", msg)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
