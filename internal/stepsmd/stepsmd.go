// Package stepsmd converts recipe-step HTML (the subset allowed by package
// sanitize) to Markdown and back, for clients that read and write Markdown.
//
// Image references are relative "/uploads/<name>" paths in stored HTML and
// absolute "<baseURL>/uploads/<name>" URLs in Markdown. Formatting outside
// Markdown's vocabulary (u, span, div boundaries) does not survive a round trip.
package stepsmd

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	uploadsPath = "/uploads/"
	// brBreak marks a <br> inside an inline run until cleanParagraph turns it
	// into a Markdown hard break. escapeText strips it from all text and
	// attribute content, so only <br> elements produce it.
	brBreak = "\x00"
)

// ToMarkdown converts sanitized steps HTML to Markdown, making upload image
// URLs absolute under baseURL (scheme://host, no trailing slash).
func ToMarkdown(stepsHTML, baseURL string) string {
	root := &xhtml.Node{Type: xhtml.ElementNode, DataAtom: atom.Div, Data: "div"}
	nodes, err := xhtml.ParseFragment(strings.NewReader(stepsHTML), root)
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	c := converter{baseURL: strings.TrimRight(baseURL, "/")}
	return strings.Join(c.blocks(root), "\n\n")
}

// ToHTML renders Markdown to HTML, mapping absolute upload image URLs under
// baseURL back to relative "/uploads/<name>" paths and clamping every heading
// to h3. Raw HTML in the input is dropped. The output is not sanitized; callers
// must pass it through sanitize.StepsHTML before storing.
func ToHTML(md, baseURL string) (string, error) {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	gm := goldmark.New(
		goldmark.WithParserOptions(parser.WithASTTransformers(
			util.Prioritized(rewriter{baseURL: strings.TrimRight(baseURL, "/")}, 100),
		)),
		goldmark.WithRendererOptions(html.WithHardWraps()),
	)
	var buf bytes.Buffer
	if err := gm.Convert([]byte(md), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// rewriter is a goldmark AST transformer applying ToHTML's image and heading
// mappings.
type rewriter struct{ baseURL string }

func (rw rewriter) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			n.Level = 3
		case *ast.Image:
			if rw.baseURL != "" {
				if rest, ok := strings.CutPrefix(string(n.Destination), rw.baseURL+uploadsPath); ok {
					n.Destination = []byte(uploadsPath + rest)
				}
			}
		}
		return ast.WalkContinue, nil
	})
}

type converter struct{ baseURL string }

// isBlock reports whether n renders as its own Markdown block.
func isBlock(n *xhtml.Node) bool {
	if n.Type != xhtml.ElementNode {
		return false
	}
	switch n.DataAtom {
	case atom.P, atom.Div, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Ul, atom.Ol, atom.Blockquote:
		return true
	}
	return false
}

// blocks renders the children of n as a list of Markdown blocks, grouping runs
// of inline children into paragraphs.
func (c converter) blocks(n *xhtml.Node) []string {
	var out []string
	var inline strings.Builder
	flush := func() {
		if p := cleanParagraph(inline.String()); p != "" {
			out = append(out, p)
		}
		inline.Reset()
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if !isBlock(ch) {
			inline.WriteString(c.inline(ch))
			continue
		}
		flush()
		if b := c.block(ch); b != "" {
			out = append(out, b)
		}
	}
	flush()
	return out
}

func (c converter) block(n *xhtml.Node) string {
	switch n.DataAtom {
	case atom.Ul, atom.Ol:
		return c.list(n)
	case atom.Blockquote:
		inner := strings.Join(c.blocks(n), "\n\n")
		if inner == "" {
			return ""
		}
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			if l == "" {
				lines[i] = ">"
			} else {
				lines[i] = "> " + l
			}
		}
		return strings.Join(lines, "\n")
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		h := cleanParagraph(c.inlineChildren(n))
		if h == "" {
			return ""
		}
		h = strings.ReplaceAll(h, "\\\n", " ")
		if strings.HasSuffix(h, "#") {
			h = h[:len(h)-1] + `\#` // a trailing # would close the ATX heading
		}
		return "### " + h
	default: // p, div
		return strings.Join(c.blocks(n), "\n\n")
	}
}

func (c converter) list(n *xhtml.Node) string {
	ordered := n.DataAtom == atom.Ol
	var items []string
	num := 1
	for li := n.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != xhtml.ElementNode || li.DataAtom != atom.Li {
			continue
		}
		marker := "- "
		if ordered {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		body := strings.Join(c.blocks(li), "\n")
		indent := strings.Repeat(" ", len(marker))
		lines := strings.Split(body, "\n")
		for i := range lines {
			if i > 0 && lines[i] != "" {
				lines[i] = indent + lines[i]
			}
		}
		items = append(items, marker+strings.Join(lines, "\n"))
	}
	return strings.Join(items, "\n")
}

func (c converter) inlineChildren(n *xhtml.Node) string {
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		b.WriteString(c.inline(ch))
	}
	return b.String()
}

// inline renders n as inline Markdown. Block elements nested in inline context
// contribute their text only.
func (c converter) inline(n *xhtml.Node) string {
	switch n.Type {
	case xhtml.TextNode:
		return escapeText(collapseSpace(n.Data))
	case xhtml.ElementNode:
	default:
		return ""
	}
	switch n.DataAtom {
	case atom.Br:
		return brBreak
	case atom.B, atom.Strong:
		return wrap(c.inlineChildren(n), "**")
	case atom.I, atom.Em:
		return wrap(c.inlineChildren(n), "*")
	case atom.Img:
		src, alt := attr(n, "src"), attr(n, "alt")
		if strings.HasPrefix(src, uploadsPath) {
			src = c.baseURL + src
		}
		if src == "" {
			return ""
		}
		return "![" + escapeText(strings.TrimSpace(collapseSpace(alt))) + "](" + src + ")"
	default:
		return c.inlineChildren(n)
	}
}

// wrap surrounds the non-blank inner text with delim, keeping edge spaces
// outside the delimiters as Markdown emphasis requires.
func wrap(inner, delim string) string {
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" {
		return inner
	}
	lead := inner[:len(inner)-len(strings.TrimLeft(inner, " \n"))]
	trail := inner[len(strings.TrimRight(inner, " \n")):]
	return lead + delim + trimmed + delim + trail
}

func attr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

var spaceRun = regexp.MustCompile(`[ \t\r\n\f]+`)

func collapseSpace(s string) string { return spaceRun.ReplaceAllString(s, " ") }

var mdSpecial = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "`", "\\`", "[", `\[`, "]", `\]`, "<", `\<`, "&", `\&`,
)

// escapeText escapes Markdown inline syntax in s and removes NUL, which is
// reserved for brBreak.
func escapeText(s string) string {
	return mdSpecial.Replace(strings.ReplaceAll(s, brBreak, ""))
}

var (
	// punctLineStart matches a line start Markdown may read as block syntax
	// (heading, quote, list, thematic break, setext underline, code fence); it is
	// escaped by a backslash before its first character.
	punctLineStart = regexp.MustCompile(`^[#>+=~-]`)
	// orderedLineStart matches an ordered-list marker; its delimiter is escaped.
	orderedLineStart = regexp.MustCompile(`^(\d+)([.)])(\s|$)`)
)

// cleanParagraph turns an inline run into one Markdown paragraph: lines (split
// at <br> breaks) are trimmed, blank ones dropped, block-syntax line starts
// escaped, and the remaining lines joined with hard breaks.
func cleanParagraph(s string) string {
	var out []string
	for _, l := range strings.Split(s, brBreak) {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if punctLineStart.MatchString(l) {
			l = `\` + l
		} else if m := orderedLineStart.FindStringSubmatchIndex(l); m != nil {
			l = l[:m[4]] + `\` + l[m[4]:]
		}
		out = append(out, l)
	}
	return strings.Join(out, "\\\n")
}
