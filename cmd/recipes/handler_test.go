package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"recipes/internal/auth"
	"recipes/internal/config"
	"recipes/internal/oauth"
	"recipes/internal/search"
	"recipes/internal/store"
	"recipes/internal/web"
)

const callback = "http://127.0.0.1:9/cb"

type env struct {
	t       *testing.T
	base    string
	browser *http.Client // cookie jar, no redirects
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	for _, d := range []string{filepath.Join(dir, "uploads"), filepath.Join(dir, "sessions")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewUnstartedServer(nil)
	base := "http://" + ts.Listener.Addr().String()
	cfg := &config.Config{Addr: ":0", DataDir: dir, SiteName: "Тест", PublicURL: base}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("pw")
	if _, err := st.CreateUser(ctx, "cook", hash, false); err != nil {
		t.Fatal(err)
	}
	keys, err := auth.LoadOrCreateKeys(cfg.KeysPath())
	if err != nil {
		t.Fatal(err)
	}
	authz, err := oauth.New(st, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	searchSvc := search.New(st, nil, 0)
	site, err := web.NewServer(cfg, st, keys, nil, searchSvc, authz)
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = buildHandler(site.Handler(), authz, newMCPServer(st, searchSvc, cfg))
	ts.Start()
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &env{t: t, base: base, browser: browser}
}

func (e *env) do(c *http.Client, method, path string, form url.Values) *http.Response {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, e.base+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", resp.Request.URL, err)
	}
}

func (e *env) register(redirect string) (string, int) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"client_name": "Тестовый клиент", "redirect_uris": []string{redirect}})
	resp, err := http.Post(e.base+"/oauth/register", "application/json", strings.NewReader(string(b)))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ClientID, resp.StatusCode
}

func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeQuery(clientID, challenge string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {callback},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"state": {"xyz"}, "scope": {"recipes:read recipes:write"}, "resource": {""},
	}
}

// login signs the browser in through the login form, following next.
func (e *env) login(next string) *http.Response {
	e.t.Helper()
	return e.do(e.browser, http.MethodPost, "/admin/login",
		url.Values{"username": {"cook"}, "password": {"pw"}, "next": {next}})
}

// authorize runs consent with the given decision and returns the redirect back
// to the client.
func (e *env) authorize(clientID, challenge, decision string) *url.URL {
	e.t.Helper()
	q := authorizeQuery(clientID, challenge)
	q.Set("resource", e.base+"/mcp")
	resp := e.do(e.browser, http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("consent page: status %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		e.t.Error("consent page may be framed")
	}
	page, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(page), "Тестовый клиент") {
		e.t.Error("consent page does not name the client")
	}
	// The client asks for write access, so the write checkbox starts ticked;
	// submit it ticked, as a browser would.
	if !strings.Contains(string(page), `name="allow_write" checked`) {
		e.t.Error("write checkbox not pre-ticked for a client asking for write access")
	}
	q.Set("allow_write", "on")
	q.Set("decision", decision)
	resp = e.do(e.browser, http.MethodPost, "/oauth/authorize", q)
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("decision: status %d", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(u.String(), callback+"?") {
		e.t.Fatalf("decision redirect = %q", resp.Header.Get("Location"))
	}
	if u.Query().Get("state") != "xyz" || u.Query().Get("iss") != e.base {
		e.t.Errorf("redirect lacks state/iss: %s", u)
	}
	return u
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func (e *env) token(form url.Values) (tokens, int) {
	e.t.Helper()
	resp := e.do(http.DefaultClient, http.MethodPost, "/oauth/token", form)
	var out tokens
	decode(e.t, resp, &out)
	return out, resp.StatusCode
}

// mcpStatus returns the HTTP status of an MCP initialize request with the given
// Authorization header value and the browser's cookies.
func (e *env) mcpStatus(authz string) (int, string) {
	e.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req, _ := http.NewRequest(http.MethodPost, e.base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := e.browser.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestOAuthAndMCPEndToEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Discovery: an unauthenticated MCP call points at the resource metadata.
	status, challengeHdr := e.mcpStatus("")
	if status != http.StatusUnauthorized || !strings.Contains(challengeHdr, `resource_metadata="`+e.base+"/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("unauthenticated /mcp: %d %q", status, challengeHdr)
	}
	// No scope hint: clients would request exactly that scope and never ask for
	// write access, instead of using scopes_supported from the metadata.
	if strings.Contains(challengeHdr, "scope=") {
		t.Fatalf("401 challenge advertises a scope: %q", challengeHdr)
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	decode(t, e.do(http.DefaultClient, http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil), &prm)
	if prm.Resource != e.base+"/mcp" || len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != e.base {
		t.Fatalf("protected resource metadata = %+v", prm)
	}
	var asm map[string]any
	decode(t, e.do(http.DefaultClient, http.MethodGet, "/.well-known/oauth-authorization-server", nil), &asm)
	if asm["issuer"] != e.base || asm["registration_endpoint"] != e.base+"/oauth/register" ||
		asm["authorization_response_iss_parameter_supported"] != true {
		t.Fatalf("authorization server metadata = %v", asm)
	}

	// Registration is limited to allowlisted redirect URIs.
	if _, code := e.register("https://evil.example/cb"); code != http.StatusBadRequest {
		t.Fatalf("registering a foreign redirect: status %d", code)
	}
	if _, code := e.register("http://localhost:1@evil.example/cb"); code != http.StatusBadRequest {
		t.Fatalf("registering a userinfo redirect: status %d", code)
	}
	clientID, code := e.register(callback)
	if code != http.StatusCreated || clientID == "" {
		t.Fatalf("register: status %d id %q", code, clientID)
	}

	// An unknown client gets an error page, never a redirect.
	resp := e.do(e.browser, http.MethodGet, "/oauth/authorize?"+authorizeQuery("nope", "x").Encode(), nil)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/admin/login?next=") {
		t.Fatalf("authorize while logged out: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Logging in returns to the authorization request.
	verifier, challenge := pkce()
	next := "/oauth/authorize?" + authorizeQuery(clientID, challenge).Encode()
	if resp := e.login(next); resp.Header.Get("Location") != next {
		t.Fatalf("login redirect = %q, want %q", resp.Header.Get("Location"), next)
	}
	if resp := e.login("//evil.example/"); resp.Header.Get("Location") != "/" {
		t.Fatalf("login followed an off-site next: %q", resp.Header.Get("Location"))
	}
	resp = e.do(e.browser, http.MethodGet, "/oauth/authorize?"+authorizeQuery("nope", challenge).Encode(), nil)
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Fatalf("unknown client: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// A session cookie alone does not authenticate MCP.
	if status, _ := e.mcpStatus(""); status != http.StatusUnauthorized {
		t.Fatalf("/mcp with a session cookie: status %d", status)
	}

	// Deny reports access_denied to the client.
	if u := e.authorize(clientID, challenge, "deny"); u.Query().Get("error") != "access_denied" {
		t.Fatalf("deny redirect = %s", u)
	}

	// A wrong PKCE verifier burns the code.
	authCode := e.authorize(clientID, challenge, "allow").Query().Get("code")
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {authCode}, "redirect_uri": {callback},
		"client_id": {clientID}, "code_verifier": {strings.Repeat("w", 50)}}
	if tok, status := e.token(exchange); status != http.StatusBadRequest || tok.Error != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %+v", status, tok)
	}

	authCode = e.authorize(clientID, challenge, "allow").Query().Get("code")
	exchange.Set("code", authCode)
	exchange.Set("code_verifier", verifier)
	exchange.Set("resource", e.base+"/mcp")
	tok, status := e.token(exchange)
	if status != http.StatusOK || tok.AccessToken == "" || tok.RefreshToken == "" || tok.Scope != "recipes:read recipes:write" {
		t.Fatalf("token exchange: %d %+v", status, tok)
	}
	if again, status := e.token(exchange); status != http.StatusBadRequest || again.Error != "invalid_grant" {
		t.Fatalf("code replay: %d %+v", status, again)
	}

	// The access token works with a real MCP client.
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: e.base + "/mcp", HTTPClient: &http.Client{Transport: bearer{tok.AccessToken}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("MCP connect with token: %v", err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_categories"})
	if err != nil || res.IsError {
		t.Fatalf("list_categories: %v %+v", err, res)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "create_recipe", Arguments: map[string]any{"title": "Чай", "category": "Напитки"}})
	if err != nil || res.IsError {
		t.Fatalf("create_recipe with a write token: %v %+v", err, res)
	}
	_ = cs.Close()

	// The user decides on write access regardless of what the client asked:
	// a read-only request with the box ticked yields a write token...
	upQ := authorizeQuery(clientID, challenge)
	upQ.Set("scope", oauth.ScopeRead)
	resp = e.do(e.browser, http.MethodGet, "/oauth/authorize?"+upQ.Encode(), nil)
	consent, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(consent), `name="allow_write" checked`) {
		t.Fatal("write checkbox pre-ticked for a client asking only for read")
	}
	upQ.Set("decision", "allow")
	upQ.Set("allow_write", "on")
	resp = e.do(e.browser, http.MethodPost, "/oauth/authorize", upQ)
	upCode, _ := url.Parse(resp.Header.Get("Location"))
	upTok, status := e.token(url.Values{"grant_type": {"authorization_code"}, "code": {upCode.Query().Get("code")},
		"redirect_uri": {callback}, "client_id": {clientID}, "code_verifier": {verifier}})
	if status != http.StatusOK || upTok.Scope != oauth.ScopeRead+" "+oauth.ScopeWrite {
		t.Fatalf("read request with write ticked: %d %+v", status, upTok)
	}

	// ...and a write request with the box unticked yields a read-only token,
	// which cannot write.
	readQ := authorizeQuery(clientID, challenge)
	readQ.Set("decision", "allow")
	resp = e.do(e.browser, http.MethodPost, "/oauth/authorize", readQ)
	readCode, _ := url.Parse(resp.Header.Get("Location"))
	readExchange := url.Values{"grant_type": {"authorization_code"}, "code": {readCode.Query().Get("code")},
		"redirect_uri": {callback}, "client_id": {clientID}, "code_verifier": {verifier}}
	readTok, status := e.token(readExchange)
	if status != http.StatusOK || readTok.Scope != oauth.ScopeRead {
		t.Fatalf("read-only token: %d %+v", status, readTok)
	}
	rcs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: e.base + "/mcp", HTTPClient: &http.Client{Transport: bearer{readTok.AccessToken}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = rcs.CallTool(ctx, &mcp.CallToolParams{Name: "create_recipe", Arguments: map[string]any{"title": "Кофе", "category": "Напитки"}})
	if err != nil || !res.IsError {
		t.Fatalf("create_recipe with a read-only token succeeded: %v %+v", err, res)
	}
	_ = rcs.Close()

	// The grant shows on the connections page.
	page, _ := io.ReadAll(e.do(e.browser, http.MethodGet, "/admin/connections", nil).Body)
	if !strings.Contains(string(page), "Тестовый клиент") {
		t.Fatal("connections page does not list the grant")
	}

	// Refresh rotates both tokens; the old access token stops working.
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {clientID}}
	tok2, status := e.token(refresh)
	if status != http.StatusOK || tok2.AccessToken == tok.AccessToken || tok2.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh: %d %+v", status, tok2)
	}
	if status, _ := e.mcpStatus("Bearer " + tok.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("old access token after refresh: status %d", status)
	}
	if status, _ := e.mcpStatus("Bearer " + tok2.AccessToken); status != http.StatusOK {
		t.Fatalf("new access token: status %d", status)
	}

	// Replaying the rotated refresh token revokes the whole grant.
	if bad, status := e.token(refresh); status != http.StatusBadRequest || bad.Error != "invalid_grant" {
		t.Fatalf("refresh reuse: %d %+v", status, bad)
	}
	if status, _ := e.mcpStatus("Bearer " + tok2.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("access token after refresh reuse: status %d", status)
	}

	// Revocation endpoint and the connections page both end a grant.
	exchange.Set("code", e.authorize(clientID, challenge, "allow").Query().Get("code"))
	tok3, _ := e.token(exchange)
	e.do(http.DefaultClient, http.MethodPost, "/oauth/revoke", url.Values{"token": {tok3.RefreshToken}})
	if status, _ := e.mcpStatus("Bearer " + tok3.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("access token after revoke: status %d", status)
	}

	exchange.Set("code", e.authorize(clientID, challenge, "allow").Query().Get("code"))
	tok4, _ := e.token(exchange)
	page, _ = io.ReadAll(e.do(e.browser, http.MethodGet, "/admin/connections", nil).Body)
	m := regexp.MustCompile(`/admin/connections/(\d+)/revoke`).FindStringSubmatch(string(page))
	if m == nil {
		t.Fatal("connections page has no revoke form")
	}
	e.do(e.browser, http.MethodPost, "/admin/connections/"+m[1]+"/revoke", url.Values{})
	if status, _ := e.mcpStatus("Bearer " + tok4.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("access token after revoking on the connections page: status %d", status)
	}
}
