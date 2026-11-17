// Package oauth is the OAuth 2.1 authorization server protecting the MCP
// endpoint. It implements what the Claude and ChatGPT connectors use:
// authorization server and protected resource metadata (RFC 8414, RFC 9728),
// dynamic client registration for public clients (RFC 7591), the authorization
// code grant with mandatory PKCE S256, rotating refresh tokens with reuse
// detection, token revocation (RFC 7009) and bearer token verification for the
// resource.
//
// The user-facing authorization step (login and consent) is rendered by package
// web, which calls ParseAuthorize, Approve and Deny.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"recipes/internal/models"
	"recipes/internal/store"
)

// Scopes.
const (
	ScopeRead  = "recipes:read"
	ScopeWrite = "recipes:write"
)

var allScopes = []string{ScopeRead, ScopeWrite}

const (
	accessTTL       = time.Hour
	refreshTTL      = 60 * 24 * time.Hour
	codeTTL         = 2 * time.Minute
	maxClients      = 200
	unusedClientAge = time.Hour
	purgeInterval   = 10 * time.Minute
	maxBodyBytes    = 64 << 10
	maxRedirectURIs = 10
	maxClientName   = 100
)

// MCPPath is the path of the protected MCP endpoint.
const MCPPath = "/mcp"

var (
	verifierRE  = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
	challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

// Server is the authorization server for one public site URL.
type Server struct {
	store     *store.Store
	issuer    string // public site URL, scheme://host
	resource  string // issuer + MCPPath
	redirects []redirectPattern
	now       func() time.Time
	lastPurge atomic.Int64 // unix seconds
}

// New returns a Server for the site at publicURL (scheme://host, no trailing
// slash). redirectPatterns limits the redirect URIs clients may register; nil
// selects DefaultRedirectPatterns. Malformed patterns are an error.
func New(st *store.Store, publicURL string, redirectPatterns []string) (*Server, error) {
	if redirectPatterns == nil {
		redirectPatterns = DefaultRedirectPatterns
	}
	s := &Server{store: st, issuer: publicURL, resource: publicURL + MCPPath, now: time.Now}
	for _, p := range redirectPatterns {
		rp, ok := parsePattern(p)
		if !ok {
			return nil, fmt.Errorf("oauth: bad redirect pattern %q", p)
		}
		s.redirects = append(s.redirects, rp)
	}
	return s, nil
}

// Resource is the canonical URL of the protected MCP endpoint.
func (s *Server) Resource() string { return s.resource }

// ResourceMetadataURL is the protected resource metadata document URL that
// 401 responses from the MCP endpoint point clients to.
func (s *Server) ResourceMetadataURL() string {
	return s.issuer + "/.well-known/oauth-protected-resource" + MCPPath
}

// Register mounts the metadata, registration, token and revocation endpoints.
// These take no cookies and need no CSRF protection.
func (s *Server) Register(mux *http.ServeMux) {
	prm := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.resource,
		AuthorizationServers:   []string{s.issuer},
		ScopesSupported:        allScopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Семейная кулинарная книга",
	})
	mux.Handle("/.well-known/oauth-protected-resource", prm)
	mux.Handle("/.well-known/oauth-protected-resource"+MCPPath, prm)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleASMetadata)
	mux.HandleFunc("/oauth/register", cors(s.handleRegister))
	mux.HandleFunc("/oauth/token", cors(s.handleToken))
	mux.HandleFunc("/oauth/revoke", cors(s.handleRevoke))
}

// cors allows cross-origin calls from browser-based MCP clients. The endpoints
// it wraps authenticate by request parameters, never by cookies.
func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

// --- metadata ---

type asMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	ResponseModesSupported            []string `json:"response_modes_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethods     []string `json:"revocation_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	IssParameterSupported             bool     `json:"authorization_response_iss_parameter_supported"`
}

func (s *Server) handleASMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, asMetadata{
		Issuer:                            s.issuer,
		AuthorizationEndpoint:             s.issuer + "/oauth/authorize",
		TokenEndpoint:                     s.issuer + "/oauth/token",
		RegistrationEndpoint:              s.issuer + "/oauth/register",
		RevocationEndpoint:                s.issuer + "/oauth/revoke",
		ScopesSupported:                   allScopes,
		ResponseTypesSupported:            []string{"code"},
		ResponseModesSupported:            []string{"query"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		RevocationEndpointAuthMethods:     []string{"none"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		IssParameterSupported:             true,
	})
}

// --- dynamic client registration ---

type registerRequest struct {
	RedirectURIs  []string `json:"redirect_uris"`
	ClientName    string   `json:"client_name"`
	GrantTypes    []string `json:"grant_types"`
	ResponseTypes []string `json:"response_types"`
}

type registerResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req registerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be a JSON client metadata document")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > maxRedirectURIs {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "between 1 and 10 redirect_uris are required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !redirectAllowed(s.redirects, u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri not allowed: "+u)
			return
		}
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type: "+g)
			return
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported response_type: "+rt)
			return
		}
	}
	name := strings.TrimSpace(req.ClientName)
	if len([]rune(name)) > maxClientName {
		name = string([]rune(name)[:maxClientName])
	}

	now := s.now()
	if _, err := s.store.DeleteUnusedOAuthClients(ctx, now.Add(-unusedClientAge)); err != nil {
		serverError(w, err)
		return
	}
	if n, err := s.store.CountOAuthClients(ctx); err != nil {
		serverError(w, err)
		return
	} else if n >= maxClients {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many registered clients")
		return
	}
	c := models.OAuthClient{ID: randomToken(), Name: name, RedirectURIs: req.RedirectURIs, CreatedAt: now}
	if err := s.store.CreateOAuthClient(ctx, c); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, registerResponse{
		ClientID:                c.ID,
		ClientIDIssuedAt:        now.Unix(),
		ClientName:              c.Name,
		RedirectURIs:            c.RedirectURIs,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	})
}

// --- authorization (driven by the web consent page) ---

// AuthorizeRequest is a validated authorization request.
type AuthorizeRequest struct {
	ClientID      string
	ClientName    string
	RedirectURI   string
	State         string
	CodeChallenge string
	Scope         string // normalized, space-separated
	Resource      string
}

// Scopes returns the requested scopes.
func (a *AuthorizeRequest) Scopes() []string { return strings.Fields(a.Scope) }

// Params returns the request as form values that ParseAuthorize accepts, for
// carrying it through the consent form.
func (a *AuthorizeRequest) Params() url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {a.ClientID},
		"redirect_uri":          {a.RedirectURI},
		"state":                 {a.State},
		"code_challenge":        {a.CodeChallenge},
		"code_challenge_method": {"S256"},
		"scope":                 {a.Scope},
		"resource":              {a.Resource},
	}
}

// RedirectError is an authorization error to report by redirecting the user
// agent to URL (the client's validated redirect URI).
type RedirectError struct {
	URL  string
	Code string
}

func (e *RedirectError) Error() string { return "oauth: authorization error " + e.Code }

// ErrBadClient is returned by ParseAuthorize when the client or its redirect URI
// cannot be trusted; the error must be shown to the user, never redirected.
var ErrBadClient = errors.New("oauth: unknown client or unregistered redirect_uri")

// ParseAuthorize validates authorization request parameters. It returns
// ErrBadClient (wrapped) when the client or redirect URI is invalid, a
// *RedirectError for any other protocol error, or the validated request.
func (s *Server) ParseAuthorize(ctx context.Context, q url.Values) (*AuthorizeRequest, error) {
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	if clientID == "" || redirectURI == "" {
		return nil, fmt.Errorf("%w: client_id and redirect_uri are required", ErrBadClient)
	}
	c, err := s.store.GetOAuthClient(ctx, clientID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: unknown client_id", ErrBadClient)
	}
	if err != nil {
		return nil, err
	}
	if !slices.Contains(c.RedirectURIs, redirectURI) || !redirectAllowed(s.redirects, redirectURI) {
		return nil, fmt.Errorf("%w: redirect_uri is not registered for this client or no longer allowed", ErrBadClient)
	}

	req := &AuthorizeRequest{ClientID: c.ID, ClientName: c.Name, RedirectURI: redirectURI, State: q.Get("state")}
	fail := func(code, desc string) error {
		return &RedirectError{URL: s.errorRedirect(req, code, desc), Code: code}
	}
	if q.Get("response_type") != "code" {
		return nil, fail("unsupported_response_type", "only response_type=code is supported")
	}
	if q.Get("code_challenge_method") != "S256" || !challengeRE.MatchString(q.Get("code_challenge")) {
		return nil, fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
	}
	req.CodeChallenge = q.Get("code_challenge")
	if res := q.Get("resource"); res != "" && strings.TrimSuffix(res, "/") != s.resource {
		return nil, fail("invalid_target", "unknown resource")
	}
	req.Resource = s.resource
	req.Scope = normalizeScope(q.Get("scope"))
	return req, nil
}

// normalizeScope keeps the known scopes of raw, always including read access,
// and grants every scope when none is known.
func normalizeScope(raw string) string {
	set := map[string]bool{}
	for _, sc := range strings.Fields(raw) {
		if slices.Contains(allScopes, sc) {
			set[sc] = true
		}
	}
	if len(set) == 0 {
		return strings.Join(allScopes, " ")
	}
	set[ScopeRead] = true
	var out []string
	for _, sc := range allScopes {
		if set[sc] {
			out = append(out, sc)
		}
	}
	return strings.Join(out, " ")
}

// Approve issues an authorization code for userID and returns the URL to
// redirect the user agent to.
func (s *Server) Approve(ctx context.Context, req *AuthorizeRequest, userID int64) (string, error) {
	code := randomToken()
	err := s.store.SaveOAuthCode(ctx, models.OAuthCode{
		CodeHash:      hashToken(code),
		ClientID:      req.ClientID,
		UserID:        userID,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		Scope:         req.Scope,
		Resource:      req.Resource,
		ExpiresAt:     s.now().Add(codeTTL),
	})
	if err != nil {
		return "", err
	}
	return s.clientRedirect(req, url.Values{"code": {code}}), nil
}

// Deny returns the URL reporting to the client that the user refused access.
func (s *Server) Deny(req *AuthorizeRequest) string {
	return s.errorRedirect(req, "access_denied", "the user denied access")
}

func (s *Server) errorRedirect(req *AuthorizeRequest, code, desc string) string {
	return s.clientRedirect(req, url.Values{"error": {code}, "error_description": {desc}})
}

// clientRedirect appends params, state and iss to the request's redirect URI.
func (s *Server) clientRedirect(req *AuthorizeRequest, params url.Values) string {
	u, _ := url.Parse(req.RedirectURI) // validated at registration
	q := u.Query()
	for k, vs := range params {
		q[k] = vs
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	q.Set("iss", s.issuer)
	u.RawQuery = q.Encode()
	return u.String()
}

// --- token endpoint ---

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	s.maybePurge(r.Context())
	clientID := r.PostForm.Get("client_id")
	if user, _, ok := r.BasicAuth(); ok {
		if clientID != "" && clientID != user {
			oauthError(w, http.StatusBadRequest, "invalid_request", "conflicting client_id")
			return
		}
		clientID = user
	}
	if clientID == "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client_id is required")
		return
	}
	if res := r.PostForm.Get("resource"); res != "" && strings.TrimSuffix(res, "/") != s.resource {
		oauthError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, clientID)
	case "refresh_token":
		s.refresh(w, r, clientID)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, clientID string) {
	ctx := r.Context()
	code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")
	if code == "" || !verifierRE.MatchString(verifier) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code and a valid PKCE code_verifier are required")
		return
	}
	c, err := s.store.ConsumeOAuthCode(ctx, hashToken(code))
	if errors.Is(err, store.ErrNotFound) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown or used authorization code")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	now := s.now()
	switch {
	case !c.ExpiresAt.After(now):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code expired")
		return
	case c.ClientID != clientID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code was issued to another client")
		return
	case r.PostForm.Get("redirect_uri") != c.RedirectURI:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case !pkceMatches(verifier, c.CodeChallenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	access, refresh := randomToken(), randomToken()
	_, err = s.store.CreateOAuthGrant(ctx,
		models.OAuthGrant{ClientID: c.ClientID, UserID: c.UserID, Scope: c.Scope, Resource: c.Resource, CreatedAt: now},
		models.OAuthToken{Hash: hashToken(access), Kind: models.OAuthAccess, ExpiresAt: now.Add(accessTTL)},
		models.OAuthToken{Hash: hashToken(refresh), Kind: models.OAuthRefresh, ExpiresAt: now.Add(refreshTTL)})
	if err != nil {
		serverError(w, err)
		return
	}
	writeTokens(w, access, refresh, c.Scope)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, clientID string) {
	ctx := r.Context()
	old := r.PostForm.Get("refresh_token")
	if old == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	now := s.now()
	access, refresh := randomToken(), randomToken()
	g, err := s.store.RotateOAuthRefreshToken(ctx, hashToken(old), clientID, now,
		models.OAuthToken{Hash: hashToken(access), Kind: models.OAuthAccess, ExpiresAt: now.Add(accessTTL)},
		models.OAuthToken{Hash: hashToken(refresh), Kind: models.OAuthRefresh, ExpiresAt: now.Add(refreshTTL)})
	switch {
	case errors.Is(err, store.ErrTokenReuse):
		log.Printf("oauth: refresh token reuse for client %s; grant revoked", clientID)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token already used; authorization revoked")
		return
	case errors.Is(err, store.ErrNotFound):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired refresh token")
		return
	case err != nil:
		serverError(w, err)
		return
	}
	writeTokens(w, access, refresh, g.Scope)
}

func writeTokens(w http.ResponseWriter, access, refresh, scope string) {
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int64(accessTTL / time.Second),
		RefreshToken: refresh,
		Scope:        scope,
	})
}

func (s *Server) maybePurge(ctx context.Context) {
	now := s.now()
	last := s.lastPurge.Load()
	if now.Unix()-last < int64(purgeInterval/time.Second) || !s.lastPurge.CompareAndSwap(last, now.Unix()) {
		return
	}
	if err := s.store.PurgeExpiredOAuth(ctx, now); err != nil {
		log.Printf("oauth: purge expired: %v", err)
	}
}

// --- revocation ---

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	tok := r.PostForm.Get("token")
	if tok == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	if err := s.store.RevokeOAuthTokenGrant(r.Context(), hashToken(tok)); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// --- resource side ---

// grantKey is the TokenInfo.Extra key holding the grant id.
const grantKey = "grant_id"

// Verify is an auth.TokenVerifier for the MCP endpoint: it accepts unexpired
// access tokens issued for this resource.
func (s *Server) Verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	now := s.now()
	g, exp, err := s.store.LookupOAuthAccessToken(ctx, hashToken(token), now)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: unknown or expired token", auth.ErrInvalidToken)
	}
	if err != nil {
		return nil, err
	}
	if g.Resource != s.resource {
		return nil, fmt.Errorf("%w: token issued for another resource", auth.ErrInvalidToken)
	}
	if err := s.store.TouchOAuthGrant(ctx, g.ID, now); err != nil {
		log.Printf("oauth: touch grant %d: %v", g.ID, err)
	}
	return &auth.TokenInfo{
		Scopes:     strings.Fields(g.Scope),
		Expiration: exp,
		UserID:     strconv.FormatInt(g.UserID, 10),
		Extra:      map[string]any{grantKey: g.ID},
	}, nil
}

// UserID returns the id of the user an MCP request acts for, from the token
// info that the bearer middleware placed in ctx.
func UserID(ctx context.Context) (int64, bool) {
	ti := auth.TokenInfoFromContext(ctx)
	if ti == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(ti.UserID, 10, 64)
	return id, err == nil
}

// HasScope reports whether the request's token in ctx carries scope.
func HasScope(ctx context.Context, scope string) bool {
	ti := auth.TokenInfoFromContext(ctx)
	return ti != nil && slices.Contains(ti.Scopes, scope)
}

// --- helpers ---

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func pkceMatches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("oauth: %v", err)
	oauthError(w, http.StatusInternalServerError, "server_error", "internal error")
}
