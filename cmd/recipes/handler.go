package main

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"recipes/internal/config"
	"recipes/internal/cookbook"
	"recipes/internal/mcpserver"
	"recipes/internal/oauth"
	"recipes/internal/search"
	"recipes/internal/store"
	"recipes/internal/web"
)

// newMCPServer builds the MCP server, authorizing writes by the request's
// OAuth scope and attributing created recipes to the token's user.
func newMCPServer(st *store.Store, searchSvc *search.Service, cfg *config.Config) *mcp.Server {
	return mcpserver.New(mcpserver.Deps{
		Store:    st,
		Search:   searchSvc,
		Cookbook: cookbook.New(st, cfg.UploadsDir()),
		BaseURL:  cfg.PublicURL,
		CanWrite: func(ctx context.Context) bool { return oauth.HasScope(ctx, oauth.ScopeWrite) },
		UserID:   oauth.UserID,
	})
}

// buildHandler returns the service's root handler. With authz nil it is the
// site alone. Otherwise the OAuth endpoints and the bearer-protected MCP
// endpoint are mounted beside the site, outside its session and cross-origin
// middleware: they authenticate by request parameters and bearer tokens only.
func buildHandler(site http.Handler, authz *oauth.Server, mcpSrv *mcp.Server) http.Handler {
	if authz == nil {
		return site
	}
	mux := http.NewServeMux()
	authz.Register(mux)
	requireToken := auth.RequireBearerToken(authz.Verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: authz.ResourceMetadataURL(),
		Scopes:              []string{oauth.ScopeRead},
	})
	mux.Handle(oauth.MCPPath, requireToken(mcpserver.Handler(mcpSrv)))
	machine := web.Logging(mux)

	root := http.NewServeMux()
	root.Handle("/.well-known/", machine)
	root.Handle("/oauth/register", machine)
	root.Handle("/oauth/token", machine)
	root.Handle("/oauth/revoke", machine)
	root.Handle(oauth.MCPPath, machine)
	root.Handle("/", site)
	return root
}
