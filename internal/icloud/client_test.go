package icloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestsIdentifyAsBrowser(t *testing.T) {
	var gotUA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("{}"))
	}))
	defer ts.Close()

	p := New(ts.Client(), 1)
	if _, _, err := p.do(context.Background(), http.MethodGet, ts.URL, nil, nil, &Session{}); err != nil {
		t.Fatal(err)
	}
	if gotUA != browserUA {
		t.Fatalf("User-Agent = %q, want browser UA", gotUA)
	}
	if strings.Contains(gotUA, "Go-http-client") {
		t.Fatal("requests still identify as the Go default client")
	}
}

// stubTransport serves canned CloudKit responses: successive changes/zone pages,
// and records whether a records/modify (create) request was made.
type stubTransport struct {
	zonePages    []string
	zoneIdx      int
	zoneCalls    int
	modifyCalled bool
	lastURL      string

	lookupRecs  map[string]string // recordName -> canned record JSON
	lookupCalls int
	lookupErr   bool // when true, records/lookup returns HTTP 500
}

func (t *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.lastURL = req.URL.String()
	body := "{}"
	switch {
	case strings.Contains(req.URL.Path, "changes/zone"):
		t.zoneCalls++
		if len(t.zonePages) == 0 {
			body = emptyZonePage(false)
		} else {
			i := t.zoneIdx
			if i >= len(t.zonePages) {
				i = len(t.zonePages) - 1
			}
			body = t.zonePages[i]
			t.zoneIdx++
		}
	case strings.Contains(req.URL.Path, "records/lookup"):
		t.lookupCalls++
		if t.lookupErr {
			return &http.Response{StatusCode: http.StatusInternalServerError,
				Body: io.NopCloser(strings.NewReader(`{"error":"boom"}`)), Header: make(http.Header)}, nil
		}
		body = t.lookupResponse(req)
	case strings.Contains(req.URL.Path, "records/modify"):
		t.modifyCalled = true
		body = `{"records":[{"recordName":"NEW","recordType":"Folder","fields":{}}]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

// lookupResponse returns the canned records for the requested names, reporting a
// serverErrorCode for any unknown name (to exercise tolerant parsing).
func (t *stubTransport) lookupResponse(req *http.Request) string {
	var reqBody struct {
		Records []struct {
			RecordName string `json:"recordName"`
		} `json:"records"`
	}
	raw, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(raw, &reqBody)
	var parts []string
	for _, r := range reqBody.Records {
		if rec, ok := t.lookupRecs[r.RecordName]; ok {
			parts = append(parts, rec)
		} else {
			parts = append(parts, fmt.Sprintf(`{"recordName":%q,"serverErrorCode":"NOT_FOUND"}`, r.RecordName))
		}
	}
	return `{"records":[` + strings.Join(parts, ",") + `]}`
}

func attachmentRec(name, mediaName string) string {
	return fmt.Sprintf(`{"recordName":%q,"recordType":"Attachment","fields":{`+
		`"Media":{"value":{"recordName":%q},"type":"REFERENCE"}}}`, name, mediaName)
}

func mediaRec(name, url string) string {
	return fmt.Sprintf(`{"recordName":%q,"recordType":"Media","fields":{`+
		`"Asset":{"value":{"downloadURL":%q,"size":3},"type":"ASSETID"}}}`, name, url)
}

// notePageImage is a changes/zone page with one Note whose body has an inline
// image attachment (att) in a step paragraph.
func notePageImage(recordName, title, att string) string {
	return notePageAttach(recordName, title, att, "public.jpeg")
}

// notePageAttach builds a changes/zone Note page with one body attachment of the
// given type_uti (e.g. public.jpeg or com.apple.notes.gallery).
func notePageAttach(recordName, title, att, uti string) string {
	text := title + "\nШаг ￼ тут\n"
	runs := []noteRun{
		{length: len([]rune(title + "\n")), styleType: 0},
		{length: len([]rune("Шаг ")), styleType: -1},
		{length: 1, styleType: -1, attachID: att, attachUTI: uti},
		{length: len([]rune(" тут\n")), styleType: -1},
	}
	blob := base64.StdEncoding.EncodeToString(buildNoteBlob(text, runs))
	encTitle := base64.StdEncoding.EncodeToString([]byte(title))
	return fmt.Sprintf(`{"zones":[{"records":[{"recordName":%q,"recordType":"Note","fields":{`+
		`"TitleEncrypted":{"value":%q,"type":"ENCRYPTED_BYTES"},`+
		`"TextDataEncrypted":{"value":%q,"type":"ENCRYPTED_BYTES"}}}],"syncToken":"t","moreComing":false}]}`,
		recordName, encTitle, blob)
}

// galleryRec is a scanned-document gallery Attachment whose MergeableDataEncrypted
// embeds the given candidate page ids (as ASCII UUIDs).
func galleryRec(name string, pageIDs ...string) string {
	blob := "crdt"
	for _, id := range pageIDs {
		blob += "\x00" + id
	}
	mb := base64.StdEncoding.EncodeToString([]byte(blob))
	return fmt.Sprintf(`{"recordName":%q,"recordType":"Attachment","fields":{`+
		`"UTI":{"value":"com.apple.notes.gallery","type":"STRING"},`+
		`"MergeableDataEncrypted":{"value":%q,"type":"ENCRYPTED_BYTES"}}}`, name, mb)
}

func emptyZonePage(more bool) string {
	return fmt.Sprintf(`{"zones":[{"records":[],"syncToken":"t","moreComing":%v}]}`, more)
}

// folderPage is a single changes/zone page with one Folder record.
func folderPage(recordName, name, parent string, more bool) string {
	enc := base64.StdEncoding.EncodeToString([]byte(name))
	return fmt.Sprintf(`{"zones":[{"records":[{"recordName":"%s","recordType":"Folder","fields":{`+
		`"TitleEncrypted":{"value":"%s","type":"ENCRYPTED_BYTES"},`+
		`"ParentFolder":{"value":{"recordName":"%s"},"type":"REFERENCE"}}}],"syncToken":"t","moreComing":%v}]}`,
		recordName, enc, parent, more)
}

func testSession() *Session {
	return &Session{DSID: "1", ClientID: "CID", WebServices: map[string]string{"ckdatabasews": "https://ck.example"}}
}

func TestEnsureFolderReusesExisting(t *testing.T) {
	// A folder "Десерты" already exists as a direct child of ROOT.
	st := &stubTransport{zonePages: []string{folderPage("F1", "Десерты", "ROOT", false)}}
	p := New(&http.Client{Transport: st}, 1)

	f, err := p.EnsureFolder(context.Background(), testSession(), "ROOT", "Десерты")
	if err != nil {
		t.Fatal(err)
	}
	if st.modifyCalled {
		t.Fatal("EnsureFolder created a duplicate folder instead of reusing the existing one")
	}
	if f.ID != "F1" || f.Name != "Десерты" {
		t.Fatalf("expected existing folder F1, got %+v", f)
	}
}

func TestEnsureFolderCreatesWhenAbsent(t *testing.T) {
	st := &stubTransport{zonePages: []string{emptyZonePage(false)}}
	p := New(&http.Client{Transport: st}, 1)

	if _, err := p.EnsureFolder(context.Background(), testSession(), "ROOT", "Десерты"); err != nil {
		t.Fatal(err)
	}
	if !st.modifyCalled {
		t.Fatal("EnsureFolder should have created the folder when absent")
	}
}

func TestListFoldersUsesChangesZoneWithParams(t *testing.T) {
	st := &stubTransport{zonePages: []string{emptyZonePage(false)}}
	p := New(&http.Client{Transport: st}, 1)

	if _, err := p.ListFolders(context.Background(), testSession(), "ROOT"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.lastURL, "changes/zone") {
		t.Fatalf("expected changes/zone, got %s", st.lastURL)
	}
	for _, want := range []string{"ckjsVersion=2.6.4", "clientId=CID", "dsid=1", "ckjsBuildVersion=", "clientBuildNumber="} {
		if !strings.Contains(st.lastURL, want) {
			t.Fatalf("CloudKit query missing %q: %s", want, st.lastURL)
		}
	}
}

func TestParseLookupSkipsErroredRecords(t *testing.T) {
	body := `{"records":[` +
		`{"recordName":"OK","recordType":"Media","fields":{}},` +
		`{"recordName":"GONE","serverErrorCode":"NOT_FOUND"}]}`
	recs, err := parseLookup([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].RecordName != "OK" {
		t.Fatalf("expected only the OK record, got %+v", recs)
	}
}

func TestResolveAttachmentURLs(t *testing.T) {
	st := &stubTransport{lookupRecs: map[string]string{
		"ATT1": attachmentRec("ATT1", "MED1"),
		"MED1": mediaRec("MED1", "https://cvws.example/img1"),
	}}
	p := New(&http.Client{Transport: st}, 1)

	pages, pageURL := p.resolveAttachmentURLs(context.Background(), testSession(), []string{"ATT1", "MISSING"})
	if len(pages["ATT1"]) != 1 || pages["ATT1"][0] != "ATT1" {
		t.Fatalf("pages[ATT1]=%v want [ATT1]", pages["ATT1"])
	}
	if pageURL["ATT1"] != "https://cvws.example/img1" {
		t.Fatalf("pageURL[ATT1] = %q, want the media asset url", pageURL["ATT1"])
	}
	if _, ok := pages["MISSING"]; ok {
		t.Fatal("unresolved attachment should be absent from the map")
	}
	if st.lookupCalls != 2 {
		t.Fatalf("expected 2 lookup rounds (attachments, media), got %d", st.lookupCalls)
	}
}

func TestFetchZoneResolvesInlineImage(t *testing.T) {
	st := &stubTransport{
		zonePages:  []string{notePageImage("N1", "Пирог", "ATT1")},
		lookupRecs: map[string]string{"ATT1": attachmentRec("ATT1", "MED1"), "MED1": mediaRec("MED1", "https://cvws.example/img1")},
	}
	p := New(&http.Client{Transport: st}, 1)

	_, notes, _, err := p.FetchZone(context.Background(), testSession(), "ROOT", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || len(notes[0].Images) != 1 {
		t.Fatalf("expected one note with one image, got %+v", notes)
	}
	if got := notes[0].Images[0]; got.ID != "ATT1" || got.Ref != "https://cvws.example/img1" {
		t.Fatalf("image not resolved: %+v", got)
	}
}

func TestFetchZoneExpandsScannedGallery(t *testing.T) {
	// The note's body attachment is a gallery; its MergeableData lists one real
	// page (PAGE1) plus a noise UUID that resolves to nothing and must be dropped.
	// Page ids are extracted from the gallery's mergeable blob by UUID regex, so
	// they must be real UUIDs; the gallery/media record names need not be.
	page := "11111111-1111-1111-1111-111111111111"
	noise := "99999999-0000-0000-0000-000000000000"
	st := &stubTransport{
		zonePages: []string{notePageAttach("N1", "Скан", "GAL1", "com.apple.notes.gallery")},
		lookupRecs: map[string]string{
			"GAL1": galleryRec("GAL1", page, noise),
			page:   attachmentRec(page, "MED1"),
			"MED1": mediaRec("MED1", "https://cvws.example/scan1"),
		},
	}
	p := New(&http.Client{Transport: st}, 1)

	_, notes, _, err := p.FetchZone(context.Background(), testSession(), "ROOT", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %d", len(notes))
	}
	// The gallery fanned out to exactly its one resolvable page (noise dropped).
	if len(notes[0].Images) != 1 || notes[0].Images[0].ID != page ||
		notes[0].Images[0].Ref != "https://cvws.example/scan1" {
		t.Fatalf("gallery not expanded to its page: %+v", notes[0].Images)
	}
	// The body marker was rewritten from the gallery id to the page id.
	if !strings.Contains(notes[0].BodyHTML, "@@IMG:"+page+"@@") || strings.Contains(notes[0].BodyHTML, "GAL1") {
		t.Fatalf("body marker not rewritten to page: %q", notes[0].BodyHTML)
	}

	// Idempotent: a second resolve yields an identical body.
	body1 := notes[0].BodyHTML
	_, notes2, _, _ := p.FetchZone(context.Background(), testSession(), "ROOT", "")
	if notes2[0].BodyHTML != body1 {
		t.Fatalf("non-idempotent gallery resolve:\n %q\n %q", body1, notes2[0].BodyHTML)
	}
}

func TestFetchZoneToleratesImageLookupFailure(t *testing.T) {
	st := &stubTransport{
		zonePages: []string{notePageImage("N1", "Пирог", "ATT1")},
		lookupErr: true, // every records/lookup fails
	}
	p := New(&http.Client{Transport: st}, 1)

	_, notes, _, err := p.FetchZone(context.Background(), testSession(), "ROOT", "")
	if err != nil {
		t.Fatalf("a failed image lookup must not abort the pull: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("expected the note to still import, got %d notes", len(notes))
	}
	if len(notes[0].Images) != 0 {
		t.Fatalf("unresolved image should be dropped, got %+v", notes[0].Images)
	}
}

func TestZoneChangesPaginates(t *testing.T) {
	st := &stubTransport{zonePages: []string{
		folderPage("F1", "A", "ROOT", true),  // moreComing -> fetch next page
		folderPage("F2", "B", "ROOT", false), // last page
	}}
	p := New(&http.Client{Transport: st}, 1)

	folders, err := p.ListFolders(context.Background(), testSession(), "ROOT")
	if err != nil {
		t.Fatal(err)
	}
	if st.zoneCalls != 2 {
		t.Fatalf("expected 2 changes/zone calls, got %d", st.zoneCalls)
	}
	if len(folders) != 2 {
		t.Fatalf("expected 2 folders across pages, got %d", len(folders))
	}
}
