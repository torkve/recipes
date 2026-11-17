package cookbook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recipes/internal/models"
	"recipes/internal/store"
)

func newTestService(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	uploads := filepath.Join(dir, "uploads")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	return New(st, uploads), st, uploads
}

// upload creates an image file in the uploads dir and returns its <img> tag.
func upload(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	return `<img src="/uploads/` + name + `" alt="">`
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

const (
	imgA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.png"
	imgB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb.jpg"
)

func TestCreateValidatesAndSanitizes(t *testing.T) {
	svc, st, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, Draft{Title: "  ", Category: CategoryRef{Name: "Супы"}}); !errors.Is(err, ErrTitleRequired) {
		t.Fatalf("blank title: err = %v, want ErrTitleRequired", err)
	}
	if _, err := svc.Create(ctx, Draft{Title: "Борщ"}); !errors.Is(err, ErrCategoryRequired) {
		t.Fatalf("no category: err = %v, want ErrCategoryRequired", err)
	}
	if _, err := svc.Create(ctx, Draft{Title: "Борщ", Category: CategoryRef{ID: 9999}}); !errors.Is(err, ErrCategoryRequired) {
		t.Fatalf("unknown category id: err = %v, want ErrCategoryRequired", err)
	}

	u, err := st.CreateUser(ctx, "cook", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	owner := u.ID
	rec, err := svc.Create(ctx, Draft{
		Title:     " Борщ ",
		Category:  CategoryRef{Name: "Новая категория"},
		StepsHTML: `<p>Варить</p><script>alert(1)</script>`,
		OwnerID:   &owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Title != "Борщ" {
		t.Errorf("title = %q, want trimmed", rec.Title)
	}
	if strings.Contains(rec.StepsHTML, "script") {
		t.Errorf("steps not sanitized: %q", rec.StepsHTML)
	}
	if rec.Category == nil || rec.Category.Name != "Новая категория" {
		t.Errorf("category = %+v, want created by name", rec.Category)
	}
	if rec.OwnerID == nil || *rec.OwnerID != owner {
		t.Errorf("owner = %v, want %d", rec.OwnerID, owner)
	}
	cats, _ := st.ListCategories(ctx)
	found := false
	for _, c := range cats {
		found = found || c.Name == "Новая категория"
	}
	if !found {
		t.Error("category by name was not created")
	}
}

func TestUpdatePatchKeepsUnsetFieldsAndLinkage(t *testing.T) {
	svc, st, _ := newTestService(t)
	ctx := context.Background()
	rec, err := svc.Create(ctx, Draft{
		Title:       "Борщ",
		Category:    CategoryRef{Name: "Супы"},
		Ingredients: []models.IngredientBlock{{Items: []string{"свёкла"}}},
		StepsHTML:   "<p>Варить</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate iCloud linkage set by the sync engine.
	note, etag := "note-1", "etag-1"
	if err := st.UpdateRecipe(ctx, rec.ID, store.RecipeInput{
		Title: rec.Title, CategoryID: rec.CategoryID, Ingredients: rec.Ingredients,
		StepsHTML: rec.StepsHTML, ICloudNoteID: &note, ICloudEtag: &etag,
	}); err != nil {
		t.Fatal(err)
	}

	title := "Борщ с пампушками"
	got, err := svc.Update(ctx, rec.ID, Patch{Title: &title}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != title {
		t.Errorf("title = %q, want %q", got.Title, title)
	}
	if got.StepsHTML != rec.StepsHTML || len(got.Ingredients) != 1 || got.CategoryID != rec.CategoryID {
		t.Errorf("unset fields changed: %+v", got)
	}
	if got.ICloudNoteID == nil || *got.ICloudNoteID != note || got.ICloudEtag == nil || *got.ICloudEtag != etag {
		t.Errorf("iCloud linkage lost: note=%v etag=%v", got.ICloudNoteID, got.ICloudEtag)
	}
	if got.Revision != rec.Revision+1 {
		t.Errorf("revision = %d, want %d", got.Revision, rec.Revision+1)
	}

	stale := rec.Revision
	if _, err := svc.Update(ctx, rec.ID, Patch{Title: &title}, &stale); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revision: err = %v, want ErrConflict", err)
	}
	if _, err := svc.Update(ctx, 9999, Patch{Title: &title}, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing recipe: err = %v, want ErrNotFound", err)
	}
}

func TestImagesOwnershipAndCleanup(t *testing.T) {
	svc, st, dir := newTestService(t)
	ctx := context.Background()

	tagA := upload(t, dir, imgA)
	first, err := svc.Create(ctx, Draft{Title: "Первый", Category: CategoryRef{Name: "Супы"}, StepsHTML: "<p>x</p>" + tagA})
	if err != nil {
		t.Fatalf("fresh upload must be accepted: %v", err)
	}
	if len(first.Images) != 1 || first.Images[0].Filename != imgA || first.Images[0].ContentType != "image/png" {
		t.Fatalf("images = %+v, want [%s image/png]", first.Images, imgA)
	}

	// Another recipe may not reference the first one's image, on create or update.
	if _, err := svc.Create(ctx, Draft{Title: "Второй", Category: CategoryRef{Name: "Супы"}, StepsHTML: tagA}); !errors.Is(err, ErrForeignImage) {
		t.Fatalf("create with foreign image: err = %v, want ErrForeignImage", err)
	}
	second, err := svc.Create(ctx, Draft{Title: "Второй", Category: CategoryRef{Name: "Супы"}, StepsHTML: "<p>y</p>"})
	if err != nil {
		t.Fatal(err)
	}
	steps := "<p>y</p>" + tagA
	if _, err := svc.Update(ctx, second.ID, Patch{StepsHTML: &steps}, nil); !errors.Is(err, ErrForeignImage) {
		t.Fatalf("update with foreign image: err = %v, want ErrForeignImage", err)
	}

	// The owner may keep its own image across edits.
	keep := "<p>x2</p>" + tagA
	if _, err := svc.Update(ctx, first.ID, Patch{StepsHTML: &keep}, nil); err != nil {
		t.Fatalf("owner re-saving its image: %v", err)
	}
	if !exists(dir, imgA) {
		t.Fatal("kept image file was removed")
	}

	// Swapping the image detaches and deletes the old file.
	tagB := upload(t, dir, imgB)
	swap := "<p>x3</p>" + tagB
	got, err := svc.Update(ctx, first.ID, Patch{StepsHTML: &swap}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exists(dir, imgA) {
		t.Error("unreferenced image file was not removed")
	}
	if len(got.Images) != 1 || got.Images[0].Filename != imgB {
		t.Errorf("images after swap = %+v, want [%s]", got.Images, imgB)
	}

	// Delete removes the recipe and its files.
	if err := svc.Delete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if exists(dir, imgB) {
		t.Error("image file survived recipe deletion")
	}
	if _, err := st.GetRecipe(ctx, first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("recipe still present: %v", err)
	}
	if err := svc.Delete(ctx, first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

// TestSharedLegacyImageIsForeignToBoth pins that an upload attached to two
// recipes is rejected when saving either of them.
func TestSharedLegacyImageIsForeignToBoth(t *testing.T) {
	svc, st, dir := newTestService(t)
	ctx := context.Background()
	tag := upload(t, dir, imgA)
	var ids []int64
	for _, title := range []string{"Первый", "Второй"} {
		rec, err := st.CreateRecipe(ctx, store.RecipeInput{Title: title, CategoryID: 1, StepsHTML: tag})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddImage(ctx, rec.ID, imgA, "image/png"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
	}
	for _, id := range ids {
		if _, err := svc.Update(ctx, id, Patch{StepsHTML: &tag}, nil); !errors.Is(err, ErrForeignImage) {
			t.Errorf("recipe %d: err = %v, want ErrForeignImage", id, err)
		}
	}
}

func TestMissingNewImageIsRejected(t *testing.T) {
	svc, _, dir := newTestService(t)
	ctx := context.Background()
	tag := upload(t, dir, imgA)
	rec, err := svc.Create(ctx, Draft{Title: "A", Category: CategoryRef{Name: "Супы"}, StepsHTML: tag})
	if err != nil {
		t.Fatal(err)
	}
	missing := `<img src="/uploads/` + imgB + `" alt="">`
	if _, err := svc.Create(ctx, Draft{Title: "B", Category: CategoryRef{Name: "Супы"}, StepsHTML: missing}); !errors.Is(err, ErrMissingImage) {
		t.Fatalf("create with a missing upload: err = %v", err)
	}
	// An already-attached image whose file vanished does not block edits.
	if err := os.Remove(filepath.Join(dir, imgA)); err != nil {
		t.Fatal(err)
	}
	title := "A2"
	if _, err := svc.Update(ctx, rec.ID, Patch{Title: &title}, nil); err != nil {
		t.Fatalf("editing a recipe with a vanished attached image: %v", err)
	}
	steps := tag + missing
	if _, err := svc.Update(ctx, rec.ID, Patch{StepsHTML: &steps}, nil); !errors.Is(err, ErrMissingImage) {
		t.Fatalf("update adding a missing upload: err = %v", err)
	}
}

func TestSweepOrphanUploads(t *testing.T) {
	svc, _, dir := newTestService(t)
	ctx := context.Background()
	tag := upload(t, dir, imgA)
	if _, err := svc.Create(ctx, Draft{Title: "С картинкой", Category: CategoryRef{Name: "Супы"}, StepsHTML: tag}); err != nil {
		t.Fatal(err)
	}
	upload(t, dir, imgB) // unattached
	const fresh = "cccccccc-cccc-4ccc-8ccc-cccccccccccc.png"
	upload(t, dir, fresh)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, n := range []string{imgA, imgB, "README"} {
		if err := os.Chtimes(filepath.Join(dir, n), old, old); err != nil {
			t.Fatal(err)
		}
	}

	n, err := svc.SweepOrphanUploads(ctx, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("SweepOrphanUploads = %d, %v; want 1", n, err)
	}
	if exists(dir, imgB) {
		t.Error("old unattached upload survived")
	}
	for _, keep := range []string{imgA, fresh} {
		if !exists(dir, keep) {
			t.Errorf("%s was swept", keep)
		}
	}
}
