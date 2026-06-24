// Package auth provides password hashing and session key management for the
// admin area. Passwords are hashed with bcrypt; session and CSRF keys are
// generated once and persisted under the data dir.
package auth

import "golang.org/x/crypto/bcrypt"

// bcryptCost is deliberately above the library default for stronger password
// hashing, as required by the spec ("надёжная хеш-защита паролей").
const bcryptCost = 12

// HashPassword returns a bcrypt hash of the plaintext password.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword reports whether password matches the stored bcrypt hash.
// It is constant-time with respect to the hash (bcrypt guarantees this).
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
