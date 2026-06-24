package icloud

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSigninInit(t *testing.T) {
	salt := []byte("sixteen-byte-slt")
	B := make([]byte, 256)
	B[0] = 0x42
	body, _ := json.Marshal(map[string]any{
		"iteration": 20081,
		"salt":      base64.StdEncoding.EncodeToString(salt),
		"protocol":  "s2k",
		"b":         base64.StdEncoding.EncodeToString(B),
		"c":         "d-255-abc:RNO",
	})

	gotSalt, gotB, iter, protocol, c, err := parseSigninInit(body)
	if err != nil {
		t.Fatal(err)
	}
	if iter != 20081 || protocol != "s2k" || c != "d-255-abc:RNO" {
		t.Fatalf("bad fields: iter=%d protocol=%q c=%q", iter, protocol, c)
	}
	if string(gotSalt) != string(salt) || len(gotB) != 256 || gotB[0] != 0x42 {
		t.Fatalf("bad salt/B decode")
	}
}

func TestParseSigninInitIncomplete(t *testing.T) {
	body := []byte(`{"iteration":0,"salt":"","protocol":"s2k","b":"","c":""}`)
	if _, _, _, _, _, err := parseSigninInit(body); err == nil {
		t.Fatal("expected error for incomplete init response")
	}
}

func TestBuildSigninInitBody(t *testing.T) {
	b, err := buildSigninInitBody("user@example.com", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["accountName"] != "user@example.com" {
		t.Fatalf("accountName missing: %v", m)
	}
	if m["a"] != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) {
		t.Fatalf("a not base64-encoded: %v", m["a"])
	}
	protos, _ := m["protocols"].([]any)
	if len(protos) != 2 || protos[0] != "s2k" || protos[1] != "s2k_fo" {
		t.Fatalf("protocols wrong: %v", m["protocols"])
	}
}

func TestBuildSigninCompleteBody(t *testing.T) {
	b, err := buildSigninCompleteBody("user@example.com", "the-c", []byte{1}, []byte{2})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"accountName":"user@example.com"`, `"c":"the-c"`, `"m1":"`, `"m2":"`, `"trustTokens":[]`} {
		if !strings.Contains(s, want) {
			t.Fatalf("complete body missing %s: %s", want, s)
		}
	}
}

func TestAuthHeadersComplete(t *testing.T) {
	h := authHeaders("frame-123")
	for _, k := range []string{
		"X-Apple-Widget-Key", "X-Apple-OAuth-State", "X-Apple-Frame-Id",
		"X-Apple-OAuth-Require-Grant-Code", "X-Apple-I-FD-Client-Info", "X-Requested-With",
	} {
		if h[k] == "" {
			t.Errorf("missing header %s", k)
		}
	}
	if h["X-Apple-OAuth-State"] != "frame-123" || h["X-Apple-Frame-Id"] != "frame-123" {
		t.Fatalf("frame id not propagated: %v / %v", h["X-Apple-OAuth-State"], h["X-Apple-Frame-Id"])
	}
}
