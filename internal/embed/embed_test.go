package embed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewDisabledWhenNoURL(t *testing.T) {
	if New("", "m", 3) != nil {
		t.Fatal("New(\"\") should return nil (semantic disabled)")
	}
}

func TestEmbedQueryAndPassagesPrefixes(t *testing.T) {
	var gotInputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" {
			t.Errorf("path = %s, want /embed", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Inputs []string `json:"inputs"`
		}
		_ = json.Unmarshal(body, &req)
		gotInputs = req.Inputs
		out := make([][]float32, len(req.Inputs))
		for i := range out {
			out[i] = []float32{1, 0, 0}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	c := New(srv.URL, "e5", 3)
	if c == nil {
		t.Fatal("client nil")
	}
	if c.Model() != "e5" || c.Dim() != 3 {
		t.Fatalf("model/dim = %q/%d", c.Model(), c.Dim())
	}

	v, err := c.EmbedQuery(context.Background(), "блины")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 3 {
		t.Fatalf("query vec dim = %d", len(v))
	}
	if len(gotInputs) != 1 || !strings.HasPrefix(gotInputs[0], "query: ") {
		t.Fatalf("query prefix missing: %v", gotInputs)
	}

	if _, err := c.EmbedPassages(context.Background(), []string{"борщ", "оладьи"}); err != nil {
		t.Fatal(err)
	}
	if len(gotInputs) != 2 || !strings.HasPrefix(gotInputs[0], "passage: ") || !strings.HasPrefix(gotInputs[1], "passage: ") {
		t.Fatalf("passage prefixes missing: %v", gotInputs)
	}
}

func TestEmbedDimMismatchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([][]float32{{1, 0}}) // dim 2, client expects 3
	}))
	defer srv.Close()
	c := New(srv.URL, "e5", 3)
	if _, err := c.EmbedQuery(context.Background(), "x"); err == nil {
		t.Fatal("expected dim-mismatch error")
	}
}
