package icloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"recipes/internal/notesync"
)

// conflictTransport returns an atomic-batch failure for records/modify: HTTP 400
// with the culprit record carrying serverErrorCode CONFLICT alongside an
// ATOMIC_ERROR sibling — the real shape that broke conflict detection.
type conflictTransport struct{}

func (conflictTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := `{}`
	status := http.StatusOK
	if strings.Contains(req.URL.Path, "records/modify") {
		status = http.StatusBadRequest
		body = `{"records":[` +
			`{"recordName":"OLD","serverErrorCode":"ATOMIC_ERROR","reason":"atomic"},` +
			`{"recordName":"OLD","serverErrorCode":"CONFLICT","reason":"etag"}` +
			`]}`
	}
	return &http.Response{StatusCode: status,
		Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

// A concurrent-edit (etag) conflict on the atomic delete+create replace must map to
// notesync.ErrEtagConflict (so the engine records a resolvable conflict and keeps
// syncing the other recipes) rather than a generic error that aborts the run.
func TestPushNoteAtomicConflictMapsToErrEtagConflict(t *testing.T) {
	p := New(&http.Client{Transport: conflictTransport{}}, 1)
	n := notesync.Note{ID: "OLD", FolderID: "F1", Title: "Борщ", BodyHTML: "Варить"}
	_, err := p.PushNote(context.Background(), testSession(), n, "etag1")
	if !errors.Is(err, notesync.ErrEtagConflict) {
		t.Fatalf("got %v, want notesync.ErrEtagConflict", err)
	}
}
