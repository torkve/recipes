package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"recipes/internal/auth"
	"recipes/internal/config"
	"recipes/internal/models"
	"recipes/internal/store"
)

// testServer spins up the full web handler against a temp store with one admin.
func testServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{dir, filepath.Join(dir, "uploads"), filepath.Join(dir, "sessions")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Addr: ":0", DataDir: dir, SiteName: "Тест", SecureCookies: false}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("pw")
	if _, err := st.CreateUser(context.Background(), "admin", hash, true); err != nil {
		t.Fatal(err)
	}
	keys, err := auth.LoadOrCreateKeys(cfg.KeysPath())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, st, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse // don't auto-follow, so we can assert statuses
	}
	return c
}

var tokenRE = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func getPage(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func token(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	m := tokenRE.FindStringSubmatch(getPage(t, c, url))
	if m == nil {
		t.Fatalf("no csrf token on %s", url)
	}
	return m[1]
}

func login(t *testing.T, c *http.Client, base string) {
	t.Helper()
	tok := token(t, c, base+"/admin/login")
	resp, err := c.PostForm(base+"/admin/login", url.Values{
		"username": {"admin"}, "password": {"pw"}, "csrf_token": {tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status %d", resp.StatusCode)
	}
}

func TestCategoryTree(t *testing.T) {
	parent := int64(1)
	cats := []models.Category{
		{ID: 1, Name: "Рецепты"},
		{ID: 2, Name: "Супы", ParentID: &parent},
		{ID: 3, Name: "Соусы", ParentID: &parent},
		{ID: 4, Name: "Десерты"},
	}
	tree := categoryTree(cats)
	want := []struct {
		name  string
		depth int
	}{{"Рецепты", 0}, {"Супы", 1}, {"Соусы", 1}, {"Десерты", 0}}
	if len(tree) != len(want) {
		t.Fatalf("got %d nodes, want %d", len(tree), len(want))
	}
	for i, w := range want {
		if tree[i].Name != w.name || tree[i].Depth != w.depth {
			t.Fatalf("node %d = (%s,%d), want (%s,%d)", i, tree[i].Name, tree[i].Depth, w.name, w.depth)
		}
	}
}

func TestHomeRendersCategoryHierarchy(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	parent, err := st.GetOrCreateCategory(ctx, "Десерты", models.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateCategoryWithParent(ctx, "Торты", &parent.ID, models.SourceManual); err != nil {
		t.Fatal(err)
	}
	home := getPage(t, newClient(t), ts.URL+"/")
	if !strings.Contains(home, "— Торты") {
		t.Error("child category not rendered indented in the nav")
	}
}

// Scenario 1: logo + navigation present on every page, linking home.
func TestScenarioLogoAndNav(t *testing.T) {
	ts, _ := testServer(t)
	c := newClient(t)
	for _, path := range []string{"/", "/admin/login"} {
		body := getPage(t, c, ts.URL+path)
		if !strings.Contains(body, `class="logo" href="/"`) {
			t.Errorf("%s: logo link to home missing", path)
		}
		if !strings.Contains(body, "logo.svg") {
			t.Errorf("%s: logo image missing", path)
		}
	}
}

// Scenario 2: a guest can search by ingredient and name but sees no edit controls.
func TestScenarioGuestSearch(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	cat, _ := st.GetOrCreateCategory(ctx, "Супы", models.SourceManual)
	if _, err := st.CreateRecipe(ctx, store.RecipeInput{
		Title: "Борщ", CategoryID: cat.ID,
		Ingredients: []models.IngredientBlock{{Items: []string{"свёкла", "капуста"}}},
		StepsHTML:   "<p>Варить.</p>",
	}); err != nil {
		t.Fatal(err)
	}
	c := newClient(t)

	if body := getPage(t, c, ts.URL+"/?q=свёкла"); !strings.Contains(body, "Борщ") {
		t.Error("search by ingredient did not find the recipe")
	}
	if body := getPage(t, c, ts.URL+"/?q=борщ"); !strings.Contains(body, "Борщ") {
		t.Error("search by name did not find the recipe")
	}
	// Guest header shows login, not the add-recipe control.
	home := getPage(t, c, ts.URL+"/")
	if strings.Contains(home, "Добавить рецепт") {
		t.Error("guest should not see the add-recipe button")
	}
	rec, _ := st.ListRecipes(ctx, nil, 0, 0)
	page := getPage(t, c, ts.URL+"/recipes/"+strconv.FormatInt(rec[0].ID, 10))
	if strings.Contains(page, "Редактировать") {
		t.Error("guest should not see the edit button on a recipe")
	}
}

// Scenario 3: a member adds a recipe in a brand-new category, which then appears
// in the catalog filter immediately.
func TestScenarioMemberAddsRecipeAndCategory(t *testing.T) {
	ts, st := testServer(t)
	c := newClient(t)
	login(t, c, ts.URL)

	tok := token(t, c, ts.URL+"/admin/recipes/new")
	resp, err := c.PostForm(ts.URL+"/admin/recipes", url.Values{
		"title":        {"Тирамису"},
		"new_category": {"Десерты"},
		"ing_subtitle": {"Крем"},
		"ing_items":    {"маскарпоне\nсахар"},
		"steps_html":   {"<p>Смешать.</p>"},
		"csrf_token":   {tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status %d", resp.StatusCode)
	}

	// New category appears specifically as a filter chip on the home catalog.
	cat, err := st.CategoryByNorm(context.Background(), store.NormalizeName("Десерты"))
	if err != nil {
		t.Fatalf("new category was not created: %v", err)
	}
	home := getPage(t, c, ts.URL+"/")
	// A logged-in member sees the admin links in the account dropdown on the
	// main page (guests do not — see TestScenarioGuestSearch).
	for _, href := range []string{`href="/admin/recipes"`, `href="/admin/categories"`, `href="/admin/recipes/new"`} {
		if !strings.Contains(home, href) {
			t.Errorf("logged-in main page missing admin link %s", href)
		}
	}
	chip := `data-cat="` + strconv.FormatInt(cat.ID, 10) + `">Десерты</a>`
	if !strings.Contains(home, chip) {
		t.Error("new category not shown as a filter chip on the catalog")
	}
	// The recipe is searchable.
	if body := getPage(t, c, ts.URL+"/?q=тирамису"); !strings.Contains(body, "Тирамису") {
		t.Error("new recipe not found by search")
	}
}
