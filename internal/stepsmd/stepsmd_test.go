package stepsmd

import (
	"strings"
	"testing"

	"recipes/internal/sanitize"
)

const base = "https://recipes.example.com"

func TestToMarkdown(t *testing.T) {
	tests := []struct {
		name, html, want string
	}{
		{"paragraphs", "<p>Один</p><p>Два</p>", "Один\n\nДва"},
		{"emphasis", "<p>Взять <b>муку</b> и <em>соль</em></p>", "Взять **муку** и *соль*"},
		{"emphasis keeps edge spaces outside", "<p>a<strong> b </strong>c</p>", "a **b** c"},
		{"line breaks", "<p>раз<br>два<br><br>три<br></p>", "раз\\\nдва\\\nтри"},
		{"ordered list", "<ol><li>Нарезать</li><li>Варить</li></ol>", "1. Нарезать\n2. Варить"},
		{"nested list", "<ul><li>а<ul><li>б</li></ul></li></ul>", "- а\n  - б"},
		{"heading", "<h3>Соус</h3><p>текст</p>", "### Соус\n\nтекст"},
		{"blockquote", "<blockquote><p>совет</p></blockquote>", "> совет"},
		{"divs from notes", "<div>строка 1</div><div><br></div><div>строка 2</div>", "строка 1\n\nстрока 2"},
		{"underline and span lose markup", "<p><u>важно</u> <span>тут</span></p>", "важно тут"},
		{"image absolute", `<p><img src="/uploads/a.png" alt="фото"></p>`, "![фото](" + base + "/uploads/a.png)"},
		{"escapes markdown chars", "<p>2*3 = 6_x [a]</p>", `2\*3 = 6\_x \[a\]`},
		{"escapes heading start", "<p># не заголовок</p>", `\# не заголовок`},
		{"escapes list start", "<p>- не список</p><p>1. не список</p>", "\\- не список\n\n1\\. не список"},
		{"escapes bare dash and plus", "<p>-</p><p>+</p>", "\\-\n\n\\+"},
		{"escapes thematic break", "<p>---</p>", `\---`},
		{"escapes code fence", "<p>~~~</p><p>после</p>", "\\~~~\n\nпосле"},
		{"escapes setext underline after br", "<p>Тесто<br>---</p>", "Тесто\\\n\\---"},
		{"escapes entity-like text", "<p>&amp;copy;</p>", `\&copy;`},
		{"escapes trailing hash in heading", "<h3>Номер #</h3>", `### Номер \#`},
		{"normalizes image alt", "<p><img src=\"/uploads/a.png\" alt=\"x\n\n# h\"></p>", "![x # h](" + base + "/uploads/a.png)"},
		{"strips NUL from alt", "<p><img src=\"/uploads/a.png\" alt=\"a\x00b\"></p>", "![ab](" + base + "/uploads/a.png)"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToMarkdown(tt.html, base); got != tt.want {
				t.Errorf("ToMarkdown(%q)\n got: %q\nwant: %q", tt.html, got, tt.want)
			}
		})
	}
}

func TestToHTML(t *testing.T) {
	tests := []struct {
		name, md   string
		want, deny []string
	}{
		{name: "heading clamped to h3", md: "# Тесто", want: []string{"<h3>Тесто</h3>"}},
		{name: "hard wraps", md: "раз\nдва", want: []string{"раз<br"}},
		{name: "own image made relative", md: "![x](" + base + "/uploads/a.png)", want: []string{`src="/uploads/a.png"`}},
		{name: "foreign image kept absolute", md: "![x](https://evil.example/a.png)", want: []string{`src="https://evil.example/a.png"`}},
		{name: "raw html dropped", md: "<script>alert(1)</script>\n\nтекст", want: []string{"текст"}, deny: []string{"<script"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToHTML(tt.md, base)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("ToHTML(%q) = %q, want it to contain %q", tt.md, got, w)
				}
			}
			for _, d := range tt.deny {
				if strings.Contains(got, d) {
					t.Errorf("ToHTML(%q) = %q, must not contain %q", tt.md, got, d)
				}
			}
		})
	}
}

// TestRoundTrip pins that sanitized HTML using only Markdown-expressible markup
// survives HTML → Markdown → HTML → sanitize with the same rendered text and
// structure.
func TestRoundTrip(t *testing.T) {
	inputs := []string{
		"<p>Взять <strong>муку</strong>, <em>соль</em> и 2*2 яйца.</p>",
		"<ol><li>Нарезать</li><li>Варить <strong>40 минут</strong></li></ol>",
		"<ul><li>а<ul><li>б</li></ul></li></ul>",
		"<h3>Соус</h3><p>раз<br>два</p>",
		`<p><img src="/uploads/a.png" alt="фото"></p>`,
		"<blockquote><p>совет</p></blockquote>",
		"<p># не заголовок</p><p>1. не список</p>",
		"<p>-</p><p>---</p><p>~~~</p><p>после</p>",
		"<p>Тесто<br>---</p>",
		"<p>&amp;copy; 2&lt;3</p>",
		"<h3>Номер #</h3>",
	}
	for _, in := range inputs {
		clean := sanitize.StepsHTML(in)
		md := ToMarkdown(clean, base)
		back, err := ToHTML(md, base)
		if err != nil {
			t.Fatal(err)
		}
		again := sanitize.StepsHTML(back)
		if got, want := ToMarkdown(again, base), md; got != want {
			t.Errorf("round trip of %q not stable:\n first md: %q\nsecond md: %q\n    html: %q", in, want, got, again)
		}
	}
}
