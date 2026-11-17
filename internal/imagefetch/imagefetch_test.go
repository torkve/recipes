package imagefetch

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestIsPublic(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	for _, s := range public {
		if !isPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s classified as non-public", s)
		}
	}
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "100.64.0.1",
		"224.0.0.1", "255.255.255.255", "198.18.0.1", "::1", "::", "fe80::1", "fc00::1", "fd12::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::7f00:1", "2002:7f00:1::", "2001:0:4136:e378::1",
		"::7f00:1", "fec0::1",
	}
	for _, s := range blocked {
		if isPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s classified as public", s)
		}
	}
}

func TestGuardDial(t *testing.T) {
	if err := guardDial("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("public:443 blocked: %v", err)
	}
	for _, addr := range []string{"8.8.8.8:80", "127.0.0.1:443", "[::1]:443", "169.254.169.254:443", "garbage"} {
		if err := guardDial("tcp", addr, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: err = %v, want ErrBlocked", addr, err)
		}
	}
}

func TestRejectsNonHTTPSAndLoopback(t *testing.T) {
	f := New(1 << 20)
	ctx := context.Background()
	for _, u := range []string{"http://example.com/a.png", "ftp://example.com/a.png", "https://example.com:8443/a.png",
		"https://user@example.com/a.png", "file:///etc/passwd", "/relative.png", ""} {
		if _, err := f.Get(ctx, u); !errors.Is(err, ErrBadURL) {
			t.Errorf("%q: err = %v, want ErrBadURL", u, err)
		}
	}
	// The real transport refuses loopback even over https on 443.
	if _, err := f.Get(ctx, "https://127.0.0.1/a.png"); !errors.Is(err, ErrBlocked) {
		t.Errorf("loopback: err = %v, want ErrBlocked", err)
	}
	if _, err := f.Get(ctx, "https://localhost/a.png"); !errors.Is(err, ErrBlocked) {
		t.Errorf("localhost: err = %v, want ErrBlocked", err)
	}
}

func TestDataURI(t *testing.T) {
	f := New(16)
	ctx := context.Background()
	payload := []byte("\x89PNG\r\n\x1a\nabc")
	got, err := f.Get(ctx, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(payload))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("data URI = %q, %v", got, err)
	}
	if _, err := f.Get(ctx, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(make([]byte, 64))); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized data URI: err = %v", err)
	}
	for _, u := range []string{"data:text/html;base64,PGI+", "data:image/png,rawtext", "data:image/png;base64,!!!"} {
		if _, err := f.Get(ctx, u); !errors.Is(err, ErrBadURL) {
			t.Errorf("%q: err = %v, want ErrBadURL", u, err)
		}
	}
}

// TestDownload exercises the download path through a TLS test server, with a
// transport that skips the address guard (the test server is on loopback).
func TestDownload(t *testing.T) {
	body := strings.Repeat("x", 100)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(body))
		case "/missing":
			http.NotFound(w, r)
		case "/redirect-http":
			http.Redirect(w, r, "http://example.com/a.png", http.StatusFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	host := strings.TrimPrefix(srv.URL, "https://")

	// allowedURL only accepts port 443, so rewrite requests to the test port.
	client := srv.Client()
	inner := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Host = host
		return inner.RoundTrip(r)
	})

	f := newWithClient(1000, client)
	got, err := f.Get(ctx, "https://images.example/ok")
	if err != nil || string(got) != body {
		t.Fatalf("download = %d bytes, %v", len(got), err)
	}
	if _, err := newWithClient(10, client).Get(ctx, "https://images.example/ok"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized download: err = %v", err)
	}
	if _, err := f.Get(ctx, "https://images.example/missing"); !errors.Is(err, ErrDownload) || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: err = %v", err)
	}
	// Network errors are reported without their details.
	if _, err := New(1000).Get(ctx, "https://no-such-host.invalid/a.png"); err != ErrDownload {
		t.Errorf("unresolvable host: err = %v, want bare ErrDownload", err)
	}
	if _, err := f.Get(ctx, "https://images.example/redirect-http"); !errors.Is(err, ErrBlocked) {
		t.Errorf("redirect to http: err = %v, want ErrBlocked", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
