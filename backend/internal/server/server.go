// Package server builds and configures the Gin HTTP router.
//
// Layer responsibilities:
//   - Apply global middleware (recovery, security headers, request ID, request logger)
//   - Apply CORS with an explicit origin allowlist (never "*")
//   - Register health / readiness endpoints
//   - Expose the /api/v1 route group for domain handlers (empty in this slice)
//
// The server package does NOT own the http.Server lifecycle — that is wired
// in main.go to keep startup/shutdown concerns in one place.
package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	ginsecure "github.com/gin-contrib/secure"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/httpx"

	"log/slog"
)

// New constructs a configured *gin.Engine.
// It does not start listening — the caller wraps it in an http.Server.
func New(cfg *config.Config, database *gorm.DB, logger *slog.Logger) *gin.Engine {
	if !cfg.IsDevelopment() {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()

	// --- Global middleware (order matters) ---
	//
	// 1. Recovery — must be outermost so it catches panics from all later middleware.
	// 2. secureHeaders (gin-contrib/secure) — single authority for X-Frame-Options,
	//    X-Content-Type-Options, HSTS, CSP, Referrer-Policy.
	// 3. httpx.SecurityHeaders — supplements with headers not covered by secure
	//    (currently: Cross-Origin-Opener-Policy).
	// 4. corsMiddleware — only registered when an explicit origin allowlist is
	//    configured; skipped entirely otherwise to avoid unintended permissiveness.
	// 5. RequestID — must run before RequestLogger so the ID is available for logging.
	// 6. RequestLogger — outermost timing wrapper after security headers are set.
	r.Use(gin.Recovery())
	r.Use(secureHeaders(cfg))
	r.Use(httpx.SecurityHeaders())
	if origins := parseOrigins(cfg.CORSAllowOrigins); len(origins) > 0 {
		r.Use(corsMiddleware(origins, cfg))
	}
	r.Use(httpx.RequestID())
	r.Use(httpx.RequestLogger(logger))

	// --- Health / readiness ---
	r.GET("/healthz", healthzHandler())
	r.GET("/readyz", readyzHandler(database))

	// --- API v1 group (domain routes added in later slices) ---
	_ = r.Group("/api/v1")

	return r
}

// healthzHandler reports that the process is alive (no external checks).
// Always returns 200 OK.
func healthzHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// readyzHandler reports whether the server is ready to serve traffic.
// Returns 503 when the database is unreachable, 200 otherwise.
// Behaviour matches the original /readyz in main.go.
func readyzHandler(database *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := db.Ping(c.Request.Context(), database); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}

// corsMiddleware builds a gin-contrib/cors handler from an explicit origin list.
// Wildcard "*" is never used — origins must be listed explicitly.
// AllowCredentials is true; combining it with a wildcard would be a security
// error and is prevented by the explicit-list requirement above.
func corsMiddleware(origins []string, cfg *config.Config) gin.HandlerFunc {
	return cors.New(cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", httpx.RequestIDHeader},
		ExposeHeaders:    []string{httpx.RequestIDHeader},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	})
}

// secureHeaders is the single authority for the following response headers:
//
//   - X-Frame-Options (FrameDeny)
//   - X-Content-Type-Options (ContentTypeNosniff)
//   - Strict-Transport-Security (HSTS, production only — STSSeconds=0 in dev)
//   - X-XSS-Protection (BrowserXssFilter)
//   - Content-Security-Policy (minimal safe default)
//   - Referrer-Policy
//
// IMPORTANT: gin-contrib/secure's IsDevelopment flag suppresses ALL header
// writing, not just HSTS/SSL-redirect. We therefore do NOT set IsDevelopment
// here; instead, HSTS is disabled in development by setting STSSeconds to 0,
// which leaves every other header active in all environments.
//
// httpx.SecurityHeaders handles headers outside gin-contrib/secure's scope
// (currently Cross-Origin-Opener-Policy) to avoid duplication.
func secureHeaders(cfg *config.Config) gin.HandlerFunc {
	isDev := cfg.IsDevelopment()
	return ginsecure.New(ginsecure.Config{
		// HSTS: disabled in development to avoid caching localhost as HTTPS-only.
		// Zero value means the header is omitted entirely (no max-age=0 sent).
		STSSeconds: func() int64 {
			if isDev {
				return 0
			}
			return 31536000 // 1 year
		}(),
		STSIncludeSubdomains: !isDev,

		// Clickjacking prevention.
		FrameDeny: true,

		// MIME-type sniffing prevention.
		ContentTypeNosniff: true,

		// Legacy XSS filter (belt-and-suspenders; CSP is the primary control).
		BrowserXssFilter: true,

		// Minimal CSP: restrict default sources to same origin.
		// Tighten once the frontend origin and CDN hosts are known.
		ContentSecurityPolicy: "default-src 'self'",

		// Referrer-Policy: no-referrer prevents the Referer header from leaking
		// the current URL to third-party resources.
		ReferrerPolicy: "no-referrer",

		// IsDevelopment is intentionally NOT set here (left false).
		// Setting it true would suppress ALL security headers in development,
		// which breaks tests and leaves the dev server unprotected.
		// HSTS suppression in development is achieved via STSSeconds=0 above.
	})
}

// parseOrigins splits a comma-separated origin string into a slice.
// Empty entries are dropped. Returns nil (empty slice) when the input is empty
// so the caller can skip CORS middleware registration entirely.
func parseOrigins(raw string) []string {
	var origins []string
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			origins = append(origins, o)
		}
	}
	return origins
}
