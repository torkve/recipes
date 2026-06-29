// Package icloudadapter adapts the general-purpose github.com/torkve/icloud-notes
// client to the engine's backend-agnostic notesync.SyncProvider and notesync.Binder
// interfaces, translating between the engine's recipe-flavored note model and the
// library's structured note model.
package icloudadapter

import (
	"context"
	"errors"

	"github.com/torkve/icloud-notes/icloud"
	"github.com/torkve/icloud-notes/notes"

	"recipes/internal/notesync"
)

// Adapter implements notesync.SyncProvider and notesync.Binder against iCloud.
type Adapter struct {
	auth *icloud.Authenticator
}

var (
	_ notesync.SyncProvider = (*Adapter)(nil)
	_ notesync.Binder       = (*Adapter)(nil)
)

// New returns an Adapter whose authenticator is configured with the given options
// (e.g. icloud.WithSRPVariant, icloud.WithHTTPClient).
func New(opts ...icloud.Option) *Adapter {
	return &Adapter{auth: icloud.NewAuthenticator(opts...)}
}

// Restore rebuilds a session from a persisted blob, refreshing tokens as needed.
func (a *Adapter) Restore(ctx context.Context, blob []byte) (notesync.Session, error) {
	sess, err := icloud.ParseSession(blob)
	if err != nil {
		return nil, err
	}
	client, err := a.auth.Restore(ctx, sess)
	if err != nil {
		return nil, mapErr(err)
	}
	return &session{client: client}, nil
}

// ListFolders returns the folder subtree under root.
func (a *Adapter) ListFolders(ctx context.Context, sess notesync.Session, root notesync.FolderID) ([]notesync.Folder, error) {
	client, err := clientOf(sess)
	if err != nil {
		return nil, err
	}
	libFolders, err := client.ListFolders(ctx, notes.FolderID(root))
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]notesync.Folder, 0, len(libFolders))
	for _, f := range libFolders {
		out = append(out, fromFolder(f))
	}
	return out, nil
}

// FetchZone enumerates the zone under root since the given token.
func (a *Adapter) FetchZone(ctx context.Context, sess notesync.Session, root notesync.FolderID, since string) ([]notesync.Folder, []notesync.Note, string, error) {
	client, err := clientOf(sess)
	if err != nil {
		return nil, nil, "", err
	}
	cs, err := client.Changes(ctx, notes.FolderID(root), since)
	if err != nil {
		return nil, nil, "", mapErr(err)
	}
	folders := make([]notesync.Folder, 0, len(cs.Folders))
	for _, f := range cs.Folders {
		folders = append(folders, fromFolder(f))
	}
	ns := make([]notesync.Note, 0, len(cs.Notes))
	for _, n := range cs.Notes {
		ns = append(ns, fromNote(n))
	}
	return folders, ns, cs.Next, nil
}

// FetchImage downloads one image's bytes.
func (a *Adapter) FetchImage(ctx context.Context, sess notesync.Session, img notesync.NoteImage) (notesync.NoteImage, error) {
	client, err := clientOf(sess)
	if err != nil {
		return notesync.NoteImage{}, err
	}
	got, err := client.FetchImage(ctx, toImage(img))
	if err != nil {
		return notesync.NoteImage{}, mapErr(err)
	}
	return fromImage(got), nil
}

// PushNote creates a note (n.ID == "") or updates the linked note in place,
// falling back to recreation when the prior version vector is unreadable.
func (a *Adapter) PushNote(ctx context.Context, sess notesync.Session, n notesync.Note, expectedEtag notesync.Etag, prev notesync.PrevNote) (notesync.Note, error) {
	client, err := clientOf(sess)
	if err != nil {
		return notesync.Note{}, err
	}
	lib := toNote(n)

	var saved notes.Note
	if n.ID == "" {
		saved, err = client.CreateNote(ctx, lib)
	} else {
		if len(prev.ReplicaUUID) == 16 {
			var id [16]byte
			copy(id[:], prev.ReplicaUUID)
			client.SetReplicaID(id)
		}
		prior := notes.Note{ID: notes.NoteID(n.ID), RawBody: prev.RawBody}
		for _, attID := range prev.AttachmentIDs {
			prior.Images = append(prior.Images, notes.Image{ID: attID})
		}
		saved, err = client.UpdateNote(ctx, lib, prior, notes.Etag(expectedEtag))
	}
	if err != nil {
		return notesync.Note{}, mapErr(err)
	}
	return fromNote(saved), nil
}

// EnsureFolder finds or creates a subfolder under parent.
func (a *Adapter) EnsureFolder(ctx context.Context, sess notesync.Session, parent notesync.FolderID, name string) (notesync.Folder, error) {
	client, err := clientOf(sess)
	if err != nil {
		return notesync.Folder{}, err
	}
	f, err := client.EnsureFolder(ctx, notes.FolderID(parent), name)
	if err != nil {
		return notesync.Folder{}, mapErr(err)
	}
	return fromFolder(f), nil
}

// Begin submits Apple ID + password, returning a session or a pending 2FA handle.
func (a *Adapter) Begin(ctx context.Context, appleID, password string) (notesync.BindResult, error) {
	res, err := a.auth.SignIn(ctx, appleID, password)
	if err != nil {
		return notesync.BindResult{}, mapErr(err)
	}
	out := notesync.BindResult{Pending: res.Pending, Handle: notesync.BindHandle(res.Handle)}
	if res.Client != nil {
		out.Session = &session{client: res.Client}
	}
	return out, nil
}

// Complete submits the 2FA code for a pending bind.
func (a *Adapter) Complete(ctx context.Context, handle notesync.BindHandle, code string) (notesync.Session, error) {
	client, err := a.auth.VerifyCode(ctx, icloud.BindHandle(handle), code)
	if err != nil {
		return nil, mapErr(err)
	}
	return &session{client: client}, nil
}

// mapErr translates library sentinel errors into the engine's equivalents so the
// engine's errors.Is checks keep working.
func mapErr(err error) error {
	switch {
	case errors.Is(err, icloud.ErrEtagConflict):
		return notesync.ErrEtagConflict
	case errors.Is(err, icloud.ErrReauthRequired):
		return notesync.ErrReauthRequired
	default:
		return err
	}
}
