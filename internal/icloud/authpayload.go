package icloud

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Endpoints and the iCloud web client identifier (the "widget key"), as used by
// the iCloud web app. These are stable-ish constants of the private API.
const (
	idmsaBase  = "https://idmsa.apple.com/appleauth/auth"
	setupBase  = "https://setup.icloud.com/setup/ws/1"
	widgetKey  = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
	oauthRedir = "https://www.icloud.com"
)

// buildFederateBody is the JSON posted to .../federate (detects managed accounts).
func buildFederateBody(appleID string) ([]byte, error) {
	return json.Marshal(map[string]any{"accountName": appleID, "rememberMe": false})
}

// buildSigninInitBody is the SRP init request: the client public value A
// (base64), the account name, and the supported password protocols.
func buildSigninInitBody(appleID string, aWire []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"a":           base64.StdEncoding.EncodeToString(aWire),
		"accountName": appleID,
		"protocols":   []string{"s2k", "s2k_fo"},
	})
}

// signinInitResp is the SRP challenge returned by .../signin/init.
type signinInitResp struct {
	Iteration int    `json:"iteration"`
	Salt      string `json:"salt"`
	Protocol  string `json:"protocol"`
	B         string `json:"b"`
	C         string `json:"c"`
}

// parseSigninInit decodes the SRP challenge (pure).
func parseSigninInit(body []byte) (salt, B []byte, iter int, protocol, c string, err error) {
	var r signinInitResp
	if err = json.Unmarshal(body, &r); err != nil {
		return nil, nil, 0, "", "", fmt.Errorf("icloud: parse signin/init: %w", err)
	}
	if salt, err = base64.StdEncoding.DecodeString(r.Salt); err != nil {
		return nil, nil, 0, "", "", fmt.Errorf("icloud: signin/init salt: %w", err)
	}
	if B, err = base64.StdEncoding.DecodeString(r.B); err != nil {
		return nil, nil, 0, "", "", fmt.Errorf("icloud: signin/init b: %w", err)
	}
	if r.Iteration <= 0 || len(salt) == 0 || len(B) == 0 {
		return nil, nil, 0, "", "", fmt.Errorf("icloud: signin/init response incomplete")
	}
	return salt, B, r.Iteration, r.Protocol, r.C, nil
}

// buildSigninCompleteBody is the SRP complete request: the client proof m1,
// the expected server proof m2, and the c token from init.
func buildSigninCompleteBody(appleID, c string, m1, m2 []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"accountName": appleID,
		"rememberMe":  false,
		"m1":          base64.StdEncoding.EncodeToString(m1),
		"c":           c,
		"m2":          base64.StdEncoding.EncodeToString(m2),
		"trustTokens": []string{},
	})
}

// authHeaders returns the full idmsa header set the iCloud web app sends.
// frameID is a per-attempt id used as both the OAuth state and frame id.
func authHeaders(frameID string) map[string]string {
	return map[string]string{
		"Content-Type":                     "application/json",
		"Accept":                           "application/json, text/javascript, */*; q=0.01",
		"X-Apple-Widget-Key":               widgetKey,
		"X-Apple-OAuth-Client-Id":          widgetKey,
		"X-Apple-OAuth-Client-Type":        "firstPartyAuth",
		"X-Apple-OAuth-Redirect-URI":       oauthRedir,
		"X-Apple-OAuth-Response-Mode":      "web_message",
		"X-Apple-OAuth-Response-Type":      "code",
		"X-Apple-OAuth-State":              frameID,
		"X-Apple-OAuth-Require-Grant-Code": "true",
		"X-Apple-Frame-Id":                 frameID,
		"X-Apple-Domain-Id":                "3",
		"X-Apple-Locale":                   "en_US",
		"X-Apple-I-FD-Client-Info":         fdClientInfo(),
		"X-Apple-Offer-Security-Upgrade":   "1",
		"X-Requested-With":                 "XMLHttpRequest",
		"Origin":                           "https://idmsa.apple.com",
		"Referer":                          "https://idmsa.apple.com/",
	}
}

// oauthQuery builds the GET /authorize/signin query that seeds the session.
func oauthQuery(frameID string) string {
	return "client_id=" + widgetKey +
		"&redirect_uri=" + oauthRedir +
		"&response_type=code&response_mode=web_message" +
		"&state=" + frameID + "&frame_id=" + frameID + "&authVersion=latest"
}

// securityCodeBody is the JSON posted to verify an HSA2 2FA code.
type securityCodeBody struct {
	SecurityCode struct {
		Code string `json:"code"`
	} `json:"securityCode"`
}

func buildSecurityCodeBody(code string) ([]byte, error) {
	var b securityCodeBody
	b.SecurityCode.Code = code
	return json.Marshal(b)
}

// accountLoginBody is posted to setup.icloud.com/accountLogin.
type accountLoginBody struct {
	DSWebAuthToken     string `json:"dsWebAuthToken"`
	TrustToken         string `json:"trustToken,omitempty"`
	AccountCountryCode string `json:"accountCountryCode,omitempty"`
	ExtendedLogin      bool   `json:"extended_login"`
}

func buildAccountLoginBody(sessionToken, trustToken, country string) ([]byte, error) {
	return json.Marshal(accountLoginBody{
		DSWebAuthToken:     sessionToken,
		TrustToken:         trustToken,
		AccountCountryCode: country,
		ExtendedLogin:      true,
	})
}

// accountLoginResp is the relevant slice of the accountLogin response.
type accountLoginResp struct {
	DSInfo struct {
		DSID string `json:"dsid"`
	} `json:"dsInfo"`
	WebServices map[string]struct {
		URL    string `json:"url"`
		Status string `json:"status"`
	} `json:"webservices"`
}

// parseAccountLogin extracts the dsid and the service->url map (pure).
func parseAccountLogin(body []byte) (dsid string, services map[string]string, err error) {
	var r accountLoginResp
	if err := json.Unmarshal(body, &r); err != nil {
		return "", nil, fmt.Errorf("icloud: parse accountLogin: %w", err)
	}
	if r.DSInfo.DSID == "" {
		return "", nil, fmt.Errorf("icloud: accountLogin response missing dsid")
	}
	services = make(map[string]string, len(r.WebServices))
	for name, ws := range r.WebServices {
		if ws.URL != "" {
			services[name] = ws.URL
		}
	}
	return r.DSInfo.DSID, services, nil
}
