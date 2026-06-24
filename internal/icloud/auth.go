package icloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"

	"recipes/internal/notesync"
)

// idmsa header names carrying the in-flight auth session and resulting tokens.
const (
	hdrSessionID = "X-Apple-ID-Session-Id"
	hdrScnt      = "scnt"
	hdrSessTok   = "X-Apple-Session-Token"
	hdrTrustTok  = "X-Apple-TwoSV-Trust-Token"
	hdrAcctCty   = "X-Apple-ID-Account-Country"
)

// bindHandle is the opaque continuation between Begin and Complete.
type bindHandle struct {
	AppleID      string        `json:"apple_id"`
	SessionID    string        `json:"session_id"`
	Scnt         string        `json:"scnt"`
	SessionToken string        `json:"session_token"`
	Country      string        `json:"country"`
	Cookies      []SavedCookie `json:"cookies"`
}

func newJarClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func randState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "auth-" + hex.EncodeToString(b)
}

// Begin submits Apple ID + password. A 409 from idmsa means HSA2 2FA is needed.
func (p *Provider) Begin(ctx context.Context, appleID, password string) (notesync.BindResult, error) {
	body, err := buildSigninBody(appleID, password, nil)
	if err != nil {
		return notesync.BindResult{}, err
	}
	headers := authHeaders(randState())
	respBody, resp, err := p.rawDo(ctx, http.MethodPost, idmsaBase+"/signin?isRememberMeEnabled=true", headers, body)
	if err != nil {
		return notesync.BindResult{}, err
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		// Signed in without 2FA.
		sess, err := p.finishLogin(ctx, resp, "", saveCookies(resp))
		if err != nil {
			return notesync.BindResult{}, err
		}
		sess.AppleID = appleID
		return notesync.BindResult{Session: sess, Pending: false}, nil

	case http.StatusConflict, http.StatusPreconditionFailed:
		// HSA2 two-factor required; carry the auth session into Complete.
		h := bindHandle{
			AppleID:      appleID,
			SessionID:    resp.Header.Get(hdrSessionID),
			Scnt:         resp.Header.Get(hdrScnt),
			SessionToken: resp.Header.Get(hdrSessTok),
			Country:      resp.Header.Get(hdrAcctCty),
			Cookies:      saveCookies(resp),
		}
		raw, _ := json.Marshal(h)
		return notesync.BindResult{Pending: true, Handle: notesync.BindHandle(raw)}, nil

	default:
		return notesync.BindResult{}, fmt.Errorf("icloud: signin status %d: %s", resp.StatusCode, truncate(respBody))
	}
}

// Complete submits the 2FA code, trusts the device, and finishes login.
func (p *Provider) Complete(ctx context.Context, handle notesync.BindHandle, code string) (notesync.Session, error) {
	var h bindHandle
	if err := json.Unmarshal(handle, &h); err != nil {
		return nil, fmt.Errorf("icloud: bad bind handle: %w", err)
	}

	sessHeaders := map[string]string{
		hdrSessionID:              h.SessionID,
		hdrScnt:                   h.Scnt,
		"X-Apple-Widget-Key":      widgetKey,
		"X-Apple-OAuth-Client-Id": widgetKey,
	}

	// Verify the security code.
	codeBody, err := buildSecurityCodeBody(code)
	if err != nil {
		return nil, err
	}
	if _, resp, err := p.rawDo(ctx, http.MethodPost, idmsaBase+"/verify/trusteddevice/securitycode", sessHeaders, codeBody); err != nil {
		return nil, err
	} else if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("icloud: 2FA verify failed: status %d", resp.StatusCode)
	}

	// Trust this session so future logins skip 2FA.
	_, trustResp, err := p.rawDo(ctx, http.MethodGet, idmsaBase+"/2sv/trust", sessHeaders, nil)
	if err != nil {
		return nil, err
	}
	trustToken := trustResp.Header.Get(hdrTrustTok)
	sessionToken := trustResp.Header.Get(hdrSessTok)
	if sessionToken == "" {
		sessionToken = h.SessionToken
	}
	country := trustResp.Header.Get(hdrAcctCty)
	if country == "" {
		country = h.Country
	}

	sess, err := p.finishLoginTokens(ctx, sessionToken, trustToken, country, mergeCookies(h.Cookies, saveCookies(trustResp)))
	if err != nil {
		return nil, err
	}
	sess.AppleID = h.AppleID
	return sess, nil
}

// finishLogin extracts the session token from a signed-in response and runs
// accountLogin.
func (p *Provider) finishLogin(ctx context.Context, resp *http.Response, trustToken string, cookies []SavedCookie) (*Session, error) {
	sessionToken := resp.Header.Get(hdrSessTok)
	country := resp.Header.Get(hdrAcctCty)
	return p.finishLoginTokens(ctx, sessionToken, trustToken, country, cookies)
}

// finishLoginTokens runs setup.icloud.com/accountLogin and builds a Session.
func (p *Provider) finishLoginTokens(ctx context.Context, sessionToken, trustToken, country string, cookies []SavedCookie) (*Session, error) {
	body, err := buildAccountLoginBody(sessionToken, trustToken, country)
	if err != nil {
		return nil, err
	}
	respBody, resp, err := p.rawDo(ctx, http.MethodPost, setupBase+"/accountLogin", map[string]string{"Origin": oauthRedir}, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("icloud: accountLogin status %d", resp.StatusCode)
	}
	dsid, services, err := parseAccountLogin(respBody)
	if err != nil {
		return nil, err
	}
	return &Session{
		Cookies:        mergeCookies(cookies, saveCookies(resp)),
		SessionToken:   sessionToken,
		TrustToken:     trustToken,
		AccountCountry: country,
		DSID:           dsid,
		WebServices:    services,
	}, nil
}

// rawDo performs a request without treating 4xx as an error (callers inspect
// the status). It replays nothing — auth state is passed via headers/cookies.
func (p *Provider) rawDo(ctx context.Context, method, urlStr string, headers map[string]string, body []byte) ([]byte, *http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return respBody, resp, err
}

func saveCookies(resp *http.Response) []SavedCookie {
	var out []SavedCookie
	for _, c := range resp.Cookies() {
		out = append(out, SavedCookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path})
	}
	return out
}

func mergeCookies(a, b []SavedCookie) []SavedCookie {
	idx := map[string]int{}
	out := append([]SavedCookie{}, a...)
	for i, c := range out {
		idx[c.Name] = i
	}
	for _, c := range b {
		if i, ok := idx[c.Name]; ok {
			out[i] = c
		} else {
			idx[c.Name] = len(out)
			out = append(out, c)
		}
	}
	return out
}

func truncate(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max])
	}
	return string(b)
}
