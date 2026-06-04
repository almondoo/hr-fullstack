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
	"net/url"
	"strings"
	"time"

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

	// --- Admin / migration credentials ---
	// DB_ADMIN_USER and DB_ADMIN_PASSWORD identify the superuser role used by
	// the migrate command and the startup migration runner. They default to the
	// application credentials when unset so that single-user local setups work
	// without extra configuration.
	DBAdminUser     string `env:"DB_ADMIN_USER"`
	DBAdminPassword string `env:"DB_ADMIN_PASSWORD"`

	// ADMIN_DATABASE_URL overrides individual DB_ADMIN_* fields when set.
	AdminDatabaseURL string `env:"ADMIN_DATABASE_URL"`

	// --- Migration on startup ---
	// MigrateOnStartup causes the server to run goose.Up using the admin DSN
	// before accepting traffic.
	//
	// Defaults to false (safe for production).  Development setups should set
	// MIGRATE_ON_STARTUP=true explicitly (docker-compose.yml does this).
	// Running auto-migration in production across multiple instances can cause
	// lock contention; prefer a dedicated pre-deploy migration step instead.
	MigrateOnStartup bool `env:"MIGRATE_ON_STARTUP" envDefault:"false"`

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

	// --- Session / Cookie configuration (P1.3a auth infrastructure) ---

	// SessionCookieName is the name of the HTTP-only session cookie.
	// Defaults to "hr_session".
	SessionCookieName string `env:"SESSION_COOKIE_NAME" envDefault:"hr_session"`

	// SessionTTL controls how long a session remains valid.
	// Accepts any duration string accepted by time.ParseDuration (e.g. "24h").
	// Defaults to 24 hours.
	SessionTTL time.Duration `env:"SESSION_TTL" envDefault:"24h"`

	// SessionCookieSecure sets the Secure attribute on the session cookie.
	// Must be true in production (HTTPS only).
	// Defaults to false for development convenience; set to true in production.
	SessionCookieSecure bool `env:"SESSION_COOKIE_SECURE" envDefault:"false"`

	// SessionCookieSameSite controls the SameSite attribute of the session
	// cookie.  Accepted values: "lax" (default), "strict", "none".
	// "none" requires Secure=true in modern browsers.
	SessionCookieSameSite string `env:"SESSION_COOKIE_SAMESITE" envDefault:"lax"`

	// --- CSRF ---
	// CSRFAuthKey is a 32-byte hex-encoded key used by gorilla/csrf.
	// Required in non-development environments.
	// In development a random key is generated at startup when this is unset.
	// NEVER set the real value here; load from env or secrets manager.
	CSRFAuthKey string `env:"CSRF_AUTH_KEY"`

	// CSRFSecure sets the Secure attribute on the CSRF cookie.
	// Should match SessionCookieSecure.
	CSRFSecure bool `env:"CSRF_SECURE" envDefault:"false"`

	// --- Field-level encryption ---
	// FieldEncryptionKey is a base64-encoded 32-byte AES-256 key used to encrypt
	// sensitive PII columns (口座番号, マイナンバー etc.) at the application layer.
	//
	// Production: inject from a secrets manager (AWS Secrets Manager, GCP Secret
	// Manager, HashiCorp Vault, …).  The actual key value MUST NOT be committed
	// to the repository.
	//
	// Development: when unset the crypto package generates an ephemeral random
	// key at startup with a warning.  Encrypted values are unreadable after
	// restart; this is acceptable for local development only.
	//
	// When KeyProvider is "aws-kms", this field is ignored — the DEK is generated
	// by AWS KMS and never stored here.
	FieldEncryptionKey string `env:"FIELD_ENCRYPTION_KEY"`

	// KeyProvider selects the field-encryption key backend.
	// Accepted values:
	//   "env"     — (default) read key from FIELD_ENCRYPTION_KEY env var.
	//   "aws-kms" — use AWS KMS envelope encryption; requires KMSKeyID.
	// Additional providers (GCP Cloud KMS, HashiCorp Vault) may be added without
	// changing callers; see internal/platform/crypto/keyprovider.go.
	KeyProvider string `env:"KEY_PROVIDER" envDefault:"env"`

	// KMSKeyID is the full ARN or alias of the AWS KMS Customer Managed Key (CMK)
	// used as the Key Encryption Key (KEK) when KeyProvider is "aws-kms".
	// Example: "arn:aws:kms:ap-northeast-1:123456789012:key/mrk-..."
	//          or "alias/hr-saas-field-encryption"
	// Required when KEY_PROVIDER=aws-kms; ignored otherwise.
	// MUST NOT be the real production ARN in committed code — set via env.
	KMSKeyID string `env:"KMS_KEY_ID"`

	// AWSRegion overrides the AWS region for KMS calls.
	// When empty, the standard AWS SDK chain is used (AWS_REGION env var,
	// ~/.aws/config, EC2/ECS metadata).  Prefer the SDK chain in production.
	AWSRegion string `env:"AWS_REGION"`

	// --- Rate limiting ---
	// AuthRateLimit is the rate limit for login and signup endpoints.
	// Format accepted by ulule/limiter: "10-M" (10 per minute), "100-H" (100 per hour).
	// Defaults to 10 per minute.
	AuthRateLimit string `env:"AUTH_RATE_LIMIT" envDefault:"10-M"`

	// --- Trusted proxies ---
	// TrustedProxies is a comma-separated list of IP addresses or CIDR ranges
	// that the server trusts to supply accurate X-Forwarded-For / X-Real-IP
	// headers.  Gin uses this list to determine the real client IP used for
	// rate-limiting and audit logging.
	//
	// When empty (the default), Gin operates in "no proxy trust" mode: it
	// ignores forwarding headers and uses the direct TCP peer address as the
	// client IP, which is the safest default.  This prevents an attacker from
	// spoofing X-Forwarded-For to bypass IP-based rate limiting or pollute the
	// audit log.
	//
	// Set this to the CIDR(s) of your load balancer / reverse proxy in
	// non-development environments where real client IPs arrive via headers.
	// Example: "10.0.0.0/8,172.16.0.0/12"
	TrustedProxies string `env:"TRUSTED_PROXIES"`

	// --- OpenTelemetry (NFR-012) ---

	// OTelEnabled activates OpenTelemetry trace and metric exporters.
	// Defaults to false; set to true in staging/production once an OTLP
	// endpoint is provisioned.
	OTelEnabled bool `env:"OTEL_ENABLED" envDefault:"false"`

	// OTelServiceName is the service.name resource attribute attached to all
	// spans and metrics.  Defaults to "hr-saas".
	OTelServiceName string `env:"OTEL_SERVICE_NAME" envDefault:"hr-saas"`

	// OTelExporterOTLPEndpoint is the base URL of the OTLP/HTTP endpoint
	// (e.g. "https://collector:4318" for an OpenTelemetry Collector, or a
	// vendor endpoint such as Grafana Cloud / Google Cloud Trace / Datadog).
	//
	// Placeholder — inject from a secrets manager in non-development
	// deployments.  NEVER hard-code a real URL or credentials here.
	// When empty (the default), OTel remains disabled even if OTEL_ENABLED=true.
	//
	// Pending: final exporter target depends on deploy-target decision (GAP-01).
	// Swap the exporter implementation in internal/platform/otel/otel.go once
	// the cloud provider is selected.
	OTelExporterOTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
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

// DSN returns the PostgreSQL data source name for the application role (hr_app).
// If DatabaseURL is set it takes precedence over individual DB_* fields.
//
// When constructing from individual fields, each value is encoded into a
// postgres:// URL so that special characters (spaces, single quotes,
// backslashes, etc.) in passwords or hostnames are safely percent-encoded.
// This prevents the connection string from being silently mis-parsed by the
// libpq keyword parser.
func (c *Config) DSN() string {
	if c.DatabaseURL != "" {
		return c.DatabaseURL
	}
	return buildPostgresURL(c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode)
}

// IsDevelopment reports whether the application is running in development mode.
func (c *Config) IsDevelopment() bool {
	return c.AppEnv == "development"
}

// AdminDSN returns the PostgreSQL DSN for the admin / migration role.
// It uses DB_ADMIN_USER and DB_ADMIN_PASSWORD when set; otherwise falls back
// to the application credentials (convenient for local single-user setups).
// ADMIN_DATABASE_URL takes highest precedence when set.
//
// Special characters in credentials are safely encoded; see DSN() for details.
func (c *Config) AdminDSN() string {
	if c.AdminDatabaseURL != "" {
		return c.AdminDatabaseURL
	}
	adminUser := c.DBAdminUser
	if adminUser == "" {
		adminUser = c.DBUser
	}
	adminPassword := c.DBAdminPassword
	if adminPassword == "" {
		adminPassword = c.DBPassword
	}
	return buildPostgresURL(c.DBHost, c.DBPort, adminUser, adminPassword, c.DBName, c.DBSSLMode)
}

// buildPostgresURL constructs a postgres:// URL from individual connection
// components.  Using net/url.URL ensures that user and password are
// percent-encoded, making the DSN safe even when credentials contain spaces,
// single quotes, backslashes, or other characters that would break the
// libpq keyword=value format.
//
// The resulting URL is accepted by GORM's postgres driver (pgx/v5 underneath).
func buildPostgresURL(host, port, user, password, dbname, sslmode string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   host + ":" + port,
		Path:   "/" + dbname,
	}
	q := u.Query()
	q.Set("sslmode", sslmode)
	u.RawQuery = q.Encode()
	return u.String()
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
	// In non-development environments, require explicit admin credentials to
	// avoid running migrations (or inadvertently exposing DDL access) through
	// the application's hr_app role.  Either DB_ADMIN_USER or
	// ADMIN_DATABASE_URL must be set.  An empty admin user causes AdminDSN()
	// to fall back to the application credentials, which is insufficient for
	// production environments where DDL requires a superuser role.
	if !c.IsDevelopment() && c.AdminDatabaseURL == "" && c.DBAdminUser == "" {
		errs = append(errs, errors.New(
			"non-development environment: either DB_ADMIN_USER or ADMIN_DATABASE_URL must be set "+
				"(the hr_app role must not be used for migrations)"))
	}

	// In non-development environments the session cookie MUST carry the Secure
	// attribute so it is only transmitted over HTTPS.  Running without Secure=true
	// in production would expose session tokens over plain HTTP.
	if !c.IsDevelopment() && !c.SessionCookieSecure {
		errs = append(errs, errors.New(
			"SESSION_COOKIE_SECURE must be true in non-development environments"))
	}

	// Empty string is treated as the default ("lax") so that Config structs
	// created in tests without this field set continue to pass validation.
	switch c.SessionCookieSameSite {
	case "", "lax", "strict", "none":
		// valid (empty resolves to "lax" at runtime)
	default:
		errs = append(errs, fmt.Errorf(
			"SESSION_COOKIE_SAMESITE %q must be one of: lax, strict, none",
			c.SessionCookieSameSite,
		))
	}

	// In non-development environments CSRF_AUTH_KEY must be set to a 32-byte
	// hex-encoded value (64 hex chars) to ensure CSRF token security.
	if !c.IsDevelopment() && c.CSRFAuthKey == "" {
		errs = append(errs, errors.New(
			"CSRF_AUTH_KEY must be set in non-development environments"))
	}
	if c.CSRFAuthKey != "" && len(c.CSRFAuthKey) != 64 {
		errs = append(errs, fmt.Errorf(
			"CSRF_AUTH_KEY must be exactly 64 hex characters (32 bytes); got %d characters",
			len(c.CSRFAuthKey),
		))
	}

	// In non-development environments, session keys and the field encryption key
	// must be explicitly set to prevent running with no key material.
	//
	// SessionHashKey: used for HMAC signing of session cookies.
	// Must be at least 32 bytes (we require exactly 64 hex chars = 32 bytes).
	if !c.IsDevelopment() && c.SessionHashKey == "" {
		errs = append(errs, errors.New(
			"SESSION_HASH_KEY must be set in non-development environments"))
	}
	if c.SessionHashKey != "" && len(c.SessionHashKey) != 64 {
		errs = append(errs, fmt.Errorf(
			"SESSION_HASH_KEY must be exactly 64 hex characters (32 bytes); got %d characters",
			len(c.SessionHashKey),
		))
	}

	// SessionBlockKey: used for AES encryption of session cookies.
	// Must be 32 bytes (we require exactly 64 hex chars = 32 bytes for AES-256).
	if !c.IsDevelopment() && c.SessionBlockKey == "" {
		errs = append(errs, errors.New(
			"SESSION_BLOCK_KEY must be set in non-development environments"))
	}
	if c.SessionBlockKey != "" && len(c.SessionBlockKey) != 64 {
		errs = append(errs, fmt.Errorf(
			"SESSION_BLOCK_KEY must be exactly 64 hex characters (32 bytes); got %d characters",
			len(c.SessionBlockKey),
		))
	}

	// KeyProvider must be one of the accepted values.
	// Empty string is treated as "env" (the envDefault applies when loaded via
	// env.Parse; direct struct construction in tests may leave it empty).
	switch c.KeyProvider {
	case "", "env", "aws-kms":
		// valid (empty resolves to "env" at runtime via envDefault tag)
	default:
		errs = append(errs, fmt.Errorf(
			"KEY_PROVIDER %q is not a valid value; accepted: env, aws-kms",
			c.KeyProvider,
		))
	}

	// FieldEncryptionKey: base64-encoded 32-byte AES-256 key for PII column
	// encryption.  In non-development environments a missing key is fatal —
	// we must never store PII without a real persistent key.
	// Length validation: base64-encoding of 32 bytes is 44 characters
	// (standard encoding with padding).
	//
	// When KEY_PROVIDER=aws-kms the plaintext key material comes from KMS and
	// FIELD_ENCRYPTION_KEY is not required; KMS_KEY_ID is required instead.
	if c.KeyProvider != "aws-kms" {
		if !c.IsDevelopment() && c.FieldEncryptionKey == "" {
			errs = append(errs, errors.New(
				"FIELD_ENCRYPTION_KEY must be set in non-development environments (or set KEY_PROVIDER=aws-kms)"))
		}
		if c.FieldEncryptionKey != "" && len(c.FieldEncryptionKey) != 44 {
			errs = append(errs, fmt.Errorf(
				"FIELD_ENCRYPTION_KEY must be exactly 44 base64 characters (32 bytes encoded); got %d characters",
				len(c.FieldEncryptionKey),
			))
		}
	}

	// When KEY_PROVIDER=aws-kms, KMS_KEY_ID is required so the application
	// knows which CMK to use.  An empty key ID would cause a runtime error
	// from the KMS SDK; fail fast here instead.
	if c.KeyProvider == "aws-kms" && c.KMSKeyID == "" {
		errs = append(errs, errors.New(
			"KMS_KEY_ID must be set when KEY_PROVIDER=aws-kms"))
	}

	// I-7: In non-development environments the CSRF cookie MUST carry the
	// Secure attribute so it is only transmitted over HTTPS.  A CSRF cookie
	// sent over plain HTTP allows network-level attackers to steal or replace
	// it, undermining double-submit cookie protection.
	// This mirrors the SESSION_COOKIE_SECURE requirement above.
	if !c.IsDevelopment() && !c.CSRFSecure {
		errs = append(errs, errors.New(
			"CSRF_SECURE must be true in non-development environments"))
	}

	return errors.Join(errs...)
}
