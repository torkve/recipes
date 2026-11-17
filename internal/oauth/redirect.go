package oauth

import (
	"net/url"
	"strings"
)

// DefaultRedirectPatterns are the redirect URIs clients may register when no
// allowlist is configured: the Claude and ChatGPT connector callbacks, and any
// loopback URI for local tools such as the MCP Inspector.
var DefaultRedirectPatterns = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
	"https://chatgpt.com/connector_platform_oauth_redirect",
	"https://chatgpt.com/connector/oauth/*",
	"http://localhost:*",
	"http://127.0.0.1:*",
}

// redirectPattern is a parsed allowlist entry.
//
// Pattern syntax: an absolute URL whose port may be "*" (any port) and whose
// path may end in "*" (path prefix). An empty path matches any path.
type redirectPattern struct {
	scheme, host string
	port         string // "" = default port only
	anyPort      bool
	path         string
	pathPrefix   bool
	anyPath      bool
}

func parsePattern(p string) (redirectPattern, bool) {
	var rp redirectPattern
	scheme, rest, ok := strings.Cut(p, "://")
	if !ok {
		return rp, false
	}
	hostport, path, _ := strings.Cut(rest, "/")
	if path != "" || strings.HasSuffix(rest, "/") {
		path = "/" + path
	}
	host, port, hasPort := strings.Cut(hostport, ":")
	rp.scheme, rp.host = strings.ToLower(scheme), strings.ToLower(host)
	if hasPort {
		if port == "*" {
			rp.anyPort = true
		} else {
			rp.port = port
		}
	}
	switch {
	case path == "":
		rp.anyPath = true
	case strings.HasSuffix(path, "*"):
		rp.path, rp.pathPrefix = strings.TrimSuffix(path, "*"), true
	default:
		rp.path = path
	}
	return rp, rp.scheme != "" && rp.host != ""
}

func (rp redirectPattern) matches(u *url.URL) bool {
	if strings.ToLower(u.Scheme) != rp.scheme || strings.ToLower(u.Hostname()) != rp.host {
		return false
	}
	if !rp.anyPort && u.Port() != rp.port {
		return false
	}
	switch {
	case rp.anyPath:
		return true
	case rp.pathPrefix:
		return strings.HasPrefix(u.Path, rp.path)
	default:
		return u.Path == rp.path
	}
}

// redirectAllowed reports whether raw is an absolute redirect URI with no
// userinfo or fragment, a canonical path, that matches one of the patterns.
//
// A canonical path has no percent-escapes beyond the default encoding (so no
// encoded dots or slashes), no "." or ".." segment and no backslash: browsers
// resolve all of these, which would let a path escape a prefix pattern.
func redirectAllowed(patterns []redirectPattern, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.RawPath != "" || strings.Contains(u.Path, `\`) {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	for _, p := range patterns {
		if p.matches(u) {
			return true
		}
	}
	return false
}
