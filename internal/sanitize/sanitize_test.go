package sanitize

import (
	"strings"
	"testing"
)

func TestStepsHTMLStripsScripts(t *testing.T) {
	out := StepsHTML(`<p>Готовим борщ.</p><script>alert(1)</script>`)
	if strings.Contains(out, "<script") {
		t.Fatalf("script tag survived: %q", out)
	}
	if !strings.Contains(out, "Готовим борщ") {
		t.Fatalf("safe text removed: %q", out)
	}
}

func TestStepsHTMLImagePolicy(t *testing.T) {
	out := StepsHTML(`<img src="/uploads/a.png"><img src="javascript:alert(1)"><img src="https://evil.test/x.png">`)
	if !strings.Contains(out, `/uploads/a.png`) {
		t.Fatalf("permitted upload image removed: %q", out)
	}
	if strings.Contains(out, "javascript:") {
		t.Fatalf("javascript: src survived: %q", out)
	}
	if strings.Contains(out, "evil.test") {
		t.Fatalf("external image src survived: %q", out)
	}
}

func TestImageFilenames(t *testing.T) {
	html := `x <img src="/uploads/a.png"> y <img src="/uploads/b.jpg"> z <img src="/uploads/a.png">`
	got := ImageFilenames(html)
	want := []string{"a.png", "b.jpg"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestIsValidUploadName(t *testing.T) {
	cases := map[string]bool{
		"abc.png":     true,
		"a-b_c.webp":  true,
		"":            false,
		"a/b.png":     false,
		`a\b.png`:     false,
		"../x.png":    false,
		"..":          false,
		"a b.png":     false,
	}
	for name, want := range cases {
		if got := IsValidUploadName(name); got != want {
			t.Errorf("IsValidUploadName(%q)=%v want %v", name, got, want)
		}
	}
}
