package icloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"recipes/internal/notesync"
)

// captureTransport records the records/modify request body and echoes back the Note
// record it creates or updates (matched by the recordName in the request) so PushNote
// succeeds.
type captureTransport struct{ lastModify string }

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	out := `{}`
	if strings.Contains(req.URL.Path, "records/lookup") {
		// Echo each looked-up Attachment with a Media reference (MEDIA-<name>) so the
		// in-place update's old-child cleanup can find and delete the backing Media.
		var q struct {
			Records []struct{ RecordName string }
		}
		_ = json.Unmarshal(body, &q)
		var recs []string
		for _, r := range q.Records {
			recs = append(recs, fmt.Sprintf(
				`{"recordName":%q,"recordType":"Attachment","fields":{"Media":{"value":{"recordName":%q}}}}`,
				r.RecordName, "MEDIA-"+r.RecordName))
		}
		out = fmt.Sprintf(`{"records":[%s]}`, strings.Join(recs, ","))
	}
	if strings.Contains(req.URL.Path, "records/modify") {
		c.lastModify = string(body)
		var m struct {
			Operations []struct {
				OperationType string
				Record        struct{ RecordName, RecordType string }
			}
		}
		_ = json.Unmarshal(body, &m)
		name := "NEW"
		for _, op := range m.Operations {
			// The note we return is the one we created or updated (not a soft-deleted one;
			// the create op, if any, is last).
			if op.Record.RecordType == "Note" && (op.OperationType == "create" || op.OperationType == "update") {
				name = op.Record.RecordName
			}
		}
		out = fmt.Sprintf(`{"records":[{"recordName":%q,"recordType":"Note","fields":{}}]}`, name)
	}
	return &http.Response{StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(out)), Header: make(http.Header)}, nil
}

// A linked note with a readable CRDT version vector is UPDATED in place: same recordName,
// etag-guarded, no delete (so no validating-reference error, no trash).
func TestPushNoteInPlaceUpdate(t *testing.T) {
	ct := &captureTransport{}
	p := New(&http.Client{Transport: ct}, 1)
	// A real prior body so parseObjectTable can read its object table.
	rawBody, err := encodeMergeableNoteBody("T", nil, "old step", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ourUUID := make([]byte, 16)
	ourUUID[0] = 0xAB

	n := notesync.Note{ID: "OLD-NOTE", FolderID: "F1", Title: "T", BodyHTML: "new step"}
	saved, err := p.PushNote(context.Background(), testSession(), n, "etag1",
		notesync.PrevNote{RawBody: rawBody, ReplicaUUID: ourUUID})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != "OLD-NOTE" {
		t.Fatalf("in-place update must keep the note id, got %q", saved.ID)
	}
	if strings.Contains(ct.lastModify, `"operationType":"delete"`) {
		t.Errorf("in-place update must not delete anything:\nbody: %s", ct.lastModify)
	}
	for _, want := range []string{
		`"operationType":"update"`,
		`"recordName":"OLD-NOTE"`,
		`"recordChangeTag":"etag1"`,
	} {
		if !strings.Contains(ct.lastModify, want) {
			t.Errorf("modify body missing %q\nbody: %s", want, ct.lastModify)
		}
	}
}

// An in-place update of an image-bearing note must delete the previous Attachment and
// its backing Media (now orphaned by the rewritten body) in the same batch — no leak.
func TestPushNoteInPlaceUpdateCleansOldImages(t *testing.T) {
	ct := &captureTransport{}
	p := New(&http.Client{Transport: ct}, 1)
	rawBody, err := encodeMergeableNoteBody("T", nil, "old", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := notesync.Note{ID: "OLD-NOTE", FolderID: "F1", Title: "T", BodyHTML: "new step"}
	_, err = p.PushNote(context.Background(), testSession(), n, "etag1",
		notesync.PrevNote{RawBody: rawBody, ReplicaUUID: make([]byte, 16), AttachmentIDs: []string{"ATT-1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"operationType":"update"`,      // the note itself
		`"recordName":"OLD-NOTE"`,       // ...in place
		`"operationType":"forceDelete"`, // old children removed (no etag needed)
		`"recordName":"ATT-1"`,          // old attachment
		`"recordName":"MEDIA-ATT-1"`,    // its backing media
		`"recordType":"Media"`,
	} {
		if !strings.Contains(ct.lastModify, want) {
			t.Errorf("modify body missing %q\nbody: %s", want, ct.lastModify)
		}
	}
}

// A linked note whose version vector can't be read falls back to soft-delete
// (update Deleted=1) + recreate — never a hard delete (which its child attachments
// would block).
func TestPushNoteSoftDeleteFallback(t *testing.T) {
	ct := &captureTransport{}
	p := New(&http.Client{Transport: ct}, 1)
	n := notesync.Note{ID: "OLD-NOTE", FolderID: "F1", Title: "T", BodyHTML: "step"}
	saved, err := p.PushNote(context.Background(), testSession(), n, "etag1",
		notesync.PrevNote{RawBody: []byte("not a note body"), ReplicaUUID: make([]byte, 16)})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "OLD-NOTE" || saved.ID == "" {
		t.Fatalf("fallback must recreate under a fresh id, got %q", saved.ID)
	}
	if strings.Contains(ct.lastModify, `"operationType":"delete"`) {
		t.Errorf("fallback must soft-delete (update Deleted=1), not hard delete:\nbody: %s", ct.lastModify)
	}
	for _, want := range []string{
		`"operationType":"update"`, // the soft-delete of the old note
		`"recordName":"OLD-NOTE"`,
		`"Deleted"`,
		`"operationType":"create"`, // the fresh note
	} {
		if !strings.Contains(ct.lastModify, want) {
			t.Errorf("modify body missing %q\nbody: %s", want, ct.lastModify)
		}
	}
}

// A new (unlinked) note is a single create — no update, no delete.
func TestPushNoteCreate(t *testing.T) {
	ct := &captureTransport{}
	p := New(&http.Client{Transport: ct}, 1)
	n := notesync.Note{FolderID: "F1", Title: "T", BodyHTML: "step"} // no ID
	if _, err := p.PushNote(context.Background(), testSession(), n, "", notesync.PrevNote{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct.lastModify, `"operationType":"delete"`) ||
		strings.Contains(ct.lastModify, `"operationType":"update"`) {
		t.Errorf("create must be a single create op:\nbody: %s", ct.lastModify)
	}
	if !strings.Contains(ct.lastModify, `"operationType":"create"`) {
		t.Errorf("create op missing:\nbody: %s", ct.lastModify)
	}
}

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

// A concurrent-edit (etag) conflict on the in-place update must map to
// notesync.ErrEtagConflict (so the engine records a resolvable conflict and keeps
// syncing the other recipes) rather than a generic error that aborts the run.
func TestPushNoteAtomicConflictMapsToErrEtagConflict(t *testing.T) {
	p := New(&http.Client{Transport: conflictTransport{}}, 1)
	rawBody, err := encodeMergeableNoteBody("Борщ", nil, "Варить", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := notesync.Note{ID: "OLD", FolderID: "F1", Title: "Борщ", BodyHTML: "Варить"}
	_, err = p.PushNote(context.Background(), testSession(), n, "etag1",
		notesync.PrevNote{RawBody: rawBody, ReplicaUUID: make([]byte, 16)})
	if !errors.Is(err, notesync.ErrEtagConflict) {
		t.Fatalf("got %v, want notesync.ErrEtagConflict", err)
	}
}
