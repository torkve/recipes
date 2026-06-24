package auth

import (
	"context"
	"errors"
	"fmt"
	"log"

	"recipes/internal/store"
)

// BootstrapAdmin ensures an admin account exists. If a user with the given
// username is already present it is left untouched; otherwise a new admin is
// created with the given password. A blank username is a no-op (the operator
// chose not to bootstrap via env).
func BootstrapAdmin(ctx context.Context, st *store.Store, username, password string) error {
	if username == "" {
		return nil
	}
	if password == "" {
		return fmt.Errorf("auth: ADMIN_USERNAME set but ADMIN_PASSWORD is empty")
	}

	if _, err := st.GetUserByUsername(ctx, username); err == nil {
		return nil // already exists
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := st.CreateUser(ctx, username, hash, true); err != nil {
		return err
	}
	log.Printf("auth: bootstrapped admin user %q", username)
	return nil
}
