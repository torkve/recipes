package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"recipes/internal/models"
	"recipes/internal/sanitize"
	"recipes/internal/store"
)

const maxUploadBytes = 8 << 20 // 8 MiB per pasted image

// extByType maps a sniffed image content type to a file extension. Only these
// image types are accepted for upload.
var extByType = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// typeByExt is the reverse lookup used when recording reconciled images.
var typeByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// handleUpload stores a pasted image and returns its public URL as JSON.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+512)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		http.Error(w, "файл слишком большой", http.StatusRequestEntityTooLarge)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "нет файла", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Sniff the content type from the first 512 bytes.
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	ct := http.DetectContentType(head)
	ext, ok := extByType[ct]
	if !ok {
		http.Error(w, "неподдерживаемый тип изображения", http.StatusUnsupportedMediaType)
		return
	}

	name := uuid.NewString() + ext
	dst := filepath.Join(s.cfg.UploadsDir(), name)
	out, err := os.Create(dst)
	if err != nil {
		http.Error(w, "ошибка сохранения", http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, io.MultiReader(bytes.NewReader(head), file)); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		http.Error(w, "ошибка сохранения", http.StatusInternalServerError)
		return
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		http.Error(w, "ошибка сохранения", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"url":      "/uploads/" + name,
		"filename": name,
	})
}

// categoryMessages maps a ?msg=<code> query value to a flash message + CSS class.
var categoryMessages = map[string][2]string{
	"renamed": {"Категория переименована", "notice"},
	"deleted": {"Категория удалена", "notice"},
	"dup":        {"Категория с таким названием уже существует", "error"},
	"inuse":      {"Нельзя удалить категорию, пока в ней есть рецепты", "error"},
	"empty":      {"Название не может быть пустым", "error"},
	"reparented": {"Родительская категория обновлена", "notice"},
	"cycle":      {"Нельзя сделать категорию потомком самой себя", "error"},
}

// handleAdminCategories lists categories with their recipe counts for the
// reference-management section.
func (s *Server) handleAdminCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.store.ListCategories(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := s.newPageData(r)
	data["Title"] = "Категории"
	data["Categories"] = categoryTree(cats)
	if m, ok := categoryMessages[r.URL.Query().Get("msg")]; ok {
		data["Message"] = m[0]
		data["MessageClass"] = m[1]
	}
	s.render(w, r, "admin_categories", http.StatusOK, data)
}

// handleCategoryRename renames a category, guarding against duplicates.
func (s *Server) handleCategoryRename(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/admin/categories?msg=empty", http.StatusSeeOther)
		return
	}
	switch err := s.store.RenameCategory(r.Context(), id, name); {
	case errors.Is(err, store.ErrDuplicate):
		http.Redirect(w, r, "/admin/categories?msg=dup", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		s.serverError(w, err)
	default:
		http.Redirect(w, r, "/admin/categories?msg=renamed", http.StatusSeeOther)
	}
}

// handleCategorySetParent sets (or clears) a category's parent, rejecting cycles.
func (s *Server) handleCategorySetParent(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var parent *int64
	if v := strings.TrimSpace(r.PostFormValue("parent_id")); v != "" {
		pid, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		parent = &pid
	}
	switch err := s.store.SetCategoryParent(r.Context(), id, parent); {
	case errors.Is(err, store.ErrCycle):
		http.Redirect(w, r, "/admin/categories?msg=cycle", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		s.serverError(w, err)
	default:
		http.Redirect(w, r, "/admin/categories?msg=reparented", http.StatusSeeOther)
	}
}

// handleCategoryDelete deletes an unused category.
func (s *Server) handleCategoryDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch err := s.store.DeleteCategory(r.Context(), id); {
	case errors.Is(err, store.ErrCategoryInUse):
		http.Redirect(w, r, "/admin/categories?msg=inuse", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		s.serverError(w, err)
	default:
		http.Redirect(w, r, "/admin/categories?msg=deleted", http.StatusSeeOther)
	}
}

// handleAdminRecipes lists recipes for management.
func (s *Server) handleAdminRecipes(w http.ResponseWriter, r *http.Request) {
	recipes, err := s.store.ListRecipes(r.Context(), nil, 0, 0)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := s.newPageData(r)
	data["Title"] = "Управление рецептами"
	data["Recipes"] = recipes
	s.render(w, r, "admin_recipes", http.StatusOK, data)
}

// recipeForm holds the editable values shown on the create/edit form, so a
// failed submission can be redrawn with the user's input intact.
type recipeForm struct {
	Title              string
	Ingredients        []models.IngredientBlock
	StepsHTML          string
	SelectedCategoryID int64
	NewCategory        string
	Error              string
}

// handleRecipeNew renders the empty create form.
func (s *Server) handleRecipeNew(w http.ResponseWriter, r *http.Request) {
	s.renderRecipeForm(w, r, nil, recipeForm{}, http.StatusOK)
}

// handleRecipeEditForm renders the form populated from an existing recipe.
func (s *Server) handleRecipeEditForm(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookupRecipe(w, r)
	if !ok {
		return
	}
	form := recipeForm{
		Title:              rec.Title,
		Ingredients:        rec.Ingredients,
		StepsHTML:          rec.StepsHTML,
		SelectedCategoryID: rec.CategoryID,
	}
	s.renderRecipeForm(w, r, rec, form, http.StatusOK)
}

// renderRecipeForm renders the recipe create/edit form. rec is nil for a new
// recipe; form carries the field values (from the recipe, or from a rejected
// submission being redrawn).
func (s *Server) renderRecipeForm(w http.ResponseWriter, r *http.Request, rec *models.Recipe, form recipeForm, status int) {
	cats, err := s.store.ListCategories(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	ings := form.Ingredients
	if len(ings) == 0 {
		ings = []models.IngredientBlock{{}} // start with one empty block
	}

	data := s.newPageData(r)
	if rec != nil {
		data["Title"] = "Редактирование рецепта"
		data["FormAction"] = "/admin/recipes/" + strconv.FormatInt(rec.ID, 10)
		data["RecipeID"] = rec.ID
		data["IsEdit"] = true
	} else {
		data["Title"] = "Новый рецепт"
		data["FormAction"] = "/admin/recipes"
		data["IsEdit"] = false
	}
	data["RecipeTitle"] = form.Title
	data["Ingredients"] = ings
	data["StepsHTMLSafe"] = template.HTML(form.StepsHTML) //nolint:gosec // sanitized on save and on draft
	data["Categories"] = categoryTree(cats)
	data["SelectedCategoryID"] = form.SelectedCategoryID
	data["NewCategory"] = form.NewCategory
	data["Error"] = form.Error
	s.render(w, r, "recipe_form", status, data)
}

// formCategorySelection extracts the category fields from a submitted form for
// redrawing after a validation error.
func formCategorySelection(r *http.Request) (selectedID int64, newCategory string) {
	newCategory = strings.TrimSpace(r.PostFormValue("new_category"))
	if idStr := strings.TrimSpace(r.PostFormValue("category_id")); idStr != "" {
		selectedID, _ = strconv.ParseInt(idStr, 10, 64)
	}
	return selectedID, newCategory
}

// handleRecipeCreate validates and inserts a new recipe.
func (s *Server) handleRecipeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	in, msg := s.recipeInputFromForm(r)
	if msg != "" {
		selID, newCat := formCategorySelection(r)
		s.renderRecipeForm(w, r, nil, recipeForm{
			Title: in.Title, Ingredients: in.Ingredients, StepsHTML: in.StepsHTML,
			SelectedCategoryID: selID, NewCategory: newCat, Error: msg,
		}, http.StatusBadRequest)
		return
	}
	// Attribute the recipe to its creator so it can be pushed to their iCloud.
	if u := currentUser(r); u != nil {
		in.OwnerID = &u.ID
	}
	rec, err := s.store.CreateRecipe(r.Context(), in)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.reconcileImages(r, rec.ID, in.StepsHTML); err != nil {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/recipes/"+strconv.FormatInt(rec.ID, 10), http.StatusSeeOther)
}

// handleRecipeUpdate validates and saves edits to an existing recipe.
func (s *Server) handleRecipeUpdate(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookupRecipe(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	in, msg := s.recipeInputFromForm(r)
	if msg != "" {
		selID, newCat := formCategorySelection(r)
		s.renderRecipeForm(w, r, rec, recipeForm{
			Title: in.Title, Ingredients: in.Ingredients, StepsHTML: in.StepsHTML,
			SelectedCategoryID: selID, NewCategory: newCat, Error: msg,
		}, http.StatusBadRequest)
		return
	}
	// Preserve iCloud linkage on update.
	in.ICloudNoteID = rec.ICloudNoteID
	in.ICloudEtag = rec.ICloudEtag
	in.OwnerID = rec.OwnerID

	if err := s.store.UpdateRecipe(r.Context(), rec.ID, in); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.reconcileImages(r, rec.ID, in.StepsHTML); err != nil {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/recipes/"+strconv.FormatInt(rec.ID, 10), http.StatusSeeOther)
}

// handleRecipeDelete removes a recipe and its image files.
func (s *Server) handleRecipeDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	files, err := s.store.DeleteRecipe(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	for _, f := range files {
		s.removeUpload(f)
	}
	http.Redirect(w, r, "/admin/recipes", http.StatusSeeOther)
}

// recipeInputFromForm builds a RecipeInput from the form, returning a non-empty
// validation message on bad input.
func (s *Server) recipeInputFromForm(r *http.Request) (store.RecipeInput, string) {
	title := strings.TrimSpace(r.PostFormValue("title"))
	in := store.RecipeInput{
		Title:       title,
		Ingredients: parseIngredients(r),
		StepsHTML:   sanitize.StepsHTML(r.PostFormValue("steps_html")),
	}
	if title == "" {
		return in, "Укажите название блюда"
	}
	catID, err := s.resolveCategory(r.Context(), r)
	if err != nil {
		if errors.Is(err, errNoCategory) {
			return in, "Выберите или введите категорию"
		}
		return in, "Не удалось определить категорию"
	}
	in.CategoryID = catID
	return in, ""
}

// reconcileImages makes recipe_images match the images referenced in the saved
// steps HTML: it records newly-referenced uploads and deletes rows + files for
// uploads no longer referenced.
func (s *Server) reconcileImages(r *http.Request, recipeID int64, html string) error {
	ctx := r.Context()
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
			s.removeUpload(img.Filename)
		}
	}
	for _, name := range referenced {
		if !existingSet[name] {
			ct := typeByExt[strings.ToLower(filepath.Ext(name))]
			if _, err := s.store.AddImage(ctx, recipeID, name, ct); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeUpload deletes an uploaded file, guarding against path traversal.
func (s *Server) removeUpload(name string) {
	if !sanitize.IsValidUploadName(name) {
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.UploadsDir(), name))
}

// lookupRecipe loads the recipe named by the {id} path value, writing a 404 and
// returning ok=false when absent.
func (s *Server) lookupRecipe(w http.ResponseWriter, r *http.Request) (*models.Recipe, bool) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return nil, false
	}
	rec, err := s.store.GetRecipe(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.serverError(w, err)
		return nil, false
	}
	return rec, true
}

func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	logError(err)
	http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
}
