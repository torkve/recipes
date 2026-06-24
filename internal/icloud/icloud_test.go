package icloud

import (
	"encoding/base64"
	"strings"
	"testing"

	"recipes/internal/notesync"
)

func TestNotesRecordDecoding(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString
	title := b64([]byte("Борщ"))
	snip := b64([]byte("Готовим борщ."))
	fname := b64([]byte("Супы"))
	body := []byte(`{"records":[
		{"recordName":"F1","recordType":"Folder","fields":{"TitleEncrypted":{"value":"` + fname + `","type":"ENCRYPTED_BYTES"}}},
		{"recordName":"N1","recordType":"Note","recordChangeTag":"tag9","fields":{
			"TitleEncrypted":{"value":"` + title + `","type":"ENCRYPTED_BYTES"},
			"SnippetEncrypted":{"value":"` + snip + `","type":"ENCRYPTED_BYTES"},
			"Folders":{"value":{"recordName":"F1"},"type":"REFERENCE"}
		}}
	]}`)
	recs, err := parseRecords(body)
	if err != nil {
		t.Fatal(err)
	}
	if f := recordToFolder(recs[0]); f.Name != "Супы" || f.ID != "F1" {
		t.Fatalf("folder decode: %+v", f)
	}
	n := recordToNote(recs[1])
	if n.Title != "Борщ" {
		t.Fatalf("note title: %q", n.Title)
	}
	if n.BodyHTML != "Готовим борщ." {
		t.Fatalf("note body: %q", n.BodyHTML)
	}
	if n.FolderID != "F1" || n.Etag != "tag9" {
		t.Fatalf("note ref/etag: %+v", n)
	}
}

func TestParseAccountLogin(t *testing.T) {
	body := []byte(`{
		"dsInfo": {"dsid": "12345"},
		"webservices": {
			"ckdatabasews": {"url": "https://p01-ckdatabasews.icloud.com:443", "status": "active"},
			"drivews": {"url": "https://p01-drivews.icloud.com:443"}
		}
	}`)
	dsid, services, err := parseAccountLogin(body)
	if err != nil {
		t.Fatal(err)
	}
	if dsid != "12345" {
		t.Fatalf("dsid=%q", dsid)
	}
	if services["ckdatabasews"] == "" {
		t.Fatalf("missing ckdatabasews url: %+v", services)
	}
}

func TestParseAccountLoginMissingDSID(t *testing.T) {
	if _, _, err := parseAccountLogin([]byte(`{"webservices":{}}`)); err == nil {
		t.Fatal("expected error for missing dsid")
	}
}

func TestParseZoneChanges(t *testing.T) {
	body := []byte(`{"zones":[{"zoneID":{"zoneName":"Notes"},"syncToken":"TOK","moreComing":true,"records":[
		{"recordName":"F1","recordType":"Folder"},
		{"recordName":"N1","recordType":"Note"}
	]}]}`)
	recs, tok, more, err := parseZoneChanges(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || tok != "TOK" || !more {
		t.Fatalf("got recs=%d tok=%q more=%v", len(recs), tok, more)
	}
}

func TestZoneChangesBody(t *testing.T) {
	b, _ := zoneChangesBody("", notesDesiredKeys, notesDesiredRecordTypes)
	s := string(b)
	if !strings.Contains(s, `"zoneName":"Notes"`) || !strings.Contains(s, `"reverse":true`) {
		t.Fatalf("missing zone/reverse: %s", s)
	}
	if strings.Contains(s, "syncToken") {
		t.Fatalf("empty token should be omitted: %s", s)
	}
	b2, _ := zoneChangesBody("TOK", notesDesiredKeys, notesDesiredRecordTypes)
	if !strings.Contains(string(b2), `"syncToken":"TOK"`) {
		t.Fatalf("sync token not included: %s", b2)
	}
}

func TestRecordToFolderParent(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("Супы"))
	body := []byte(`{"records":[{"recordName":"C","recordType":"Folder","fields":{
		"TitleEncrypted":{"value":"` + enc + `","type":"ENCRYPTED_BYTES"},
		"ParentFolder":{"value":{"recordName":"P"},"type":"REFERENCE"}
	}}]}`)
	recs, err := parseRecords(body)
	if err != nil {
		t.Fatal(err)
	}
	f := recordToFolder(recs[0])
	if f.Name != "Супы" || f.ParentID != "P" {
		t.Fatalf("bad folder parent: %+v", f)
	}
}

func TestParseRecordsEnvelope(t *testing.T) {
	// Records with a server error are skipped; clean ones are returned.
	body := []byte(`{"records":[
		{"recordName":"F1","recordType":"Folder","fields":{}},
		{"recordName":"N1","recordType":"Note","fields":{}}
	]}`)
	recs, err := parseRecords(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
}

func TestParseRecordsConflict(t *testing.T) {
	body := []byte(`{"records":[{"recordName":"N1","serverErrorCode":"CONFLICT","reason":"etag"}]}`)
	if _, err := parseRecords(body); err != errEtagConflict {
		t.Fatalf("expected errEtagConflict, got %v", err)
	}
}

func TestDescendantsOf(t *testing.T) {
	all := []notesync.Folder{
		{ID: "root", ParentID: ""},
		{ID: "a", ParentID: "root", Name: "Супы"},
		{ID: "b", ParentID: "a", Name: "Холодные"},
		{ID: "x", ParentID: "other", Name: "Unrelated"},
	}
	got := descendantsOf(all, "root")
	ids := map[notesync.FolderID]notesync.FolderID{}
	for _, f := range got {
		ids[f.ID] = f.ParentID
	}
	if _, ok := ids["x"]; ok {
		t.Fatal("unrelated folder should be excluded")
	}
	if ids["a"] != "" {
		t.Fatalf("direct child should be reparented to root scope, got %q", ids["a"])
	}
	if ids["b"] != "a" {
		t.Fatalf("grandchild parent should be preserved, got %q", ids["b"])
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := &Session{AppleID: "a@b.com", DSID: "1", WebServices: map[string]string{"ckdatabasews": "https://x"}}
	blob, err := s.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := parseSession(blob)
	if err != nil {
		t.Fatal(err)
	}
	if s2.AppleID != "a@b.com" || s2.ckDatabaseURL() != "https://x" {
		t.Fatalf("round trip lost data: %+v", s2)
	}
}

func TestNoteToRecordRoundTrip(t *testing.T) {
	n := notesync.Note{ID: "N1", FolderID: "F1", Title: "Борщ",
		BodyHTML: "Варить", Checklists: [][]string{{"свёкла"}}}
	rec := noteToRecord(n)
	if rec.RecordName != "N1" || rec.stringField("title") != "Борщ" {
		t.Fatalf("bad record: %+v", rec)
	}
	if rec.referenceField("Folders") != "F1" {
		t.Fatalf("bad folder ref: %+v", rec.Fields["Folders"])
	}
}
