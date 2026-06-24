package icloud

import (
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

// signinBody is the JSON posted to idmsa .../signin.
type signinBody struct {
	AccountName string   `json:"accountName"`
	Password    string   `json:"password"`
	RememberMe  bool     `json:"rememberMe"`
	TrustTokens []string `json:"trustTokens,omitempty"`
}

// buildSigninBody builds the sign-in request body (pure).
func buildSigninBody(appleID, password string, trustTokens []string) ([]byte, error) {
	return json.Marshal(signinBody{
		AccountName: appleID,
		Password:    password,
		RememberMe:  true,
		TrustTokens: trustTokens,
	})
}

// authHeaders returns the OAuth headers idmsa requires (pure). state is a
// per-attempt random identifier.
func authHeaders(state string) map[string]string {
	return map[string]string{
		"Content-Type":                "application/json",
		"Accept":                      "application/json",
		"X-Apple-Widget-Key":          widgetKey,
		"X-Apple-OAuth-Client-Id":     widgetKey,
		"X-Apple-OAuth-Client-Type":   "firstPartyAuth",
		"X-Apple-OAuth-Redirect-URI":  oauthRedir,
		"X-Apple-OAuth-Response-Mode": "web_message",
		"X-Apple-OAuth-Response-Type": "code",
		"X-Apple-OAuth-State":         state,
		"Origin":                      "https://idmsa.apple.com",
	}
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
