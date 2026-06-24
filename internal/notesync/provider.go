// Package notesync is the backend-agnostic sync engine: it owns the
// recipe/category mapping, conflict detection and orchestration against the
// store, and consumes a SyncProvider/Binder for the actual notes backend
// (iCloud). It never imports the iCloud client, so its logic is fully
// unit-testable with a fake provider. The package is named notesync rather
// than sync to avoid shadowing the standard library sync package.
package notesync

import (
	"context"
	"errors"
)

// ErrReauthRequired indicates the persisted session can no longer authenticate
// (e.g. the trust token was revoked) and the user must re-bind interactively.
var ErrReauthRequired = errors.New("notesync: reauthentication required")

// Opaque identifiers — the engine never interprets backend-specific structure.
type (
	NoteID   string
	FolderID string
	Etag     string
)

// Folder is a notes folder in the synced subtree.
type Folder struct {
	ID       FolderID
	ParentID FolderID // "" for the bound root
	Name     string
}

// NoteImage is a decoded image attachment of a note.
type NoteImage struct {
	ID          string
	ContentType string
	Data        []byte
}

// Note is a backend note projected into the fields the engine maps to a recipe.
type Note struct {
	ID         NoteID
	FolderID   FolderID
	Etag       Etag
	Title      string
	BodyHTML   string      // raw/untrusted; the engine sanitizes before storing
	Checklists [][]string  // each checklist becomes one ingredient block
	Images     []NoteImage // re-hosted under /uploads/ on import
}

// Session is opaque, serializable auth state persisted (encrypted) in
// icloud_accounts.session_blob.
type Session interface {
	Bytes() ([]byte, error)
	Expired() bool
}

// BindHandle carries in-flight bind state (cookies, scnt, auth headers) between
// the credentials step and the 2FA step. It is opaque and serializable so it
// can be stashed in the user's server-side session.
type BindHandle []byte

// BindResult is returned by Binder.Begin.
type BindResult struct {
	Session Session    // non-nil when no 2FA is required
	Pending bool       // true => a 2FA code is required next
	Handle  BindHandle // opaque continuation for Complete
}

// SyncProvider performs note/folder operations for an authenticated session.
// iCloud is the only implementor.
type SyncProvider interface {
	// Restore rebuilds a session from persisted bytes, refreshing tokens
	// transparently. Returns ErrReauthRequired when re-binding is needed.
	Restore(ctx context.Context, blob []byte) (Session, error)

	// ListFolders returns the folder subtree under root (cheap, folders only).
	ListFolders(ctx context.Context, sess Session, root FolderID) ([]Folder, error)

	// FetchZone enumerates the zone in a single pass, returning the folders under
	// root and the in-scope notes changed since the given token, plus the next
	// token to persist. since == "" performs a full enumeration.
	FetchZone(ctx context.Context, sess Session, root FolderID, since string) (folders []Folder, notes []Note, next string, err error)

	// PushNote creates (expectedEtag == "") or updates a note. A mismatch
	// between expectedEtag and the live note must be reported as ErrEtagConflict
	// rather than overwriting.
	PushNote(ctx context.Context, sess Session, n Note, expectedEtag Etag) (Note, error)

	// EnsureFolder finds or creates a subfolder under parent.
	EnsureFolder(ctx context.Context, sess Session, parent FolderID, name string) (Folder, error)
}

// Binder runs the interactive sign-in (+ HSA2 2FA) flow.
type Binder interface {
	// Begin submits Apple ID + password. Pending=true means a 2FA code is needed.
	Begin(ctx context.Context, appleID, password string) (BindResult, error)
	// Complete submits the 2FA code against a pending handle from Begin.
	Complete(ctx context.Context, handle BindHandle, code string) (Session, error)
}

// ErrEtagConflict is returned by PushNote when the remote note changed since the
// engine last read it (optimistic-concurrency failure).
var ErrEtagConflict = errors.New("notesync: remote note etag conflict")
