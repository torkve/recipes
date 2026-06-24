package icloud

import (
	"testing"

	"recipes/internal/notesync"
)

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

func TestParseRecordsAndMapping(t *testing.T) {
	body := []byte(`{"records":[
		{"recordName":"F1","recordType":"Folder","fields":{"title":{"value":"Десерты","type":"STRING"}}},
		{"recordName":"N1","recordType":"Note","recordChangeTag":"tag1","fields":{
			"title":{"value":"Шарлотка","type":"STRING"},
			"snippet":{"value":"Печь 30 минут","type":"STRING"},
			"Folders":{"value":{"recordName":"F1"},"type":"REFERENCE"}
		}}
	]}`)
	recs, err := parseRecords(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records", len(recs))
	}

	f := recordToFolder(recs[0])
	if f.ID != "F1" || f.Name != "Десерты" {
		t.Fatalf("bad folder: %+v", f)
	}

	n := recordToNote(recs[1])
	if n.ID != "N1" || n.Title != "Шарлотка" || n.Etag != "tag1" {
		t.Fatalf("bad note: %+v", n)
	}
	if n.BodyHTML != "Печь 30 минут" {
		t.Fatalf("bad body: %q", n.BodyHTML)
	}
	if n.FolderID != "F1" {
		t.Fatalf("bad folder ref: %q", n.FolderID)
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
