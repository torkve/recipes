package icloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	hdrAuthAttrs = "X-Apple-Auth-Attributes"
)

// authState is the in-flight idmsa auth session, threaded through the SRP steps
// and serialized into a BindHandle so the 2FA step can resume it.
type authState struct {
	AppleID        string        `json:"apple_id"`
	FrameID        string        `json:"frame_id"`
	Scnt           string        `json:"scnt"`
	SessionID      string        `json:"session_id"`
	AuthAttributes string        `json:"auth_attributes"`
	SessionToken   string        `json:"session_token"` // X-Apple-Session-Token (dsWebAuthToken)
	TrustToken     string        `json:"trust_token"`
	Country        string        `json:"country"`
	Cookies        []SavedCookie `json:"cookies"`

	// 2FA delivery: "device" (trusted-device push) or "sms" (to PhoneID).
	Mode    string `json:"mode"`
	PhoneID int    `json:"phone_id"`
}

func newJarClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func randState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// idmsaDo issues an idmsa request with the full header set plus the current
// session headers (scnt / session-id / auth-attributes), then folds the
// response's session headers and cookies back into st.
func (p *Provider) idmsaDo(ctx context.Context, method, url string, st *authState, body []byte) ([]byte, *http.Response, error) {
	return p.idmsaDoAccept(ctx, method, url, "", st, body)
}

// idmsaDoAccept is idmsaDo with an optional Accept override (e.g. strict
// "application/json" for the 2FA context, which otherwise returns the HTML widget).
func (p *Provider) idmsaDoAccept(ctx context.Context, method, url, accept string, st *authState, body []byte) ([]byte, *http.Response, error) {
	headers := authHeaders(st.FrameID)
	if accept != "" {
		headers["Accept"] = accept
	}
	if st.Scnt != "" {
		headers[hdrScnt] = st.Scnt
	}
	if st.SessionID != "" {
		headers[hdrSessionID] = st.SessionID
	}
	if st.AuthAttributes != "" {
		headers[hdrAuthAttrs] = st.AuthAttributes
	}
	respBody, resp, err := p.rawDo(ctx, method, url, headers, body)
	if resp != nil {
		captureSessionState(resp, st)
	}
	return respBody, resp, err
}

// captureSessionState updates st with any session headers / cookies the response
// carries. scnt rotates on every response, so the latest value must be reused.
func captureSessionState(resp *http.Response, st *authState) {
	if v := resp.Header.Get(hdrScnt); v != "" {
		st.Scnt = v
	}
	if v := resp.Header.Get(hdrSessionID); v != "" {
		st.SessionID = v
	}
	if v := resp.Header.Get(hdrAuthAttrs); v != "" {
		st.AuthAttributes = v
	}
	if v := resp.Header.Get(hdrSessTok); v != "" {
		st.SessionToken = v
	}
	if v := resp.Header.Get(hdrTrustTok); v != "" {
		st.TrustToken = v
	}
	if v := resp.Header.Get(hdrAcctCty); v != "" {
		st.Country = v
	}
	st.Cookies = mergeCookies(st.Cookies, saveCookies(resp))
}

// requestSecurityCode fetches the 2FA context and sends the HSA2 code. Trusted
// devices receive the code automatically on the 409; accounts with no trusted
// device but a trusted phone get an SMS via PUT /verify/phone. The chosen mode
// (and phone id) is recorded in st so Complete verifies against the right
// endpoint. Best-effort: failures are logged, never block the code form.
func (p *Provider) requestSecurityCode(ctx context.Context, st *authState) {
	st.Mode = "device" // default: trusted-device code is auto-sent on the 409

	body, resp, err := p.idmsaDoAccept(ctx, http.MethodGet, idmsaBase, "application/json", st, nil)
	if err != nil {
		log.Printf("icloud: 2FA context fetch failed: %v", err)
		return
	}
	log.Printf("icloud: 2FA context (%d): %s", resp.StatusCode, truncate(body))
	td, phoneIDs := parseAuthContext(body)
	log.Printf("icloud: 2FA context: trustedDevices=%d trustedPhones=%d", td, len(phoneIDs))

	// If there is no trusted device but a trusted phone, request an SMS code.
	if td == 0 && len(phoneIDs) > 0 {
		st.Mode = "sms"
		st.PhoneID = phoneIDs[0]
		reqBody, berr := buildPhoneRequestBody(st.PhoneID)
		if berr != nil {
			log.Printf("icloud: build SMS request failed: %v", berr)
			return
		}
		if _, r2, e2 := p.idmsaDo(ctx, http.MethodPut, idmsaBase+"/verify/phone", st, reqBody); e2 != nil {
			log.Printf("icloud: request SMS code failed: %v", e2)
		} else {
			log.Printf("icloud: request SMS code to phone %d: status %d", st.PhoneID, r2.StatusCode)
		}
	}
}

// Begin runs Apple's SRP-6a sign-in: authorize (seed session) → federate →
// signin/init (SRP challenge) → signin/complete (SRP proof). A 409 means HSA2
// two-factor is required next.
func (p *Provider) Begin(ctx context.Context, appleID, password string) (notesync.BindResult, error) {
	st := &authState{AppleID: appleID, FrameID: randState()}

	// Seed the OAuth session and aasp cookie (the web_message iframe bootstrap).
	authBody, resp, err := p.idmsaDo(ctx, http.MethodGet, idmsaBase+"/authorize/signin?"+oauthQuery(st.FrameID), st, nil)
	if err != nil {
		return notesync.BindResult{}, err
	}
	if resp.StatusCode >= 400 {
		return notesync.BindResult{}, fmt.Errorf("icloud: authorize/signin status %d: %s", resp.StatusCode, truncate(authBody))
	}

	// Account discovery (managed/federated accounts take a different path).
	fed, err := buildFederateBody(appleID)
	if err != nil {
		return notesync.BindResult{}, err
	}
	fedBody, resp, err := p.idmsaDo(ctx, http.MethodPost, idmsaBase+"/federate?isRememberMeEnabled=true", st, fed)
	if err != nil {
		return notesync.BindResult{}, err
	}
	if resp.StatusCode >= 400 {
		return notesync.BindResult{}, fmt.Errorf("icloud: federate status %d: %s", resp.StatusCode, truncate(fedBody))
	}

	// SRP: Apple throttles repeated sign-in attempts (a second rapid
	// signin/complete returns 503), so we make exactly ONE attempt per bind with
	// the configured byte convention. If it is rejected (401), the operator sets
	// the next RECIPES_ICLOUD_SRP_VARIANT and re-binds.
	opts := srpVariants[p.srpVariant]
	log.Printf("icloud: using SRP variant %d (%+v)", p.srpVariant, opts)

	client, err := newSRPClient()
	if err != nil {
		return notesync.BindResult{}, err
	}
	initReq, err := buildSigninInitBody(appleID, client.aWire())
	if err != nil {
		return notesync.BindResult{}, err
	}
	initBody, resp, err := p.idmsaDo(ctx, http.MethodPost, idmsaBase+"/signin/init", st, initReq)
	if err != nil {
		return notesync.BindResult{}, err
	}
	if resp.StatusCode >= 400 {
		return notesync.BindResult{}, fmt.Errorf("icloud: signin/init status %d: %s", resp.StatusCode, truncate(initBody))
	}
	salt, B, iter, protocol, cVal, err := parseSigninInit(initBody)
	if err != nil {
		return notesync.BindResult{}, err
	}

	dk, err := derivePasswordKey(password, salt, iter, protocol)
	if err != nil {
		return notesync.BindResult{}, err
	}
	m1, m2, err := client.proof(appleID, dk, salt, B, opts)
	if err != nil {
		return notesync.BindResult{}, err
	}

	compReq, err := buildSigninCompleteBody(appleID, cVal, m1, m2)
	if err != nil {
		return notesync.BindResult{}, err
	}
	compBody, resp, err := p.idmsaDo(ctx, http.MethodPost, idmsaBase+"/signin/complete?isRememberMeEnabled=true", st, compReq)
	if err != nil {
		return notesync.BindResult{}, err
	}

	switch resp.StatusCode {
	case http.StatusConflict, http.StatusPreconditionFailed:
		// HSA2 two-factor required. Apple does not auto-push the code to the
		// headless flow, so explicitly request a trusted-device code before
		// returning. This also rotates scnt/session-id into st for Complete.
		log.Printf("icloud: SRP variant %d accepted (2FA required)", p.srpVariant)
		p.requestSecurityCode(ctx, st)
		raw, _ := json.Marshal(st)
		return notesync.BindResult{Pending: true, Handle: notesync.BindHandle(raw)}, nil
	case http.StatusOK, http.StatusNoContent, http.StatusFound:
		// Signed in without 2FA (uncommon).
		log.Printf("icloud: SRP variant %d accepted", p.srpVariant)
		sess, err := p.finishLoginTokens(ctx, st.SessionToken, st.TrustToken, st.Country, st.Cookies)
		if err != nil {
			return notesync.BindResult{}, err
		}
		sess.AppleID = appleID
		return notesync.BindResult{Session: sess}, nil
	case http.StatusUnauthorized:
		return notesync.BindResult{}, fmt.Errorf(
			"icloud: signin/complete rejected SRP variant %d (401). Set RECIPES_ICLOUD_SRP_VARIANT to another value in 0..%d and re-bind: %s",
			p.srpVariant, len(srpVariants)-1, truncate(compBody))
	default:
		return notesync.BindResult{}, fmt.Errorf("icloud: signin/complete status %d: %s", resp.StatusCode, truncate(compBody))
	}
}

// Complete submits the 2FA code, trusts the session, and finishes login.
func (p *Provider) Complete(ctx context.Context, handle notesync.BindHandle, code string) (notesync.Session, error) {
	var st authState
	if err := json.Unmarshal(handle, &st); err != nil {
		return nil, fmt.Errorf("icloud: bad bind handle: %w", err)
	}

	// Verify the security code against the endpoint matching how it was sent.
	var codeBody []byte
	var verifyURL string
	var err error
	if st.Mode == "sms" {
		codeBody, err = buildPhoneSecurityCodeBody(st.PhoneID, code)
		verifyURL = idmsaBase + "/verify/phone/securitycode"
	} else {
		codeBody, err = buildSecurityCodeBody(code)
		verifyURL = idmsaBase + "/verify/trusteddevice/securitycode"
	}
	if err != nil {
		return nil, err
	}
	body, resp, err := p.idmsaDo(ctx, http.MethodPost, verifyURL, &st, codeBody)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("icloud: 2FA verify status %d: %s", resp.StatusCode, truncate(body))
	}

	// Trust this session so future logins can skip 2FA.
	if _, _, err := p.idmsaDo(ctx, http.MethodGet, idmsaBase+"/2sv/trust", &st, nil); err != nil {
		return nil, err
	}

	sess, err := p.finishLoginTokens(ctx, st.SessionToken, st.TrustToken, st.Country, st.Cookies)
	if err != nil {
		return nil, err
	}
	sess.AppleID = st.AppleID
	return sess, nil
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
	setBrowserHeaders(req)
	logRequest(method, urlStr)
	resp, err := p.http.Do(req)
	if err != nil {
		log.Printf("icloud: ✗ %s %s: %v", method, stripQuery(urlStr), err)
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	logResponse(method, urlStr, resp.StatusCode, respBody)
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
