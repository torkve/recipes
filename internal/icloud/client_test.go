package icloud

import (
	"context"
	"encoding/base64"
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
