package icloud

import (
	"context"
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

	p := New(ts.Client())
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

// stubTransport returns canned CloudKit responses and records whether a
// records/modify (create) request was made.
type stubTransport struct {
	folderQueryJSON string
	modifyCalled    bool
}

func (t *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := "{}"
	switch {
	case strings.Contains(req.URL.Path, "records/query"):
		body = t.folderQueryJSON
	case strings.Contains(req.URL.Path, "records/modify"):
		t.modifyCalled = true
		body = `{"records":[{"recordName":"NEW","recordType":"Folder","fields":{"title":{"value":"Десерты","type":"STRING"}}}]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func testSession() *Session {
	return &Session{
		DSID:        "1",
		WebServices: map[string]string{"ckdatabasews": "https://ck.example"},
	}
}

func TestEnsureFolderReusesExisting(t *testing.T) {
	// A folder "Десерты" already exists as a direct child of ROOT.
	st := &stubTransport{
		folderQueryJSON: `{"records":[
			{"recordName":"F1","recordType":"Folder","fields":{
				"title":{"value":"Десерты","type":"STRING"},
				"parent":{"value":{"recordName":"ROOT"},"type":"REFERENCE"}
			}}
		]}`,
	}
	p := New(&http.Client{Transport: st})

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
	st := &stubTransport{folderQueryJSON: `{"records":[]}`}
	p := New(&http.Client{Transport: st})

	if _, err := p.EnsureFolder(context.Background(), testSession(), "ROOT", "Десерты"); err != nil {
		t.Fatal(err)
	}
	if !st.modifyCalled {
		t.Fatal("EnsureFolder should have created the folder when absent")
	}
}
