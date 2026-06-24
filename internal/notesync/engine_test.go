package notesync

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"recipes/internal/models"
	"recipes/internal/store"
)

// fakeProvider implements SyncProvider and Binder from in-memory state.
type fakeProvider struct {
	folders   []Folder
	notes     []Note
	pushed    []Note
	pushCount int
}

type fakeSession struct{ data []byte }

func (s fakeSession) Bytes() ([]byte, error) { return s.data, nil }
func (s fakeSession) Expired() bool          { return false }

func (p *fakeProvider) Begin(ctx context.Context, appleID, password string) (BindResult, error) {
	return BindResult{Session: fakeSession{[]byte("sess")}, Pending: false}, nil
}
func (p *fakeProvider) Complete(ctx context.Context, h BindHandle, code string) (Session, error) {
	return fakeSession{[]byte("sess")}, nil
}
func (p *fakeProvider) Restore(ctx context.Context, blob []byte) (Session, error) {
	return fakeSession{blob}, nil
}
func (p *fakeProvider) ListFolders(ctx context.Context, sess Session, root FolderID) ([]Folder, error) {
	return p.folders, nil
}
func (p *fakeProvider) ChangedNotes(ctx context.Context, sess Session, root FolderID, since string) ([]Note, string, error) {
	return p.notes, "", nil
}
func (p *fakeProvider) PushNote(ctx context.Context, sess Session, n Note, expected Etag) (Note, error) {
	p.pushCount++
	if n.ID == "" {
		n.ID = NoteID(fmt.Sprintf("note-%d", p.pushCount))
	}
	n.Etag = Etag(fmt.Sprintf("etag-%s-%d", n.ID, p.pushCount))
	p.pushed = append(p.pushed, n)
	return n, nil
}
func (p *fakeProvider) EnsureFolder(ctx context.Context, sess Session, parent FolderID, name string) (Folder, error) {
	return Folder{ID: FolderID("f-" + name), ParentID: parent, Name: name}, nil
}

func newTestEngine(t *testing.T) (*Engine, *store.Store, *fakeProvider, int64) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	user, err := st.CreateUser(context.Background(), "admin", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{}
	key := make([]byte, 32)
	eng, err := NewEngine(st, fp, fp, key, dir)
	if err != nil {
		t.Fatal(err)
	}
	return eng, st, fp, user.ID
}

func TestPullCreatesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)

	if _, _, err := eng.BeginBind(ctx, uid, "a@b.com", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetFolder(ctx, uid, "root"); err != nil {
		t.Fatal(err)
	}
	fp.folders = []Folder{{ID: "f1", ParentID: "", Name: "Десерты"}}
	fp.notes = []Note{{ID: "n1", FolderID: "f1", Etag: "e1", Title: "Шарлотка",
		Checklists: [][]string{{"яблоки", "мука"}}, BodyHTML: "Печь 30 минут."}}

	rep, err := eng.PullUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 1 {
		t.Fatalf("first pull: created=%d want 1 (%+v)", rep.Created, rep)
	}
	// Category created from the folder.
	if _, err := st.CategoryByNorm(ctx, store.NormalizeName("Десерты")); err != nil {
		t.Fatalf("category Десерты not created: %v", err)
	}
	rec, err := st.GetRecipeByNoteID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Title != "Шарлотка" || len(rec.Ingredients) != 1 {
		t.Fatalf("bad imported recipe: %+v", rec)
	}

	// Second pull with no changes is a no-op.
	rep, _ = eng.PullUser(ctx, uid)
	if rep.Created != 0 || rep.Updated != 0 || rep.Conflicts != 0 {
		t.Fatalf("idempotent pull changed something: %+v", rep)
	}
}

func TestPullAppliesRemoteChange(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)
	fp.folders = []Folder{{ID: "f1", Name: "Супы"}}
	fp.notes = []Note{{ID: "n1", FolderID: "f1", Etag: "e1", Title: "Борщ",
		Checklists: [][]string{{"свёкла"}}, BodyHTML: "Варить."}}
	if _, err := eng.PullUser(ctx, uid); err != nil {
		t.Fatal(err)
	}

	// Remote content change.
	fp.notes[0].Etag = "e2"
	fp.notes[0].BodyHTML = "Варить два часа."
	rep, err := eng.PullUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 1 {
		t.Fatalf("expected 1 update, got %+v", rep)
	}
	rec, _ := st.GetRecipeByNoteID(ctx, "n1")
	if got := store.PlainTextHTML(rec.StepsHTML); got != "Варить два часа." {
		t.Fatalf("recipe not updated from note: %q", got)
	}
}

func TestPullConflictWhenBothChange(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)
	fp.folders = []Folder{{ID: "f1", Name: "Супы"}}
	fp.notes = []Note{{ID: "n1", FolderID: "f1", Etag: "e1", Title: "Борщ",
		Checklists: [][]string{{"свёкла"}}, BodyHTML: "Варить."}}
	if _, err := eng.PullUser(ctx, uid); err != nil {
		t.Fatal(err)
	}

	// Local edit.
	rec, _ := st.GetRecipeByNoteID(ctx, "n1")
	noteID, etag := "n1", "e1"
	if err := st.UpdateRecipe(ctx, rec.ID, store.RecipeInput{
		Title: "Борщ московский", CategoryID: rec.CategoryID,
		Ingredients: rec.Ingredients, StepsHTML: rec.StepsHTML,
		ICloudNoteID: &noteID, ICloudEtag: &etag,
	}); err != nil {
		t.Fatal(err)
	}
	// Remote edit too.
	fp.notes[0].Etag = "e2"
	fp.notes[0].BodyHTML = "Совсем другой текст."

	rep, err := eng.PullUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Conflicts != 1 {
		t.Fatalf("expected 1 conflict, got %+v", rep)
	}
	conflicts, _ := eng.Conflicts(ctx)
	if len(conflicts) != 1 || conflicts[0].Kind != models.ConflictBothChanged {
		t.Fatalf("expected a both_changed conflict, got %+v", conflicts)
	}
}

func TestPushCreatesNoteForNewRecipe(t *testing.T) {
	ctx := context.Background()
	eng, st, fp, uid := newTestEngine(t)
	mustBind(t, eng, uid)

	cat, _ := st.GetOrCreateCategory(ctx, "Салаты", models.SourceManual)
	if _, err := st.CreateRecipe(ctx, store.RecipeInput{
		Title: "Винегрет", CategoryID: cat.ID,
		Ingredients: []models.IngredientBlock{{Items: []string{"свёкла", "морковь"}}},
		StepsHTML:   "<p>Нарезать.</p>", OwnerID: &uid,
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := eng.PushUser(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 1 {
		t.Fatalf("expected 1 created note, got %+v", rep)
	}
	if len(fp.pushed) != 1 || fp.pushed[0].Title != "Винегрет" {
		t.Fatalf("note not pushed correctly: %+v", fp.pushed)
	}
	// Recipe is now linked; a second push with no local change is a no-op.
	rep, _ = eng.PushUser(ctx, uid)
	if rep.Created != 0 || rep.Updated != 0 {
		t.Fatalf("second push should be no-op, got %+v", rep)
	}
}

func mustBind(t *testing.T, eng *Engine, uid int64) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := eng.BeginBind(ctx, uid, "a@b.com", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetFolder(ctx, uid, "root"); err != nil {
		t.Fatal(err)
	}
}
