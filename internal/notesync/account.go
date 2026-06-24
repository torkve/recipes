package notesync

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// newAEAD builds an AES-256-GCM AEAD from a 32-byte key. Used to encrypt the
// persisted iCloud session blob at rest.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("notesync: sync key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealBlob encrypts plaintext, prefixing a random nonce.
func sealBlob(aead cipher.AEAD, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// openBlob decrypts a nonce-prefixed ciphertext produced by sealBlob.
func openBlob(aead cipher.AEAD, ct []byte) ([]byte, error) {
	ns := aead.NonceSize()
	if len(ct) < ns {
		return nil, errors.New("notesync: ciphertext too short")
	}
	nonce, body := ct[:ns], ct[ns:]
	return aead.Open(nil, nonce, body, nil)
}
