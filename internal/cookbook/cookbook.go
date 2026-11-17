// Package cookbook holds the recipe write path shared by every front end (the
// web admin and the MCP server): validation, sanitizing, category resolution,
// image bookkeeping and file cleanup.
package cookbook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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
	ErrForeignImage = store.ErrForeignImage
	// ErrMissingImage is returned when the steps newly reference an upload
	// that has no file in the uploads directory.
	ErrMissingImage = errors.New("cookbook: image file no longer exists")
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
// reference in the same transaction.
func (s *Service) Create(ctx context.Context, d Draft) (*models.Recipe, error) {
	in, err := s.prepare(ctx, d.Title, d.Category, d.Ingredients, d.StepsHTML)
	if err != nil {
		return nil, err
	}
	in.OwnerID = d.OwnerID
	images := referencedImages(in.StepsHTML)
	uploadsMu.RLock()
	defer uploadsMu.RUnlock()
	if err := s.checkNewImagesExist(images, nil); err != nil {
		return nil, err
	}
	return s.store.CreateRecipeWithImages(ctx, in, images)
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
	in, err := s.prepare(ctx, title, cat, ings, steps)
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
	images := referencedImages(in.StepsHTML)
	uploadsMu.RLock()
	defer uploadsMu.RUnlock()
	if err := s.checkNewImagesExist(images, cur.Images); err != nil {
		return nil, err
	}
	detached, err := s.store.UpdateRecipeWithImages(ctx, id, in, images)
	if err != nil {
		return nil, err
	}
	for _, name := range detached {
		s.RemoveUpload(name)
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

// prepare validates and normalizes recipe fields into a store input.
func (s *Service) prepare(ctx context.Context, title string, cat CategoryRef, ings []models.IngredientBlock, stepsHTML string) (store.RecipeInput, error) {
	in := store.RecipeInput{
		Title:       strings.TrimSpace(title),
		Ingredients: ings,
		StepsHTML:   sanitize.StepsHTML(stepsHTML),
	}
	if in.Title == "" {
		return in, ErrTitleRequired
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

// MaxUploadBytes is the size limit of one uploaded image.
const MaxUploadBytes = 8 << 20

// ErrUnsupportedImage is returned by SaveUpload for data that is not a PNG,
// JPEG, GIF or WebP image, or is larger than MaxUploadBytes.
var ErrUnsupportedImage = errors.New("cookbook: not a supported image (png, jpeg, gif, webp up to 8 MiB)")

// extByType maps a sniffed image content type to its upload file extension.
var extByType = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// SaveUpload stores an image as a new unattached upload and returns its file
// name; steps reference it as /uploads/<name>, which attaches it to the recipe
// on save. The type is sniffed from the content, not taken from the caller.
func (s *Service) SaveUpload(data []byte) (string, error) {
	if len(data) == 0 || len(data) > MaxUploadBytes {
		return "", ErrUnsupportedImage
	}
	ext, ok := extByType[http.DetectContentType(data)]
	if !ok {
		return "", ErrUnsupportedImage
	}
	name := uuid.NewString() + ext
	dst := filepath.Join(s.uploadsDir, name)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return name, nil
}

// typeByExt maps an upload's extension to its content type.
var typeByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// referencedImages returns the uploads referenced by sanitized steps HTML, as
// image rows to attach (never nil, so an empty result detaches every image).
func referencedImages(html string) []models.RecipeImage {
	names := sanitize.ImageFilenames(html)
	out := make([]models.RecipeImage, 0, len(names))
	for _, name := range names {
		out = append(out, models.RecipeImage{Filename: name, ContentType: typeByExt[strings.ToLower(filepath.Ext(name))]})
	}
	return out
}

// uploadsMu orders recipe saves against the orphan sweep within the process:
// saves hold it shared from checking their new images' files to committing,
// the sweep holds it exclusively, so the sweep never deletes a file that a
// save is attaching. It is package-level because every Service instance
// shares the one uploads directory.
var uploadsMu sync.RWMutex

// checkNewImagesExist returns ErrMissingImage when an image in images that is
// not already in attached has no file. The caller holds uploadsMu.
func (s *Service) checkNewImagesExist(images, attached []models.RecipeImage) error {
	have := make(map[string]bool, len(attached))
	for _, img := range attached {
		have[img.Filename] = true
	}
	for _, img := range images {
		if have[img.Filename] {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.uploadsDir, img.Filename)); err != nil {
			return fmt.Errorf("%w: %s", ErrMissingImage, img.Filename)
		}
	}
	return nil
}

// isGeneratedUpload reports whether name has the shape of an upload this
// service stores: a UUID followed by a supported image extension.
func isGeneratedUpload(name string) bool {
	ext := filepath.Ext(name)
	if _, ok := typeByExt[ext]; !ok {
		return false
	}
	_, err := uuid.Parse(strings.TrimSuffix(name, ext))
	return err == nil
}

// SweepOrphanUploads deletes uploaded images (UUID-named image files in the
// uploads directory) that no recipe references and that were last modified
// before now minus minAge, returning how many it deleted. minAge must leave
// time for a fresh upload to be saved into a recipe.
func (s *Service) SweepOrphanUploads(ctx context.Context, minAge time.Duration) (int, error) {
	uploadsMu.Lock()
	defer uploadsMu.Unlock()
	attached, err := s.store.AllImageFilenames(ctx)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(s.uploadsDir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-minAge)
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || attached[name] || !isGeneratedUpload(name) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.uploadsDir, name)); err == nil {
			n++
		}
	}
	return n, nil
}
