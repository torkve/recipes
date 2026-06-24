package auth

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
)

// Keys holds the secret keys used for session cookies and CSRF protection.
// They are generated once on first start and persisted so that sessions and
// CSRF tokens survive restarts.
type Keys struct {
	// SessionAuth authenticates (HMAC) session cookies (64 bytes).
	SessionAuth []byte `json:"session_auth"`
	// SessionEnc encrypts session cookies (32 bytes => AES-256).
	SessionEnc []byte `json:"session_enc"`
	// CSRF is the 32-byte key for the gorilla/csrf middleware.
	CSRF []byte `json:"csrf"`
}

// LoadOrCreateKeys reads the keys from path, generating and persisting a fresh
// set (with 0600 perms) if the file does not yet exist.
func LoadOrCreateKeys(path string) (*Keys, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var k Keys
		if err := json.Unmarshal(data, &k); err != nil {
			return nil, fmt.Errorf("auth: parse keys %s: %w", path, err)
		}
		if len(k.SessionAuth) == 0 || len(k.SessionEnc) != 32 || len(k.CSRF) != 32 {
			return nil, fmt.Errorf("auth: keys file %s is malformed", path)
		}
		return &k, nil
	case os.IsNotExist(err):
		k, err := generateKeys()
		if err != nil {
			return nil, err
		}
		out, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, out, 0o600); err != nil {
			return nil, fmt.Errorf("auth: write keys %s: %w", path, err)
		}
		return k, nil
	default:
		return nil, fmt.Errorf("auth: read keys %s: %w", path, err)
	}
}

func generateKeys() (*Keys, error) {
	k := &Keys{
		SessionAuth: make([]byte, 64),
		SessionEnc:  make([]byte, 32),
		CSRF:        make([]byte, 32),
	}
	for _, buf := range [][]byte{k.SessionAuth, k.SessionEnc, k.CSRF} {
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("auth: generate keys: %w", err)
		}
	}
	return k, nil
}
