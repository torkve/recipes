package auth

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
)

// Keys holds the secret keys used for session cookies and iCloud blob
// encryption. They are generated once on first start and persisted so sessions
// survive restarts. (CSRF protection no longer needs a key — it uses the stdlib
// net/http.CrossOriginProtection; a legacy "csrf" field in keys.json is ignored.)
type Keys struct {
	// SessionAuth authenticates (HMAC) session cookies (64 bytes).
	SessionAuth []byte `json:"session_auth"`
	// SessionEnc encrypts session cookies (32 bytes => AES-256).
	SessionEnc []byte `json:"session_enc"`
	// SyncEnc encrypts persisted iCloud session blobs (32 bytes => AES-256-GCM).
	SyncEnc []byte `json:"sync_enc"`
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
		if len(k.SessionAuth) == 0 || len(k.SessionEnc) != 32 {
			return nil, fmt.Errorf("auth: keys file %s is malformed", path)
		}
		// Backfill SyncEnc for key files created before iCloud sync existed,
		// without rotating the cookie keys.
		if len(k.SyncEnc) != 32 {
			k.SyncEnc = make([]byte, 32)
			if _, err := rand.Read(k.SyncEnc); err != nil {
				return nil, fmt.Errorf("auth: generate sync key: %w", err)
			}
			out, err := json.Marshal(&k)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, out, 0o600); err != nil {
				return nil, fmt.Errorf("auth: rewrite keys %s: %w", path, err)
			}
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
		SyncEnc:     make([]byte, 32),
	}
	for _, buf := range [][]byte{k.SessionAuth, k.SessionEnc, k.SyncEnc} {
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("auth: generate keys: %w", err)
		}
	}
	return k, nil
}
