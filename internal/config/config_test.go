package config

import "testing"

func TestNormalizePublicURL(t *testing.T) {
	ok := map[string]string{
		"":                                 "",
		"https://Recipes.Example.com":      "https://recipes.example.com",
		"https://recipes.example.com/":     "https://recipes.example.com",
		"https://recipes.example.com:8443": "https://recipes.example.com:8443",
		"http://localhost:8080":            "http://localhost:8080",
		"http://127.0.0.1:8080/":           "http://127.0.0.1:8080",
	}
	for in, want := range ok {
		got, err := normalizePublicURL(in)
		if err != nil || got != want {
			t.Errorf("normalizePublicURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"recipes.example.com",
		"http://recipes.example.com",
		"ftp://recipes.example.com",
		"https://recipes.example.com/sub",
		"https://recipes.example.com/?x=1",
		"https://recipes.example.com/#f",
		"https://user@recipes.example.com",
	}
	for _, in := range bad {
		if got, err := normalizePublicURL(in); err == nil {
			t.Errorf("normalizePublicURL(%q) = %q, want error", in, got)
		}
	}
}
