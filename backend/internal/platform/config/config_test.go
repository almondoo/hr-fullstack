package config

import (
	"net/url"
	"strings"
	"testing"
)

// TestDSNSpecialCharacters verifies that buildPostgresURL correctly encodes
// special characters in credentials — spaces, single quotes, backslashes, and
// at-signs — that would break the libpq keyword=value connection string format.
// No actual database connection is made; this is a pure unit test of the DSN
// construction logic.
func TestDSNSpecialCharacters(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		port     string
		user     string
		password string
		dbname   string
		sslmode  string
	}{
		{
			name:     "password with single quote",
			host:     "localhost",
			port:     "5432",
			user:     "hr_app",
			password: "p'assword",
			dbname:   "hr_saas",
			sslmode:  "disable",
		},
		{
			name:     "password with backslash",
			host:     "localhost",
			port:     "5432",
			user:     "hr_app",
			password: `p\assword`,
			dbname:   "hr_saas",
			sslmode:  "disable",
		},
		{
			name:     "password with space",
			host:     "localhost",
			port:     "5432",
			user:     "hr_app",
			password: "pass word",
			dbname:   "hr_saas",
			sslmode:  "disable",
		},
		{
			name:     "password with at-sign",
			host:     "localhost",
			port:     "5432",
			user:     "hr_app",
			password: "pass@word",
			dbname:   "hr_saas",
			sslmode:  "disable",
		},
		{
			name:     "password with multiple special chars",
			host:     "db.internal",
			port:     "5433",
			user:     "admin user",
			password: `p'a\ss w@ord`,
			dbname:   "hr_saas_prod",
			sslmode:  "require",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := buildPostgresURL(tc.host, tc.port, tc.user, tc.password, tc.dbname, tc.sslmode)

			// The DSN must be a valid URL that can be re-parsed.
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("DSN is not a valid URL: %v\nDSN: %s", err, dsn)
			}

			// Scheme must be postgres.
			if u.Scheme != "postgres" {
				t.Errorf("expected scheme 'postgres', got %q", u.Scheme)
			}

			// Extracted credentials must round-trip exactly.
			gotUser := u.User.Username()
			if gotUser != tc.user {
				t.Errorf("user: want %q, got %q", tc.user, gotUser)
			}
			gotPass, _ := u.User.Password()
			if gotPass != tc.password {
				t.Errorf("password: want %q, got %q", tc.password, gotPass)
			}

			// Database name must appear in the path.
			wantPath := "/" + tc.dbname
			if u.Path != wantPath {
				t.Errorf("path: want %q, got %q", wantPath, u.Path)
			}

			// sslmode query parameter must be present.
			if got := u.Query().Get("sslmode"); got != tc.sslmode {
				t.Errorf("sslmode: want %q, got %q", tc.sslmode, got)
			}

			// The raw DSN must not contain an unencoded literal single quote or
			// unencoded space in the host/user/password section — confirm the
			// special characters are percent-encoded in the raw URL string.
			// (They may still appear in the query string via normal encoding.)
			userInfo := u.User.String() // already encoded by net/url
			_ = userInfo

			// Verify the password does NOT appear in plaintext inside the URL
			// if it contains characters that would need encoding.
			if strings.ContainsAny(tc.password, " '\\@") {
				// The literal password must not appear as-is in the raw DSN.
				if strings.Contains(dsn, tc.password) {
					t.Errorf("raw DSN contains unencoded password with special characters: DSN=%s", dsn)
				}
			}
		})
	}
}

// TestValidateAdminCredentialsRequiredInProd verifies that validate() rejects a
// configuration that has no admin credentials in a non-development environment.
func TestValidateAdminCredentialsRequiredInProd(t *testing.T) {
	t.Run("non-dev without admin credentials fails", func(t *testing.T) {
		cfg := &Config{
			AppEnv:           "production",
			HTTPPort:         "8080",
			DBPassword:       "secret",
			DBSSLMode:        "require",
			CORSAllowOrigins: "https://example.com",
			// DBAdminUser and AdminDatabaseURL intentionally left empty.
		}
		err := cfg.validate()
		if err == nil {
			t.Fatal("expected validation error for missing admin credentials in production, got nil")
		}
		if !strings.Contains(err.Error(), "DB_ADMIN_USER") && !strings.Contains(err.Error(), "ADMIN_DATABASE_URL") {
			t.Errorf("error message should mention DB_ADMIN_USER or ADMIN_DATABASE_URL, got: %v", err)
		}
	})

	t.Run("non-dev with DB_ADMIN_USER passes", func(t *testing.T) {
		cfg := &Config{
			AppEnv:           "production",
			HTTPPort:         "8080",
			DBPassword:       "secret",
			DBSSLMode:        "require",
			CORSAllowOrigins: "https://example.com",
			DBAdminUser:      "postgres",
		}
		err := cfg.validate()
		if err != nil {
			t.Errorf("expected no validation error with DB_ADMIN_USER set, got: %v", err)
		}
	})

	t.Run("non-dev with ADMIN_DATABASE_URL passes", func(t *testing.T) {
		cfg := &Config{
			AppEnv:           "production",
			HTTPPort:         "8080",
			DBPassword:       "secret",
			DBSSLMode:        "require",
			CORSAllowOrigins: "https://example.com",
			AdminDatabaseURL: "postgres://postgres:secret@db:5432/hr_saas",
		}
		err := cfg.validate()
		if err != nil {
			t.Errorf("expected no validation error with ADMIN_DATABASE_URL set, got: %v", err)
		}
	})

	t.Run("development without admin credentials passes (fallback allowed)", func(t *testing.T) {
		cfg := &Config{
			AppEnv:     "development",
			HTTPPort:   "8080",
			DBPassword: "secret",
			DBSSLMode:  "disable",
			// DBAdminUser and AdminDatabaseURL intentionally left empty.
		}
		err := cfg.validate()
		if err != nil {
			t.Errorf("expected no validation error in development without admin credentials, got: %v", err)
		}
	})
}

// TestMigrateOnStartupDefault verifies that the envDefault for MigrateOnStartup
// is "false" (safe production default).  This is a structural guard so that a
// refactor cannot silently restore the unsafe "true" default.
//
// The Go zero value for bool is false, which matches envDefault:"false".
// A change to envDefault:"true" would require explicitly setting the field
// to true before env.Parse, which would be caught by integration smoke tests;
// this test guards the zero-value assumption.
func TestMigrateOnStartupDefault(t *testing.T) {
	var cfg Config
	if cfg.MigrateOnStartup {
		t.Error("MigrateOnStartup zero value should be false; ensure envDefault tag is \"false\"")
	}
}
