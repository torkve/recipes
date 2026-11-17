package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"recipes/internal/cookbook"
	"recipes/internal/models"
	"recipes/internal/search"
	"recipes/internal/store"
)

const base = "https://recipes.example.com"

type fixture struct {
	cs                  *mcp.ClientSession
	st                  *store.Store
	soups, cold, baking *models.Category
	borscht, gazpacho   *models.Recipe
	owner               int64
	canWrite            atomic.Bool
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
	f := &fixture{st: st}
	u, err := st.CreateUser(ctx, "cook", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	f.owner = u.ID
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

	srv := New(Deps{
		Store: st, Search: search.New(st, nil, 0), Cookbook: cookbook.New(st, t.TempDir()), BaseURL: base,
		CanWrite: func(context.Context) bool { return f.canWrite.Load() },
		UserID:   func(context.Context) (int64, bool) { return f.owner, true },
		FetchImage: func(_ context.Context, u string) ([]byte, error) {
			switch u {
			case "https://img.example/cat.png":
				return []byte("\x89PNG\r\n\x1a\n0000"), nil
			case "https://img.example/page.html":
				return []byte("<html>not an image</html>"), nil
			}
			return nil, errors.New("blocked")
		},
	})
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

func TestToolAnnotations(t *testing.T) {
	f := setup(t)
	res, err := f.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// readOnly / destructive per tool.
	want := map[string][2]bool{
		"list_categories": {true, false},
		"list_recipes":    {true, false},
		"search_recipes":  {true, false},
		"get_recipe":      {true, false},
		"create_recipe":   {false, false},
		"update_recipe":   {false, true},
		"upload_image":    {false, false},
	}
	if len(res.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(res.Tools), len(want))
	}
	for _, tool := range res.Tools {
		w, ok := want[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint != w[0] {
			t.Errorf("%s: readOnlyHint = %v, want %v", tool.Name, a != nil && a.ReadOnlyHint, w[0])
			continue
		}
		// An absent destructiveHint means true (MCP spec default).
		if destructive := a.DestructiveHint == nil || *a.DestructiveHint; !w[0] && destructive != w[1] {
			t.Errorf("%s: destructive = %v, want %v", tool.Name, destructive, w[1])
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s: no output schema", tool.Name)
		}
	}
}

func TestCreateAndUpdateRecipe(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	args := map[string]any{
		"title":    "Окрошка",
		"category": "супы / холодные",
		"ingredients": []map[string]any{
			{"items": []string{"квас", " ", "огурцы"}},
			{"subtitle": "Заправка", "items": []string{"горчица"}},
		},
		"steps_markdown": "# Подготовка\n\nНарезать **всё**.\n\n<script>alert(1)</script>\n\n- охладить",
	}

	var out recipe
	if msg := f.call(t, "create_recipe", args, &out); !strings.Contains(msg, "read-only") {
		t.Fatalf("create without write access: error %q, want read-only", msg)
	}
	f.canWrite.Store(true)
	if msg := f.call(t, "create_recipe", args, &out); msg != "" {
		t.Fatal(msg)
	}
	if out.CategoryID != f.cold.ID || out.Category != "Супы / Холодные" {
		t.Errorf("category = %d %q, want cold soups", out.CategoryID, out.Category)
	}
	if len(out.Ingredients) != 2 || len(out.Ingredients[0].Items) != 2 {
		t.Errorf("ingredients = %+v, want blank item dropped", out.Ingredients)
	}
	if !strings.Contains(out.StepsMarkdown, "### Подготовка") || !strings.Contains(out.StepsMarkdown, "**всё**") ||
		strings.Contains(out.StepsMarkdown, "alert") {
		t.Errorf("steps_markdown = %q", out.StepsMarkdown)
	}
	rec, err := f.st.GetRecipe(ctx, out.ID)
	if err != nil || rec.OwnerID == nil || *rec.OwnerID != f.owner {
		t.Fatalf("owner not recorded: %+v %v", rec, err)
	}

	// A new name creates a top-level category; an unknown path is an error.
	var other recipe
	if msg := f.call(t, "create_recipe", map[string]any{"title": "Чай", "category": "Напитки"}, &other); msg != "" {
		t.Fatal(msg)
	}
	if other.Category != "Напитки" {
		t.Errorf("new category = %q", other.Category)
	}
	if msg := f.call(t, "create_recipe", map[string]any{"title": "x", "category": "Нет / Такой"}, &other); !strings.Contains(msg, "no category has path") {
		t.Errorf("unknown path: error %q", msg)
	}
	if msg := f.call(t, "create_recipe", map[string]any{"title": "x"}, &other); !strings.Contains(msg, "category is required") {
		t.Errorf("no category: error %q", msg)
	}

	// Partial update keeps the other fields and bumps the revision.
	var upd recipe
	if msg := f.call(t, "update_recipe", map[string]any{"id": out.ID, "expected_revision": out.Revision, "title": "Окрошка на квасе"}, &upd); msg != "" {
		t.Fatal(msg)
	}
	if upd.Title != "Окрошка на квасе" || upd.StepsMarkdown != out.StepsMarkdown || upd.Revision != out.Revision+1 {
		t.Errorf("after title update: %+v", upd)
	}

	// A stale revision is rejected.
	if msg := f.call(t, "update_recipe", map[string]any{"id": out.ID, "expected_revision": out.Revision, "steps_markdown": "x"}, &upd); !strings.Contains(msg, "was changed") {
		t.Errorf("stale revision: error %q", msg)
	}
	// So is a reference to another recipe's image.
	img := "![](" + base + "/uploads/a.png)"
	if msg := f.call(t, "update_recipe", map[string]any{"id": out.ID, "expected_revision": upd.Revision, "steps_markdown": img}, &upd); !strings.Contains(msg, "another recipe") {
		t.Errorf("foreign image: error %q", msg)
	}
	if msg := f.call(t, "update_recipe", map[string]any{"id": 9999, "expected_revision": 0, "title": "x"}, &upd); !strings.Contains(msg, "not found") {
		t.Errorf("missing recipe: error %q", msg)
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

func TestUploadImage(t *testing.T) {
	f := setup(t)
	var up uploadImageOut
	if msg := f.call(t, "upload_image", map[string]any{"url": "https://img.example/cat.png"}, &up); !strings.Contains(msg, "read-only") {
		t.Fatalf("upload without write access: error %q", msg)
	}
	f.canWrite.Store(true)

	// ChatGPT-style file parameter.
	if msg := f.call(t, "upload_image", map[string]any{"file": map[string]any{"download_url": "https://img.example/cat.png", "file_id": "f1"}}, &up); msg != "" {
		t.Fatal(msg)
	}
	if !strings.HasPrefix(up.URL, base+"/uploads/") || !strings.HasSuffix(up.URL, ".png") || up.Markdown != "![]("+up.URL+")" {
		t.Fatalf("upload result = %+v", up)
	}

	var rec recipe
	if msg := f.call(t, "create_recipe", map[string]any{
		"title": "Кот в тесте", "category_id": f.baking.ID, "steps_markdown": "Испечь.\n\n" + up.Markdown,
	}, &rec); msg != "" {
		t.Fatal(msg)
	}
	if len(rec.ImageURLs) != 1 || rec.ImageURLs[0] != up.URL || !strings.Contains(rec.StepsMarkdown, up.URL) {
		t.Fatalf("image not attached: urls %v steps %q", rec.ImageURLs, rec.StepsMarkdown)
	}

	cases := map[string]map[string]any{
		"not a supported image":   {"url": "https://img.example/page.html"},
		"could not get the image": {"url": "https://internal.example/x.png"},
		"url or file is required": {},
		"either url or file":      {"url": "https://img.example/cat.png", "file": map[string]any{"download_url": "https://img.example/cat.png"}},
	}
	for want, args := range cases {
		if msg := f.call(t, "upload_image", args, &up); !strings.Contains(msg, want) {
			t.Errorf("%v: error %q, want %q", args, msg, want)
		}
	}
}

func TestUploadImageAdvertisesFileParam(t *testing.T) {
	f := setup(t)
	res, err := f.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "upload_image" {
			continue
		}
		b, _ := json.Marshal(tool.Meta["openai/fileParams"])
		if string(b) != `["file"]` {
			t.Fatalf("openai/fileParams = %s, want [\"file\"]", b)
		}
		return
	}
	t.Fatal("upload_image not listed")
}
