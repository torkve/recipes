package notesync

import (
	"context"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"recipes/internal/models"
	"recipes/internal/sanitize"
	"recipes/internal/store"
)

// Engine orchestrates sync between the local store and a notes backend. It is
// the single serialization point for a user's sync operations.
type Engine struct {
	store      *store.Store
	provider   SyncProvider
	binder     Binder
	enc        cipher.AEAD
	uploadsDir string

	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

// NewEngine builds an Engine. syncKey is the 32-byte AES key for session blobs.
func NewEngine(st *store.Store, provider SyncProvider, binder Binder, syncKey []byte, uploadsDir string) (*Engine, error) {
	aead, err := newAEAD(syncKey)
	if err != nil {
		return nil, err
	}
	return &Engine{
		store:      st,
		provider:   provider,
		binder:     binder,
		enc:        aead,
		uploadsDir: uploadsDir,
		locks:      map[int64]*sync.Mutex{},
	}, nil
}

// PullReport / PushReport summarize a sync run.
type PullReport struct{ Created, Updated, Conflicts, Skipped int }
type PushReport struct{ Created, Updated, Conflicts, Skipped int }

// Resolution selects which side wins a conflict.
type Resolution int

const (
	ResolveKeepLocal Resolution = iota
	ResolveKeepRemote
)

func (e *Engine) userLock(id int64) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.locks[id]
	if !ok {
		m = &sync.Mutex{}
		e.locks[id] = m
	}
	return m
}

// --- bind flow --------------------------------------------------------------

// BeginBind submits credentials. Returns pending=true with an opaque handle
// when a 2FA code is required next.
func (e *Engine) BeginBind(ctx context.Context, userID int64, appleID, password string) (pending bool, handle string, err error) {
	res, err := e.binder.Begin(ctx, appleID, password)
	if err != nil {
		return false, "", err
	}
	if _, err := e.store.UpsertICloudAccount(ctx, userID, appleID); err != nil {
		return false, "", err
	}
	if res.Pending {
		return true, base64.StdEncoding.EncodeToString(res.Handle), nil
	}
	if res.Session == nil {
		return false, "", errors.New("notesync: binder returned no session and not pending")
	}
	return false, "", e.persistSession(ctx, userID, res.Session)
}

// CompleteBind submits the 2FA code for a pending bind.
func (e *Engine) CompleteBind(ctx context.Context, userID int64, handle, code string) error {
	raw, err := base64.StdEncoding.DecodeString(handle)
	if err != nil {
		return fmt.Errorf("notesync: bad bind handle: %w", err)
	}
	sess, err := e.binder.Complete(ctx, BindHandle(raw), code)
	if err != nil {
		return err
	}
	return e.persistSession(ctx, userID, sess)
}

// Unbind removes a user's iCloud binding.
func (e *Engine) Unbind(ctx context.Context, userID int64) error {
	return e.store.DeleteICloudAccount(ctx, userID)
}

// SetFolder records the chosen root Notes folder.
func (e *Engine) SetFolder(ctx context.Context, userID int64, folder string) error {
	return e.store.SetNotesFolder(ctx, userID, folder)
}

// ListRemoteFolders lists folders under the bound root (for folder selection).
func (e *Engine) ListRemoteFolders(ctx context.Context, userID int64) ([]Folder, error) {
	acct, err := e.store.GetICloudAccount(ctx, userID)
	if err != nil {
		return nil, err
	}
	sess, err := e.restoreSession(ctx, acct)
	if err != nil {
		return nil, err
	}
	// Pass an empty root so the picker shows the whole folder tree (including the
	// already-chosen folder), regardless of any saved selection.
	return e.provider.ListFolders(ctx, sess, "")
}

func (e *Engine) persistSession(ctx context.Context, userID int64, sess Session) error {
	raw, err := sess.Bytes()
	if err != nil {
		return err
	}
	blob, err := sealBlob(e.enc, raw)
	if err != nil {
		return err
	}
	return e.store.SaveICloudSession(ctx, userID, blob)
}

func (e *Engine) restoreSession(ctx context.Context, acct *models.ICloudAccount) (Session, error) {
	if len(acct.SessionBlob) == 0 {
		return nil, ErrReauthRequired
	}
	raw, err := openBlob(e.enc, acct.SessionBlob)
	if err != nil {
		return nil, fmt.Errorf("notesync: decrypt session: %w", err)
	}
	sess, err := e.provider.Restore(ctx, raw)
	if err != nil {
		return nil, err
	}
	// Persist any token refresh that Restore performed.
	if refreshed, rerr := sess.Bytes(); rerr == nil && len(refreshed) > 0 {
		if blob, serr := sealBlob(e.enc, refreshed); serr == nil {
			_ = e.store.SaveICloudSession(ctx, acct.UserID, blob)
		}
	}
	return sess, nil
}

// --- conflicts --------------------------------------------------------------

// Conflicts lists unresolved sync conflicts.
func (e *Engine) Conflicts(ctx context.Context) ([]models.SyncConflict, error) {
	return e.store.ListConflicts(ctx)
}

// ResolveConflict applies the chosen side and clears the conflict. For
// keep-remote it re-pulls the single note; for keep-local it pushes. Here we
// resolve the bookkeeping by marking resolved and rebasing sync_state so the
// next sync treats the chosen side as the base.
func (e *Engine) ResolveConflict(ctx context.Context, userID int64, conflictID int64, choice Resolution) error {
	l := e.userLock(userID)
	l.Lock()
	defer l.Unlock()

	c, err := e.store.GetConflict(ctx, conflictID)
	if err != nil {
		return err
	}
	if c.RecipeID == nil {
		return e.store.ResolveConflict(ctx, conflictID)
	}
	rec, err := e.store.GetRecipe(ctx, *c.RecipeID)
	if err != nil {
		return err
	}

	acct, err := e.store.GetICloudAccount(ctx, userID)
	if err != nil {
		return err
	}
	sess, err := e.restoreSession(ctx, acct)
	if err != nil {
		return err
	}

	root := FolderID(acct.NotesFolder)

	// Find the current remote note (if the recipe is linked); used by both sides.
	var remote *Note
	if rec.ICloudNoteID != nil {
		_, notes, _, err := e.provider.FetchZone(ctx, sess, root, "")
		if err != nil {
			return err
		}
		for i := range notes {
			if string(notes[i].ID) == *rec.ICloudNoteID {
				remote = &notes[i]
				break
			}
		}
	}

	switch choice {
	case ResolveKeepLocal:
		// Re-push the recipe, replacing the live note (or recreating a vanished one).
		if _, err := e.pushRecipe(ctx, sess, root, rec, remote); err != nil {
			return err
		}
	case ResolveKeepRemote:
		if remote != nil {
			if err := e.applyRemote(ctx, sess, userID, rec, *remote); err != nil {
				return err
			}
		}
	}
	return e.store.ResolveConflict(ctx, conflictID)
}

// --- helpers ----------------------------------------------------------------

var extForType = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

func (e *Engine) saveImage(img NoteImage) (string, error) {
	ext, ok := extForType[img.ContentType]
	if !ok {
		return "", fmt.Errorf("notesync: unsupported image type %q", img.ContentType)
	}
	name := uuid.NewString() + ext
	if err := os.WriteFile(filepath.Join(e.uploadsDir, name), img.Data, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

// buildRecipeInput maps a note to a store.RecipeInput, downloading + saving its
// inline images and sanitizing the body. Each image is substituted at its
// @@IMG:id@@ marker; any marker left unresolved (download failed or unsupported
// type) is stripped so it never reaches the rendered recipe.
func (e *Engine) buildRecipeInput(ctx context.Context, sess Session, n Note, categoryID, userID int64) store.RecipeInput {
	body := n.BodyHTML
	for _, img := range n.Images {
		fetched, err := e.provider.FetchImage(ctx, sess, img)
		if err != nil {
			continue // download failed; its marker is stripped below
		}
		name, err := e.saveImage(fetched)
		if err != nil {
			continue // skip unsupported/broken images
		}
		body = strings.ReplaceAll(body, imgMarker(img.ID), `<img src="/uploads/`+name+`" alt="">`)
	}
	body = imgMarkerRE.ReplaceAllString(body, "") // drop any unresolved markers
	steps := sanitize.StepsHTML(body)
	noteID := string(n.ID)
	etag := string(n.Etag)
	uid := userID
	return store.RecipeInput{
		Title:        strings.TrimSpace(n.Title),
		CategoryID:   categoryID,
		Ingredients:  ChecklistsToIngredients(n.Checklists),
		StepsHTML:    steps,
		ICloudNoteID: &noteID,
		ICloudEtag:   &etag,
		OwnerID:      &uid,
	}
}

// reconcileImages records images referenced by the steps and removes files for
// images no longer referenced (mirrors the web import path).
func (e *Engine) reconcileImages(ctx context.Context, recipeID int64, stepsHTML string) error {
	referenced := sanitize.ImageFilenames(stepsHTML)
	refSet := map[string]bool{}
	for _, n := range referenced {
		refSet[n] = true
	}
	existing, err := e.store.ImagesForRecipe(ctx, recipeID)
	if err != nil {
		return err
	}
	existingSet := map[string]bool{}
	for _, img := range existing {
		existingSet[img.Filename] = true
		if !refSet[img.Filename] {
			_ = e.store.DeleteImageByName(ctx, recipeID, img.Filename)
			if sanitize.IsValidUploadName(img.Filename) {
				_ = os.Remove(filepath.Join(e.uploadsDir, img.Filename))
			}
		}
	}
	for _, name := range referenced {
		if !existingSet[name] {
			ct := ""
			if _, err := e.store.AddImage(ctx, recipeID, name, ct); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveFolderCategories maps the folder subtree to categories, returning a
// folder-id -> category-id map. Parents are created before children.
func (e *Engine) resolveFolderCategories(ctx context.Context, folders []Folder) (map[FolderID]int64, error) {
	out := map[FolderID]int64{}
	for _, f := range SortFoldersTopologically(folders) {
		var parent *int64
		if f.ParentID != "" {
			if pid, ok := out[f.ParentID]; ok {
				parent = &pid
			}
		}
		name := strings.TrimSpace(f.Name)
		if name == "" {
			continue
		}
		cat, err := e.store.CategoryByNorm(ctx, store.NormalizeName(name))
		if errors.Is(err, store.ErrNotFound) {
			cat, err = e.store.CreateCategoryWithParent(ctx, name, parent, models.SourceICloud)
		} else if err == nil && cat.ParentID == nil && parent != nil && *parent != cat.ID {
			// Adopt the folder parent for a category that predates hierarchy
			// support (stuck at NULL). Only when currently NULL, so a manual
			// reparent in the app is never silently overwritten by sync. Two
			// folders whose names normalize equal collapse onto one category, so
			// a self-parent (and thus ErrCycle) is possible with malformed remote
			// data — tolerate it rather than aborting the whole pull.
			if serr := e.store.SetCategoryParent(ctx, cat.ID, parent); serr != nil && !errors.Is(serr, store.ErrCycle) {
				return nil, serr
			}
		}
		if err != nil {
			return nil, err
		}
		out[f.ID] = cat.ID
	}
	return out, nil
}
