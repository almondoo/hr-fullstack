// Package config loads typed application configuration from environment variables.
// Required values cause startup failure when absent — fail-fast to prevent
// running with an incomplete configuration.
//
// Security note: never log Config values (passwords, session keys) — pass
// the struct by value to callers that need individual fields.
package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Config holds all application configuration loaded from environment variables.
// Tag `env:"VAR,required"` causes immediate failure when the variable is unset.
// Tag `env:"VAR,notEmpty"` additionally rejects empty strings.
type Config struct {
	// --- Application ---
	AppEnv   string `env:"APP_ENV"   envDefault:"development"`
	HTTPPort string `env:"HTTP_PORT" envDefault:"8080"`

	// --- Database (all required for a running server) ---
	DBHost     string `env:"DB_HOST"     envDefault:"localhost"`
	DBPort     string `env:"DB_PORT"     envDefault:"5432"`
	DBUser     string `env:"DB_USER"     envDefault:"hr_app"`
	DBPassword string `env:"DB_PASSWORD,required,notEmpty"`
	DBName     string `env:"DB_NAME"     envDefault:"hr_saas"`
	DBSSLMode  string `env:"DB_SSLMODE"  envDefault:"disable"`

	// DATABASE_URL overrides individual DB_* fields when set.
	// Useful in Docker Compose / hosted environments.
	DatabaseURL string `env:"DATABASE_URL"`

	// --- CORS ---
	// CORSAllowOrigins is a comma-separated list of allowed origins.
	// Defaults to the local frontend dev server in development.
	// In non-development environments the server refuses to start when this is
	// empty (fail-fast) to avoid running with no CORS policy at all.
	CORSAllowOrigins string `env:"CORS_ALLOW_ORIGINS" envDefault:"http://localhost:3000"`

	// --- Session / cookie keys (placeholders — populated in auth slice) ---
	// SessionHashKey is used for HMAC signing of session cookies.
	// Must be at least 32 bytes. Required when session auth is enabled.
	// Placeholder: not enforced here because auth is added in a later slice.
	SessionHashKey string `env:"SESSION_HASH_KEY"`

	// SessionBlockKey is used for AES encryption of session cookies.
	// Must be 16, 24, or 32 bytes. Required when session auth is enabled.
	// Placeholder: not enforced here because auth is added in a later slice.
	SessionBlockKey string `env:"SESSION_BLOCK_KEY"`
}

// Load reads Config from environment variables.
// Returns an error if any required variable is missing or invalid.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("config: parse env: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: validation: %w", err)
	}
	return cfg, nil
}

// DSN returns the PostgreSQL data source name.
// If DatabaseURL is set it takes precedence over individual DB_* fields.
func (c *Config) DSN() string {
	if c.DatabaseURL != "" {
		return c.DatabaseURL
	}
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode,
	)
}

// IsDevelopment reports whether the application is running in development mode.
func (c *Config) IsDevelopment() bool {
	return c.AppEnv == "development"
}

// validate performs semantic checks beyond what env tag parsing covers.
func (c *Config) validate() error {
	var errs []error

	if c.HTTPPort == "" {
		errs = append(errs, errors.New("HTTP_PORT must not be empty"))
	}
	if c.DBSSLMode != "disable" && c.DBSSLMode != "require" &&
		c.DBSSLMode != "verify-ca" && c.DBSSLMode != "verify-full" {
		errs = append(errs, fmt.Errorf("DB_SSLMODE %q is not a valid value", c.DBSSLMode))
	}
	// In non-development environments, require an explicit CORS origin allowlist.
	// An empty value would leave the server with no CORS policy, which is a
	// misconfiguration error — fail fast rather than silently reject all requests.
	if !c.IsDevelopment() && strings.TrimSpace(c.CORSAllowOrigins) == "" {
		errs = append(errs, errors.New("CORS_ALLOW_ORIGINS must not be empty in non-development environments"))
	}

	return errors.Join(errs...)
}
