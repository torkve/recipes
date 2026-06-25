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
	"strconv"
	"strings"
	"testing"

	"recipes/internal/auth"
	"recipes/internal/config"
	"recipes/internal/models"
	"recipes/internal/search"
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
	srv, err := NewServer(cfg, st, keys, nil, search.New(st, nil, 0))
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

func login(t *testing.T, c *http.Client, base string) {
	t.Helper()
	resp, err := c.PostForm(base+"/admin/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
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

func TestCategoryPathAndDescendants(t *testing.T) {
	one, two := int64(1), int64(2)
	cats := []models.Category{
		{ID: 1, Name: "Супы"},
		{ID: 2, Name: "Холодные", ParentID: &one},
		{ID: 3, Name: "Гаспачо", ParentID: &two},
		{ID: 4, Name: "Десерты"},
	}
	path := categoryPath(cats, 3) // root -> leaf
	if len(path) != 3 || path[0].ID != 1 || path[1].ID != 2 || path[2].ID != 3 {
		t.Fatalf("categoryPath(3) = %+v, want [1 2 3]", path)
	}
	ds := categoryDescendantIDs(cats, 1) // 1 + all descendants
	set := map[int64]bool{}
	for _, id := range ds {
		set[id] = true
	}
	if len(ds) != 3 || !set[1] || !set[2] || !set[3] || set[4] {
		t.Fatalf("categoryDescendantIDs(1) = %v, want {1,2,3}", ds)
	}
}

func TestHomeParentFilterIncludesSubcategories(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	sup, err := st.CreateCategoryWithParent(ctx, "СупыТест", nil, models.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	hot, err := st.CreateCategoryWithParent(ctx, "ГорячиеТест", &sup.ID, models.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRecipe(ctx, store.RecipeInput{
		Title: "Борщ субдерево", CategoryID: hot.ID,
		Ingredients: []models.IngredientBlock{{Items: []string{"свёкла"}}},
		StepsHTML:   "<p>Варить.</p>",
	}); err != nil {
		t.Fatal(err)
	}
	// Filtering by the PARENT must surface the recipe filed under its child.
	body := getPage(t, newClient(t), ts.URL+"/?cat="+strconv.FormatInt(sup.ID, 10))
	if !strings.Contains(body, "Борщ субдерево") {
		t.Error("parent filter did not include subcategory recipe")
	}
}

// Characterization of the home search/browse contract that the (upcoming) search
// service must preserve: a query filters to matching recipes; an empty query
// browses everything.
func TestHomeSearchDiscriminatesAndBrowses(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	cat, _ := st.GetOrCreateCategory(ctx, "Разное", models.SourceManual)
	for _, title := range []string{"Борщ московский", "Тирамису классический"} {
		if _, err := st.CreateRecipe(ctx, store.RecipeInput{
			Title: title, CategoryID: cat.ID,
			Ingredients: []models.IngredientBlock{{Items: []string{"что-то"}}},
			StepsHTML:   "<p>Готовить.</p>",
		}); err != nil {
			t.Fatal(err)
		}
	}
	c := newClient(t)
	// Query filters: "борщ" finds only the borscht.
	hit := getPage(t, c, ts.URL+"/?q=борщ")
	if !strings.Contains(hit, "Борщ московский") || strings.Contains(hit, "Тирамису классический") {
		t.Errorf("search did not discriminate: %q", hit)
	}
	// Empty query browses: both recipes listed.
	all := getPage(t, c, ts.URL+"/")
	if !strings.Contains(all, "Борщ московский") || !strings.Contains(all, "Тирамису классический") {
		t.Error("empty-query browse did not list all recipes")
	}
}

func TestCategoryTreeTerminatesOnCycle(t *testing.T) {
	// categoryTree runs on every home/admin/recipe-form render, so a corrupt
	// parent cycle in the data must never make it loop forever or emit a node
	// twice. A normal root+child still renders; the cyclic pair is dropped
	// (unreachable from the top level) rather than recursed into.
	a, b := int64(10), int64(11)
	cats := []models.Category{
		{ID: 1, Name: "Корень"},
		{ID: 2, Name: "Ребёнок", ParentID: &a /* will be remapped below */},
		{ID: 10, Name: "A", ParentID: &b},
		{ID: 11, Name: "B", ParentID: &a},
	}
	cats[1].ParentID = &cats[0].ID // child of the real root
	tree := categoryTree(cats)

	seen := map[int64]bool{}
	for _, n := range tree {
		if seen[n.ID] {
			t.Fatalf("category %d emitted more than once", n.ID)
		}
		seen[n.ID] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("acyclic root/child not rendered: %+v", tree)
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
	// The child renders in the dropdown, indented by depth via the --d CSS var
	// (real indentation, no space padding).
	if !strings.Contains(home, `style="--d:1"`) || !strings.Contains(home, ">Торты</a>") {
		t.Errorf("child category not rendered indented in the dropdown")
	}
}

func TestAdminSetCategoryParent(t *testing.T) {
	ts, st := testServer(t)
	ctx := context.Background()
	a, _ := st.CreateCategoryWithParent(ctx, "A", nil, models.SourceManual)
	b, _ := st.CreateCategoryWithParent(ctx, "B", nil, models.SourceManual)

	c := newClient(t)
	login(t, c, ts.URL)

	post := func(id int64, parent string) *http.Response {
		resp, err := c.PostForm(ts.URL+"/admin/categories/"+strconv.FormatInt(id, 10)+"/parent",
			url.Values{"parent_id": {parent}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// Set B under A.
	if resp := post(b.ID, strconv.FormatInt(a.ID, 10)); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set parent status %d", resp.StatusCode)
	}
	if got, _ := st.GetCategory(ctx, b.ID); got.ParentID == nil || *got.ParentID != a.ID {
		t.Fatalf("B parent = %v, want %d", got.ParentID, a.ID)
	}

	// A under B would create a cycle: rejected with the cycle flash, unchanged.
	resp := post(a.ID, strconv.FormatInt(b.ID, 10))
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "msg=cycle") {
		t.Fatalf("cycle attempt: status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got, _ := st.GetCategory(ctx, a.ID); got.ParentID != nil {
		t.Fatalf("A parent changed despite cycle: %v", got.ParentID)
	}

	// Clearing B's parent moves it back to the top level.
	post(b.ID, "")
	if got, _ := st.GetCategory(ctx, b.ID); got.ParentID != nil {
		t.Fatalf("B parent not cleared: %v", got.ParentID)
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
// TestCrossOriginProtection proves the CSRF defense: an authenticated
// state-changing POST is denied when it looks cross-origin (Sec-Fetch-Site:
// cross-site) and allowed when same-origin.
func TestCrossOriginProtection(t *testing.T) {
	ts, st := testServer(t)
	cat, err := st.GetOrCreateCategory(context.Background(), "Супы", models.SourceManual)
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t)
	login(t, c, ts.URL) // c's jar now holds the session cookie

	target := ts.URL + "/admin/categories/" + strconv.FormatInt(cat.ID, 10) + "/rename"
	doPost := func(secFetchSite string) int {
		req, _ := http.NewRequest(http.MethodPost, target,
			strings.NewReader(url.Values{"name": {"Первые блюда"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", secFetchSite)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// A forged cross-origin request (even with the victim's cookie) is blocked
	// before the handler runs.
	if got := doPost("cross-site"); got != http.StatusForbidden {
		t.Fatalf("cross-site POST: got %d, want 403", got)
	}
	// The app's own same-origin form succeeds (303 redirect from the handler).
	if got := doPost("same-origin"); got != http.StatusSeeOther {
		t.Fatalf("same-origin POST: got %d, want 303", got)
	}
}

func TestScenarioMemberAddsRecipeAndCategory(t *testing.T) {
	ts, st := testServer(t)
	c := newClient(t)
	login(t, c, ts.URL)

	resp, err := c.PostForm(ts.URL+"/admin/recipes", url.Values{
		"title":        {"Тирамису"},
		"new_category": {"Десерты"},
		"ing_subtitle": {"Крем"},
		"ing_items":    {"маскарпоне\nсахар"},
		"steps_html":   {"<p>Смешать.</p>"},
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
	if !strings.Contains(home, `data-cat="`+strconv.FormatInt(cat.ID, 10)+`"`) ||
		!strings.Contains(home, ">Десерты</a>") {
		t.Error("new category not shown in the catalog filter dropdown")
	}
	// The recipe is searchable.
	if body := getPage(t, c, ts.URL+"/?q=тирамису"); !strings.Contains(body, "Тирамису") {
		t.Error("new recipe not found by search")
	}
}
