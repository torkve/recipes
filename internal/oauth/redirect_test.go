package oauth

import "testing"

func TestRedirectAllowed(t *testing.T) {
	var pats []redirectPattern
	for _, p := range DefaultRedirectPatterns {
		rp, ok := parsePattern(p)
		if !ok {
			t.Fatalf("default pattern %q does not parse", p)
		}
		pats = append(pats, rp)
	}
	allowed := []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://claude.com/api/mcp/auth_callback",
		"https://CLAUDE.AI/api/mcp/auth_callback",
		"https://chatgpt.com/connector_platform_oauth_redirect",
		"https://chatgpt.com/connector/oauth/abc123",
		"http://localhost:6274/oauth/callback",
		"http://127.0.0.1:33418/",
		"http://localhost/cb",
	}
	for _, u := range allowed {
		if !redirectAllowed(pats, u) {
			t.Errorf("%q rejected, want allowed", u)
		}
	}
	denied := []string{
		"",
		"/relative/cb",
		"https://claude.ai/api/mcp/auth_callback/extra",
		"https://claude.ai:8443/api/mcp/auth_callback",
		"http://claude.ai/api/mcp/auth_callback",
		"https://evil.claude.ai/api/mcp/auth_callback",
		"https://claude.ai.evil.example/api/mcp/auth_callback",
		"https://chatgpt.com/connector/other",
		"https://chatgpt.com/connector/oauth/../../evil",
		"https://chatgpt.com/connector/oauth/%2e%2e/evil",
		"https://chatgpt.com/connector/oauth/.%2e/.%2e/evil",
		"https://chatgpt.com/connector/oauth/%2E%2e/evil",
		"https://chatgpt.com/connector/oauth/%2e./evil",
		"https://chatgpt.com/connector/oauth/./x",
		"https://chatgpt.com/connector/oauth/..",
		"https://chatgpt.com/connector/oauth/x%2F..%2F..%2Fevil",
		`https://chatgpt.com/connector/oauth/..\evil`,
		"https://chatgpt.com/connector/oauth/%5c..%5cevil",
		"https://claude.ai/api/mcp/auth_callback#frag",
		"http://localhost:1@evil.example/cb",
		"https://user@claude.ai/api/mcp/auth_callback",
		"https://localhost:8080/cb",
		"javascript:alert(1)",
	}
	for _, u := range denied {
		if redirectAllowed(pats, u) {
			t.Errorf("%q allowed, want rejected", u)
		}
	}
}

func TestNormalizeScope(t *testing.T) {
	cases := map[string]string{
		"":                           "recipes:read recipes:write",
		"offline_access":             "recipes:read recipes:write",
		"recipes:read":               "recipes:read",
		"recipes:write":              "recipes:read recipes:write",
		"recipes:write recipes:read": "recipes:read recipes:write",
		"recipes:read openid":        "recipes:read",
	}
	for in, want := range cases {
		if got := normalizeScope(in); got != want {
			t.Errorf("normalizeScope(%q) = %q, want %q", in, got, want)
		}
	}
}
