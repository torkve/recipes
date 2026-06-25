// Package config loads service configuration from environment variables.
//
// All values have sensible defaults so the service can boot with zero config
// for local development; secrets (admin bootstrap, session keys) are supplied
// via environment in production.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds all runtime configuration.
type Config struct {
	// Addr is the TCP listen address, e.g. ":8080".
	Addr string
	// DataDir holds the sqlite database, uploaded images and session files.
	// Archiving this directory is the supported backup mechanism.
	DataDir string
	// SiteName is shown in the header next to the logo.
	SiteName string

	// AdminUsername / AdminPassword bootstrap the first admin user on first
	// start. If the user already exists it is left untouched.
	AdminUsername string
	AdminPassword string

	// SecureCookies marks session/CSRF cookies as Secure (HTTPS only).
	// Disable for plain-HTTP local development.
	SecureCookies bool

	// ICloudEnabled turns on the iCloud Notes sync feature (admin UI, routes
	// and the background pull worker). Default off: the reverse-engineered
	// iCloud client is shipped dark until explicitly enabled with credentials.
	ICloudEnabled bool
	// ICloudPullMinutes is the background pull interval in minutes.
	ICloudPullMinutes int
	// ICloudSRPVariant selects the SRP byte convention for Apple sign-in. Apple
	// throttles repeated attempts, so we try one per bind; if sign-in fails with
	// 401, set the next index and restart. Default 1 (pyicloud-style).
	ICloudSRPVariant int

	// EmbedURL is the base URL of the self-hosted embedding service (e.g. HF
	// text-embeddings-inference). Empty disables semantic search (lexical-only).
	EmbedURL string
	// EmbedModel tags stored vectors; changing it triggers a re-embed backfill.
	EmbedModel string
	// EmbedDim is the embedding dimension the model returns. Change it together
	// with EmbedModel: stored vectors are keyed by model name, so a dim change
	// under the same model leaves old vectors that silently mismatch until the
	// backfill re-embeds.
	EmbedDim int
	// EmbedBackfillMinutes is how often the backfill worker indexes new recipes.
	EmbedBackfillMinutes int
}

// DBPath returns the sqlite database file path.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "recipes.db") }

// UploadsDir returns the directory holding uploaded recipe images.
func (c *Config) UploadsDir() string { return filepath.Join(c.DataDir, "uploads") }

// SessionsDir returns the directory holding filesystem session files.
func (c *Config) SessionsDir() string { return filepath.Join(c.DataDir, "sessions") }

// KeysPath returns the file holding persisted session/CSRF keys.
func (c *Config) KeysPath() string { return filepath.Join(c.DataDir, "keys.json") }

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Addr:              env("RECIPES_ADDR", ":8080"),
		DataDir:           env("RECIPES_DATA_DIR", "./data"),
		SiteName:          env("RECIPES_SITE_NAME", "Семейная кулинарная книга"),
		AdminUsername:     strings.TrimSpace(os.Getenv("ADMIN_USERNAME")),
		AdminPassword:     os.Getenv("ADMIN_PASSWORD"),
		SecureCookies:     envBool("RECIPES_SECURE_COOKIES", false),
		ICloudEnabled:     envBool("RECIPES_ICLOUD_ENABLED", false),
		ICloudPullMinutes: envInt("RECIPES_ICLOUD_PULL_MINUTES", 15),
		ICloudSRPVariant:  envInt("RECIPES_ICLOUD_SRP_VARIANT", 1),

		EmbedURL:             env("RECIPES_EMBED_URL", ""),
		EmbedModel:           env("RECIPES_EMBED_MODEL", "intfloat/multilingual-e5-small"),
		EmbedDim:             envInt("RECIPES_EMBED_DIM", 384),
		EmbedBackfillMinutes: envInt("RECIPES_EMBED_BACKFILL_MINUTES", 5),
	}

	if c.Addr == "" {
		return nil, fmt.Errorf("config: RECIPES_ADDR must not be empty")
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("config: RECIPES_DATA_DIR must not be empty")
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
