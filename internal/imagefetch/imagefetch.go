// Package imagefetch obtains image bytes from a URL supplied by an MCP client:
// a data: URI, or an https URL downloaded with protection against server-side
// request forgery.
//
// Downloads go only to public IP addresses on port 443. The address check runs
// on every connection after DNS resolution (so DNS rebinding cannot evade it),
// redirects are re-checked, environment proxies are ignored, and size and time
// are bounded.
package imagefetch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Errors returned by Fetcher.Get.
var (
	ErrBadURL   = errors.New("imagefetch: only https:// URLs on the default port and data:image/...;base64 URIs are accepted")
	ErrTooLarge = errors.New("imagefetch: image too large")
	ErrBlocked  = errors.New("imagefetch: address not allowed")
	ErrDownload = errors.New("imagefetch: download failed")
)

const (
	timeout      = 20 * time.Second
	maxRedirects = 3
)

// Fetcher downloads images up to MaxBytes.
type Fetcher struct {
	MaxBytes int
	client   *http.Client
}

// New returns a Fetcher that only reaches public addresses.
func New(maxBytes int) *Fetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: guardDial}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return newWithClient(maxBytes, &http.Client{Transport: transport})
}

// newWithClient returns a Fetcher using client's transport, with this
// package's timeout and redirect policy.
func newWithClient(maxBytes int, client *http.Client) *Fetcher {
	c := *client
	c.Timeout = timeout
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("imagefetch: too many redirects")
		}
		if !allowedURL(req.URL) {
			return ErrBadURL
		}
		return nil
	}
	return &Fetcher{MaxBytes: maxBytes, client: &c}
}

// Get returns the bytes of the image at raw: a data: URI or an https URL.
// It does not check that the bytes are an image.
func (f *Fetcher) Get(ctx context.Context, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		return f.decodeDataURI(raw)
	}
	u, err := url.Parse(raw)
	if err != nil || !allowedURL(u) {
		return nil, ErrBadURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrBadURL
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "recipes-image-fetch/1.0")
	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlocked) || errors.Is(err, ErrBadURL) {
			return nil, ErrBlocked
		}
		// Network error details (resolver addresses, which internal names
		// exist) are logged, not returned to the client.
		log.Printf("imagefetch: get %s: %v", u.Redacted(), err)
		return nil, ErrDownload
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrDownload, resp.StatusCode)
	}
	if resp.ContentLength > int64(f.MaxBytes) {
		return nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(f.MaxBytes)+1))
	if err != nil {
		log.Printf("imagefetch: read %s: %v", u.Redacted(), err)
		return nil, ErrDownload
	}
	if len(data) > f.MaxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// decodeDataURI decodes a base64 data:image/... URI.
func (f *Fetcher) decodeDataURI(raw string) ([]byte, error) {
	meta, payload, ok := strings.Cut(raw[len("data:"):], ",")
	if !ok || !strings.HasPrefix(strings.ToLower(meta), "image/") || !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return nil, ErrBadURL
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > f.MaxBytes+3 {
		return nil, ErrTooLarge
	}
	payload = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, payload)
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if data, err = base64.RawStdEncoding.DecodeString(payload); err != nil {
			return nil, fmt.Errorf("%w: invalid base64", ErrBadURL)
		}
	}
	if len(data) > f.MaxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// allowedURL reports whether u is an https URL on the default port without
// userinfo.
func allowedURL(u *url.URL) bool {
	return u.Scheme == "https" && u.Host != "" && u.User == nil && (u.Port() == "" || u.Port() == "443")
}

// guardDial rejects connections to anything but a public IP on port 443.
func guardDial(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return ErrBlocked
	}
	if ap.Port() != 443 || !isPublic(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrBlocked, address)
	}
	return nil
}

// nonPublic lists special-purpose ranges not covered by netip.Addr methods.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 (may reach internal IPv4)
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4 (embeds an IPv4 address)
	netip.MustParsePrefix("2001::/32"),       // Teredo (embeds an IPv4 address)
	netip.MustParsePrefix("::/96"),           // IPv4-compatible (embeds an IPv4 address)
	netip.MustParsePrefix("fec0::/10"),       // deprecated site-local
}

// isPublic reports whether a is a globally routable unicast address.
func isPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() ||
		a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
