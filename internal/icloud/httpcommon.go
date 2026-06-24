package icloud

import (
	"log"
	"net/http"
	"strings"
)

// browserUA identifies the client as Safari on macOS. Apple's idmsa / iCloud web
// services reject or 503 the default Go user-agent; presenting a real browser
// user-agent (as the iCloud web app does) is required for the flow to work.
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Safari/605.1.15"

// fdClientInfo returns Apple's fraud-detection client-info header value. The
// web app computes a device fingerprint in "F"; a headless client sends an empty
// fingerprint, which Apple accepts for the password/2FA flow.
func fdClientInfo() string {
	return `{"U":"` + browserUA + `","L":"en_US","Z":"GMT+00:00","V":"1.1","F":""}`
}

// setBrowserHeaders adds browser-like headers that Apple expects, without
// overwriting any explicitly-set values.
func setBrowserHeaders(req *http.Request) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", browserUA)
	}
	if req.Header.Get("Accept-Language") == "" {
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	}
}

// stripQuery removes the query string from a URL for logging, so secrets passed
// as query parameters (dsid, tokens) are never written to logs.
func stripQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// logRequest logs an outgoing request (method + URL without query). Request
// bodies are never logged — they may contain the Apple ID password.
func logRequest(method, urlStr string) {
	log.Printf("icloud: → %s %s", method, stripQuery(urlStr))
}

// logResponse logs the response status. On a 4xx/5xx it also logs a truncated
// response body (Apple's error payloads carry no credentials of ours).
func logResponse(method, urlStr string, status int, body []byte) {
	if status >= 400 {
		log.Printf("icloud: ← %d %s %s", status, stripQuery(urlStr), truncate(body))
	} else {
		log.Printf("icloud: ← %d %s", status, stripQuery(urlStr))
	}
}
