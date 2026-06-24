// Package sanitize cleans user-supplied recipe-step HTML, allowing only a small
// set of formatting tags plus inline images that point at our own uploads.
package sanitize

import (
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// uploadSrc matches the only image sources we permit: files we stored under the
// uploads dir. This blocks data:, external hotlinks and javascript: URLs.
var uploadSrc = regexp.MustCompile(`^/uploads/[A-Za-z0-9._-]+$`)

// uploadRef extracts the basename of every permitted image reference in HTML.
var uploadRef = regexp.MustCompile(`/uploads/([A-Za-z0-9._-]+)`)

var policy = buildPolicy()

func buildPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	// Text formatting and simple structure a cook might use in steps.
	p.AllowElements("p", "br", "b", "strong", "i", "em", "u", "ol", "ul", "li", "h3", "blockquote", "div", "span")
	// Inline images restricted to our uploads.
	p.AllowAttrs("src").Matching(uploadSrc).OnElements("img")
	p.AllowAttrs("alt").OnElements("img")
	p.AllowRelativeURLs(true)
	p.RequireParseableURLs(true)
	return p
}

// StepsHTML returns a sanitized copy of the recipe-steps HTML.
func StepsHTML(in string) string {
	return policy.Sanitize(in)
}

// ImageFilenames returns the distinct upload basenames referenced by the
// (already sanitized) HTML, in first-seen order.
func ImageFilenames(html string) []string {
	matches := uploadRef.FindAllStringSubmatch(html, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// IsValidUploadName reports whether name is a safe upload basename (no path
// traversal, matches our generated pattern).
func IsValidUploadName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/\\") && uploadSrc.MatchString("/uploads/"+name)
}
