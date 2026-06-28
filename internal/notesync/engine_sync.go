package notesync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
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

	// A single zone scan returns both folders and notes.
	folders, notes, _, err := e.provider.FetchZone(ctx, sess, root, "")
	if err != nil {
		return rep, err
	}
	folderCat, err := e.resolveFolderCategories(ctx, folders)
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
			in := e.buildRecipeInput(ctx, sess, n, catID, userID)
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
			if err := e.applyRemote(ctx, sess, userID, existing, n); err != nil {
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
func (e *Engine) applyRemote(ctx context.Context, sess Session, userID int64, rec *models.Recipe, n Note) error {
	in := e.buildRecipeInput(ctx, sess, n, rec.CategoryID, userID)
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

	// Our stable CRDT replica id, minted once per account. In-place note updates
	// advance this replica's version-vector counters so they propagate to devices.
	replicaUUID, err := e.store.EnsureReplicaUUID(ctx, userID)
	if err != nil {
		return rep, err
	}

	// Read current remote state once so conflict detection is content-based: a stale
	// stored etag must not look like a remote edit. Mirrors the pull path.
	_, notes, _, err := e.provider.FetchZone(ctx, sess, root, "")
	if err != nil {
		return rep, err
	}
	remoteByID := make(map[NoteID]Note, len(notes))
	for _, n := range notes {
		remoteByID[n.ID] = n
	}

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

		op, err := e.classifyPush(ctx, rec)
		if err != nil {
			return rep, err
		}
		if op == pushSkip {
			rep.Skipped++
			continue
		}

		// Resolve the current remote note for a linked recipe. A vanished note
		// (remote == nil) is recreated; a present note that changed since our last
		// sync is a genuine conflict.
		var remote *Note
		if rec.ICloudNoteID != nil {
			if n, ok := remoteByID[NoteID(*rec.ICloudNoteID)]; ok {
				remote = &n
			}
		}
		if op == pushUpdate && remote != nil {
			base := ""
			if state, serr := e.store.GetSyncState(ctx, rec.ID); serr == nil {
				base = state.RemoteHash
			} else if !errors.Is(serr, store.ErrNotFound) {
				return rep, serr
			}
			if HashNote(*remote) != base {
				e.recordConflict(ctx, &rec.ID, strptr(*rec.ICloudNoteID), models.ConflictBothChanged,
					"Заметка изменена в iCloud с момента последней синхронизации")
				rep.Conflicts++
				continue
			}
		}

		_, err = e.pushRecipe(ctx, sess, root, rec, remote, replicaUUID)
		if errors.Is(err, ErrEtagConflict) {
			// TOCTOU backstop: the note changed between the zone read and the delete.
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
		if op == pushCreate {
			rep.Created++
		} else {
			rep.Updated++
		}
	}
	return rep, nil
}

// pushOp is what a push would do for a single recipe.
type pushOp int

const (
	pushSkip   pushOp = iota // linked and unchanged since last sync
	pushCreate               // no linked note yet -> create one
	pushUpdate               // linked and locally changed -> overwrite the note
)

// classifyPush decides, without performing any work, what a push would do for
// one recipe. A recipe with no linked note is a create; a linked recipe is an
// update only if its content changed since the last sync, otherwise a skip. A
// missing sync-state row (linked but never recorded) counts as changed, matching
// the original push behavior. Any non-not-found store error is propagated.
//
// PushUser and PlanPushDiff share this so the preview and the executor can never
// disagree.
func (e *Engine) classifyPush(ctx context.Context, rec *models.Recipe) (pushOp, error) {
	if rec.ICloudNoteID == nil {
		return pushCreate, nil
	}
	state, serr := e.store.GetSyncState(ctx, rec.ID)
	if serr != nil && !errors.Is(serr, store.ErrNotFound) {
		return pushSkip, serr
	}
	// Short-circuit keeps a nil state (not-found) from being dereferenced.
	localChanged := errors.Is(serr, store.ErrNotFound) || HashRecipe(rec) != state.LocalHash
	if localChanged {
		return pushUpdate, nil
	}
	return pushSkip, nil
}

// pushRecipe creates or replaces the note for a recipe and re-links it. When remote
// is non-nil it replaces that live note (PushNote deletes it — guarded by its current
// etag — and creates a fresh one); when remote is nil it creates a new note, which also
// re-links a recipe whose old note has vanished.
func (e *Engine) pushRecipe(ctx context.Context, sess Session, root FolderID, rec *models.Recipe, remote *Note, replicaUUID []byte) (Note, error) {
	folderID := root
	if rec.Category != nil && strings.TrimSpace(rec.Category.Name) != "" {
		if f, err := e.provider.EnsureFolder(ctx, sess, root, rec.Category.Name); err == nil {
			folderID = f.ID
		}
	}

	note := e.recipeToNote(rec, folderID)
	expected := Etag("")
	prev := PrevNote{ReplicaUUID: replicaUUID}
	if remote != nil {
		note.ID = remote.ID    // update/replace targets the live note...
		expected = remote.Etag // ...guarded by its current etag
		// Carry the live note's raw body (its CRDT version vector) for an in-place
		// update, and its attachment ids for the soft-delete-and-recreate fallback.
		prev.RawBody = remote.RawBody
		for _, im := range remote.Images {
			prev.AttachmentIDs = append(prev.AttachmentIDs, im.ID)
		}
	} else {
		note.ID = "" // pure create (new recipe, or the old note is gone)
	}
	saved, err := e.provider.PushNote(ctx, sess, note, expected, prev)
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

// imgSrcRE matches an inline <img> in the steps and captures its uploads filename,
// so push can turn it into an @@IMG:filename@@ marker the provider places inline.
var imgSrcRE = regexp.MustCompile(`(?is)<img\b[^>]*src="/uploads/([^"?]+)"[^>]*>`)

// recipeToNote maps a recipe to a note for pushing, loading image bytes.
func (e *Engine) recipeToNote(rec *models.Recipe, folderID FolderID) Note {
	// The pushed body is plain text, one line per step paragraph, with inline images
	// kept as @@IMG:filename@@ markers (the icloud provider encodes them into the note
	// protobuf). PlainTextHTML-based hashing (HashNote/HashRecipe) collapses newlines
	// and strips image markers, so the round trip stays stable.
	withMarkers := imgSrcRE.ReplaceAllString(rec.StepsHTML, "@@IMG:$1@@")
	n := Note{
		FolderID:   folderID,
		Title:      rec.Title,
		BodyHTML:   strings.Join(stepLinesKeepMarkers(withMarkers), "\n"),
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
