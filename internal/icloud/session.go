// Package icloud is a Go client for Apple's private iCloud web services
// (CloudKit), modeled on the reverse-engineered flow used by icloud.js /
// ElyaConrad's iCloud-API: Apple ID sign-in via idmsa.apple.com/appleauth
// (with HSA2 two-factor + a trust token), then setup.icloud.com accountLogin to
// obtain CloudKit tokens and per-service web-service URLs, then ckdatabasews
// CloudKit record query/modify against the Notes zone.
//
// Apple publishes no official Notes API; these endpoints and record formats are
// undocumented and may change without notice. This client cannot be exercised
// without real credentials and Apple's live servers, so it is shipped behind a
// feature flag and its pure request-building / response-parsing helpers are the
// only parts covered by tests. It implements notesync.SyncProvider and
// notesync.Binder.
package icloud

import (
	"encoding/json"
	"time"
)

// Session is the persisted, serializable authentication state. It is what
// notesync stores (encrypted) in icloud_accounts.session_blob.
type Session struct {
	AppleID string `json:"apple_id"`

	// Cookies captured from the iCloud web-auth flow, replayed on every request.
	Cookies []SavedCookie `json:"cookies"`

	// Tokens / identifiers from accountLogin.
	SessionToken   string `json:"session_token"`   // X-APPLE-WEBAUTH-TOKEN
	TrustToken     string `json:"trust_token"`     // skips 2FA on refresh
	AccountCountry string `json:"account_country"` // X-APPLE-ID-ACCOUNT-COUNTRY
	DSID           string `json:"dsid"`

	// CloudKit access.
	WebServices map[string]string `json:"web_services"` // service name -> base URL
	ClientID    string            `json:"client_id"`    // stable per-session UUID for CloudKit/setup calls

	ExpiresAt time.Time `json:"expires_at"`
}

// SavedCookie is a JSON-serializable subset of http.Cookie.
type SavedCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

// Bytes serializes the session for encrypted storage.
func (s *Session) Bytes() ([]byte, error) { return json.Marshal(s) }

// Expired reports whether the session's CloudKit token window has elapsed.
// A zero ExpiresAt is treated as not-yet-known (not expired).
func (s *Session) Expired() bool {
	return !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt)
}

// ckDatabaseURL returns the CloudKit database web-service base URL, if known.
func (s *Session) ckDatabaseURL() string { return s.WebServices["ckdatabasews"] }

// parseSession deserializes a stored session blob.
func parseSession(blob []byte) (*Session, error) {
	var s Session
	if err := json.Unmarshal(blob, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
