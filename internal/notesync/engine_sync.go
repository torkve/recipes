package notesync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"recipes/internal/models"
	"recipes/internal/store"
)

func strptr(s string) *string { return &s }

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

var typeForExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// PullUser imports new/changed notes for a user. Folders become categories,
// notes become recipes; conflicting edits are recorded for manual resolution.
func (e *Engine) PullUser(ctx context.Context, userID int64) (PullReport, error) {
	l := e.userLock(userID)
	l.Lock()
	defer l.Unlock()

	var rep PullReport
	acct, err := e.store.GetICloudAccount(ctx, userID)
	if err != nil {
		return rep, err
	}
	sess, err := e.restoreSession(ctx, acct)
	if err != nil {
		return rep, err
	}
	root := FolderID(acct.NotesFolder)

	folders, err := e.provider.ListFolders(ctx, sess, root)
	if err != nil {
		return rep, err
	}
	folderCat, err := e.resolveFolderCategories(ctx, folders)
	if err != nil {
		return rep, err
	}

	notes, _, err := e.provider.ChangedNotes(ctx, sess, root, "")
	if err != nil {
		return rep, err
	}

	for _, n := range notes {
		if strings.TrimSpace(n.Title) == "" {
			e.recordConflict(ctx, nil, strptr(string(n.ID)), models.ConflictNoteUnparsable, "Заметка без заголовка")
			rep.Conflicts++
			continue
		}
		catID, ok := folderCat[n.FolderID]
		if !ok {
			cat, err := e.store.GetOrCreateCategory(ctx, "Импортированные", models.SourceICloud)
			if err != nil {
				return rep, err
			}
			catID = cat.ID
		}

		existing, err := e.store.GetRecipeByNoteID(ctx, string(n.ID))
		if errors.Is(err, store.ErrNotFound) {
			in := e.buildRecipeInput(n, catID, userID)
			rec, err := e.store.CreateRecipe(ctx, in)
			if err != nil {
				return rep, err
			}
			if err := e.reconcileImages(ctx, rec.ID, in.StepsHTML); err != nil {
				return rep, err
			}
			full, err := e.store.GetRecipe(ctx, rec.ID)
			if err != nil {
				return rep, err
			}
			if err := e.store.UpsertSyncState(ctx, models.SyncState{
				RecipeID: rec.ID, NoteID: string(n.ID),
				LocalHash: HashRecipe(full), RemoteHash: HashNote(n),
			}); err != nil {
				return rep, err
			}
			rep.Created++
			continue
		} else if err != nil {
			return rep, err
		}

		localHash := HashRecipe(existing)
		remoteHash := HashNote(n)
		var localChanged, remoteChanged bool
		state, serr := e.store.GetSyncState(ctx, existing.ID)
		switch {
		case errors.Is(serr, store.ErrNotFound):
			// No base recorded: only a real content divergence is a change.
			localChanged = localHash != remoteHash
			remoteChanged = localHash != remoteHash
		case serr != nil:
			return rep, serr
		default:
			localChanged = localHash != state.LocalHash
			remoteChanged = remoteHash != state.RemoteHash
		}

		switch Classify(localChanged, remoteChanged) {
		case DecisionNoOp, DecisionApplyLocal:
			rep.Skipped++
		case DecisionApplyRemote:
			if err := e.applyRemote(ctx, userID, existing, n); err != nil {
				return rep, err
			}
			rep.Updated++
		case DecisionConflict:
			e.recordConflict(ctx, &existing.ID, strptr(string(n.ID)), models.ConflictBothChanged,
				"Изменены и заметка в iCloud, и рецепт в приложении")
			rep.Conflicts++
		}
	}
	return rep, nil
}

// applyRemote overwrites a recipe from a note, keeping its current category.
func (e *Engine) applyRemote(ctx context.Context, userID int64, rec *models.Recipe, n Note) error {
	in := e.buildRecipeInput(n, rec.CategoryID, userID)
	if err := e.store.UpdateRecipe(ctx, rec.ID, in); err != nil {
		return err
	}
	if err := e.reconcileImages(ctx, rec.ID, in.StepsHTML); err != nil {
		return err
	}
	full, err := e.store.GetRecipe(ctx, rec.ID)
	if err != nil {
		return err
	}
	return e.store.UpsertSyncState(ctx, models.SyncState{
		RecipeID: rec.ID, NoteID: string(n.ID),
		LocalHash: HashRecipe(full), RemoteHash: HashNote(n),
	})
}

// PushUser sends a user's recipes back to iCloud: new recipes create notes,
// locally-changed linked recipes update their notes (etag-guarded).
func (e *Engine) PushUser(ctx context.Context, userID int64) (PushReport, error) {
	l := e.userLock(userID)
	l.Lock()
	defer l.Unlock()

	var rep PushReport
	acct, err := e.store.GetICloudAccount(ctx, userID)
	if err != nil {
		return rep, err
	}
	sess, err := e.restoreSession(ctx, acct)
	if err != nil {
		return rep, err
	}
	root := FolderID(acct.NotesFolder)

	ids, err := e.store.ListRecipeIDsByOwner(ctx, userID)
	if err != nil {
		return rep, err
	}

	for _, id := range ids {
		rec, err := e.store.GetRecipe(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return rep, err
		}

		isNew := rec.ICloudNoteID == nil
		if !isNew {
			state, serr := e.store.GetSyncState(ctx, rec.ID)
			localChanged := errors.Is(serr, store.ErrNotFound) || (serr == nil && HashRecipe(rec) != state.LocalHash)
			if serr != nil && !errors.Is(serr, store.ErrNotFound) {
				return rep, serr
			}
			if !localChanged {
				rep.Skipped++
				continue
			}
		}

		_, err = e.pushRecipe(ctx, sess, root, rec)
		if errors.Is(err, ErrEtagConflict) {
			var nid *string
			if rec.ICloudNoteID != nil {
				nid = strptr(*rec.ICloudNoteID)
			}
			e.recordConflict(ctx, &rec.ID, nid, models.ConflictBothChanged,
				"Заметка изменена в iCloud с момента последней синхронизации")
			rep.Conflicts++
			continue
		}
		if err != nil {
			return rep, err
		}
		if isNew {
			rep.Created++
		} else {
			rep.Updated++
		}
	}
	return rep, nil
}

// pushRecipe creates or updates the note for a recipe and re-links it.
func (e *Engine) pushRecipe(ctx context.Context, sess Session, root FolderID, rec *models.Recipe) (Note, error) {
	folderID := root
	if rec.Category != nil && strings.TrimSpace(rec.Category.Name) != "" {
		if f, err := e.provider.EnsureFolder(ctx, sess, root, rec.Category.Name); err == nil {
			folderID = f.ID
		}
	}

	note := e.recipeToNote(rec, folderID)
	expected := Etag("")
	if rec.ICloudEtag != nil {
		expected = Etag(*rec.ICloudEtag)
	}
	saved, err := e.provider.PushNote(ctx, sess, note, expected)
	if err != nil {
		return Note{}, err
	}

	in := store.RecipeInput{
		Title:        rec.Title,
		CategoryID:   rec.CategoryID,
		Ingredients:  rec.Ingredients,
		StepsHTML:    rec.StepsHTML,
		ICloudNoteID: strptr(string(saved.ID)),
		ICloudEtag:   strptr(string(saved.Etag)),
		OwnerID:      rec.OwnerID,
	}
	if err := e.store.UpdateRecipe(ctx, rec.ID, in); err != nil {
		return Note{}, err
	}
	full, err := e.store.GetRecipe(ctx, rec.ID)
	if err != nil {
		return Note{}, err
	}
	if err := e.store.UpsertSyncState(ctx, models.SyncState{
		RecipeID: rec.ID, NoteID: string(saved.ID),
		LocalHash: HashRecipe(full), RemoteHash: HashNote(saved),
	}); err != nil {
		return Note{}, err
	}
	return saved, nil
}

// recipeToNote maps a recipe to a note for pushing, loading image bytes.
func (e *Engine) recipeToNote(rec *models.Recipe, folderID FolderID) Note {
	n := Note{
		FolderID:   folderID,
		Title:      rec.Title,
		BodyHTML:   rec.StepsHTML,
		Checklists: IngredientsToChecklists(rec.Ingredients),
	}
	if rec.ICloudNoteID != nil {
		n.ID = NoteID(*rec.ICloudNoteID)
	}
	if rec.ICloudEtag != nil {
		n.Etag = Etag(*rec.ICloudEtag)
	}
	for _, img := range rec.Images {
		data, err := os.ReadFile(filepath.Join(e.uploadsDir, img.Filename))
		if err != nil {
			continue
		}
		ct := img.ContentType
		if ct == "" {
			ct = typeForExt[strings.ToLower(filepath.Ext(img.Filename))]
		}
		n.Images = append(n.Images, NoteImage{ID: img.Filename, ContentType: ct, Data: data})
	}
	return n
}

// recordConflict inserts a conflict unless an identical unresolved one exists.
func (e *Engine) recordConflict(ctx context.Context, recipeID *int64, noteID *string, kind, detail string) {
	existing, _ := e.store.ListConflicts(ctx)
	for _, c := range existing {
		if c.Kind == kind && eqInt64Ptr(c.RecipeID, recipeID) && eqStrPtr(c.NoteID, noteID) {
			return
		}
	}
	_, _ = e.store.InsertConflict(ctx, models.SyncConflict{
		RecipeID: recipeID, NoteID: noteID, Kind: kind, Detail: detail,
	})
}
