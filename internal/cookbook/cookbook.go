// Package cookbook holds the recipe write path shared by every front end (the
// web admin and the MCP server): validation, sanitizing, category resolution,
// image bookkeeping and file cleanup.
package cookbook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"recipes/internal/models"
	"recipes/internal/sanitize"
	"recipes/internal/store"
)

var (
	// ErrTitleRequired is returned when a recipe would have an empty title.
	ErrTitleRequired = errors.New("cookbook: title required")
	// ErrCategoryRequired is returned when no category is given or the given
	// category id does not exist.
	ErrCategoryRequired = errors.New("cookbook: category required")
	// ErrForeignImage is returned when the steps reference an uploaded image that
	// belongs to a different recipe.
	ErrForeignImage = errors.New("cookbook: image belongs to another recipe")
)

// CategoryRef selects a category: by Name (created when missing; takes
// precedence when non-blank) or by ID (must exist).
type CategoryRef struct {
	ID   int64
	Name string
}

// Draft is a new recipe. StepsHTML is untrusted; it is sanitized on save.
type Draft struct {
	Title       string
	Category    CategoryRef
	Ingredients []models.IngredientBlock
	StepsHTML   string
	OwnerID     *int64
}

// Patch changes selected fields of a recipe; nil fields are left as they are.
// StepsHTML is untrusted; it is sanitized on save.
type Patch struct {
	Title       *string
	Category    *CategoryRef
	Ingredients *[]models.IngredientBlock
	StepsHTML   *string
}

// Service implements recipe writes on top of the store.
type Service struct {
	store      *store.Store
	uploadsDir string
}

// New returns a Service storing image files under uploadsDir.
func New(st *store.Store, uploadsDir string) *Service {
	return &Service{store: st, uploadsDir: uploadsDir}
}

// Create validates and stores a new recipe, attaching the uploads its steps
// reference.
func (s *Service) Create(ctx context.Context, d Draft) (*models.Recipe, error) {
	in, err := s.prepare(ctx, 0, d.Title, d.Category, d.Ingredients, d.StepsHTML)
	if err != nil {
		return nil, err
	}
	in.OwnerID = d.OwnerID
	rec, err := s.store.CreateRecipe(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := s.reconcileImages(ctx, rec.ID, in.StepsHTML); err != nil {
		return nil, err
	}
	return s.store.GetRecipe(ctx, rec.ID)
}

// Update applies p to recipe id, keeping its iCloud linkage and owner. With
// expectedRevision set it fails with store.ErrConflict when the recipe changed
// since that revision. Returns store.ErrNotFound for a missing recipe.
func (s *Service) Update(ctx context.Context, id int64, p Patch, expectedRevision *int64) (*models.Recipe, error) {
	cur, err := s.store.GetRecipe(ctx, id)
	if err != nil {
		return nil, err
	}
	if expectedRevision != nil && *expectedRevision != cur.Revision {
		return nil, store.ErrConflict
	}
	title, cat, ings, steps := cur.Title, CategoryRef{ID: cur.CategoryID}, cur.Ingredients, cur.StepsHTML
	if p.Title != nil {
		title = *p.Title
	}
	if p.Category != nil {
		cat = *p.Category
	}
	if p.Ingredients != nil {
		ings = *p.Ingredients
	}
	if p.StepsHTML != nil {
		steps = *p.StepsHTML
	}
	in, err := s.prepare(ctx, id, title, cat, ings, steps)
	if err != nil {
		return nil, err
	}
	in.ICloudNoteID = cur.ICloudNoteID
	in.ICloudEtag = cur.ICloudEtag
	in.OwnerID = cur.OwnerID
	// The store re-checks the revision inside its write transaction, catching an
	// edit that lands between GetRecipe above and this write.
	rev := cur.Revision
	in.ExpectedRevision = &rev
	if err := s.store.UpdateRecipe(ctx, id, in); err != nil {
		return nil, err
	}
	if err := s.reconcileImages(ctx, id, in.StepsHTML); err != nil {
		return nil, err
	}
	return s.store.GetRecipe(ctx, id)
}

// Delete removes a recipe and its image files. Returns store.ErrNotFound for a
// missing recipe.
func (s *Service) Delete(ctx context.Context, id int64) error {
	files, err := s.store.DeleteRecipe(ctx, id)
	if err != nil {
		return err
	}
	for _, f := range files {
		s.RemoveUpload(f)
	}
	return nil
}

// RemoveUpload deletes an uploaded file, guarding against path traversal.
func (s *Service) RemoveUpload(name string) {
	if !sanitize.IsValidUploadName(name) {
		return
	}
	_ = os.Remove(filepath.Join(s.uploadsDir, name))
}

// prepare validates and normalizes recipe fields into a store input. selfID is
// the recipe being edited (0 for a new one), used for the image ownership check.
func (s *Service) prepare(ctx context.Context, selfID int64, title string, cat CategoryRef, ings []models.IngredientBlock, stepsHTML string) (store.RecipeInput, error) {
	in := store.RecipeInput{
		Title:       strings.TrimSpace(title),
		Ingredients: ings,
		StepsHTML:   sanitize.StepsHTML(stepsHTML),
	}
	if in.Title == "" {
		return in, ErrTitleRequired
	}
	if err := s.checkImageOwnership(ctx, selfID, in.StepsHTML); err != nil {
		return in, err
	}
	catID, err := s.resolveCategory(ctx, cat)
	if err != nil {
		return in, err
	}
	in.CategoryID = catID
	return in, nil
}

func (s *Service) resolveCategory(ctx context.Context, ref CategoryRef) (int64, error) {
	if name := strings.TrimSpace(ref.Name); name != "" {
		c, err := s.store.GetOrCreateCategory(ctx, name, models.SourceManual)
		if err != nil {
			return 0, err
		}
		return c.ID, nil
	}
	if ref.ID <= 0 {
		return 0, ErrCategoryRequired
	}
	if _, err := s.store.GetCategory(ctx, ref.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, ErrCategoryRequired
		}
		return 0, err
	}
	return ref.ID, nil
}

// checkImageOwnership returns ErrForeignImage when the steps reference an upload
// attached to a recipe other than selfID. Unattached uploads (fresh pastes) pass.
func (s *Service) checkImageOwnership(ctx context.Context, selfID int64, stepsHTML string) error {
	foreign, err := s.store.ImagesAttachedElsewhere(ctx, selfID, sanitize.ImageFilenames(stepsHTML))
	if err != nil {
		return err
	}
	if len(foreign) > 0 {
		return fmt.Errorf("%w: %s", ErrForeignImage, strings.Join(foreign, ", "))
	}
	return nil
}

// typeByExt maps an upload's extension to its content type.
var typeByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// reconcileImages makes recipe_images match the images referenced in the saved
// steps HTML: it records referenced uploads missing from recipe_images and
// deletes the rows and files of attached uploads the steps do not reference.
func (s *Service) reconcileImages(ctx context.Context, recipeID int64, html string) error {
	referenced := sanitize.ImageFilenames(html)
	refSet := make(map[string]bool, len(referenced))
	for _, n := range referenced {
		refSet[n] = true
	}

	existing, err := s.store.ImagesForRecipe(ctx, recipeID)
	if err != nil {
		return err
	}
	existingSet := make(map[string]bool, len(existing))
	for _, img := range existing {
		existingSet[img.Filename] = true
		if !refSet[img.Filename] {
			if err := s.store.DeleteImageByName(ctx, recipeID, img.Filename); err != nil {
				return err
			}
			s.RemoveUpload(img.Filename)
		}
	}
	for _, name := range referenced {
		if !existingSet[name] {
			existingSet[name] = true // a name referenced twice is recorded once
			ct := typeByExt[strings.ToLower(filepath.Ext(name))]
			if _, err := s.store.AddImage(ctx, recipeID, name, ct); err != nil {
				return err
			}
		}
	}
	return nil
}
