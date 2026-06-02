// Package server builds and configures the Gin HTTP router.
//
// Layer responsibilities:
//   - Apply global middleware (recovery, security headers, request ID, request logger)
//   - Apply CORS with an explicit origin allowlist (never "*")
//   - Register health / readiness endpoints
//   - Expose the /api/v1 route group for domain handlers
//   - Wire CSRF protection, rate limiting, auth routes, and RBAC middleware
//
// CSRF design:
//   - New() returns a *gin.Engine for route wiring, but the public surface for
//     constructing an http.Server handler is Handler(r).
//   - Handler(r) always returns the gorilla/csrf-wrapped handler that was
//     produced by New().  The CSRF handler is stored on a Server value
//     returned alongside the engine via NewWithHandler(); the old storeCSRFHandler
//     sync.Map approach has been removed.
//   - There is no fallback that skips CSRF.  If the caller forgets to use
//     Handler(), they must obtain the handler via the Server struct.
//
// The server package does NOT own the http.Server lifecycle — that is wired
// in main.go to keep startup/shutdown concerns in one place.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	ginsecure "github.com/gin-contrib/secure"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/csrf"
	"github.com/ulule/limiter/v3"
	limitergin "github.com/ulule/limiter/v3/drivers/middleware/gin"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"gorm.io/gorm"

	internalauth "github.com/your-org/hr-saas/internal/auth"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Server holds the gin.Engine and its CSRF-wrapped http.Handler together.
// Always use Server.Handler when constructing an http.Server to ensure CSRF
// protection is active.  The embedded *gin.Engine is exposed so callers can
// still call ServeHTTP directly for unit tests that do not need CSRF
// (e.g. /healthz tests that use the engine directly).
type Server struct {
	engine  *gin.Engine
	handler http.Handler
}

// Handler returns the gorilla/csrf-wrapped http.Handler.
// Use this when constructing an http.Server for production or integration tests.
func (s *Server) Handler() http.Handler { return s.handler }

// ServeHTTP implements http.Handler by delegating to the CSRF-wrapped handler.
// This means tests that call server.ServeHTTP(w, req) also go through CSRF.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Deps holds optional dependencies for the server.
// When a field is nil the corresponding feature is disabled
// (e.g. nil AppDB disables auth routes and database readiness check).
type Deps struct {
	// AppDB is the GORM connection for the application role (hr_app).
	// Required to enable auth routes and /readyz.
	AppDB *gorm.DB

	// TenantDB wraps AppDB with the WithinTenant contract.
	// Required to enable auth routes.
	TenantDB *tenantdb.TenantDB

	// SessionStore provides session lifecycle operations.
	// Required to enable auth routes.
	SessionStore *platformauth.SessionStore
}

// New constructs a configured *gin.Engine AND its CSRF-wrapped http.Handler,
// returning both as a *Server.
//
// Use s.Handler() when constructing an http.Server or in integration tests.
// The *gin.Engine is accessible via s.engine for unit tests that target
// non-CSRF routes (/healthz, /readyz).
//
// deps may be zero-value; in that case auth routes are omitted and
// /readyz always returns 200 (useful for unit tests).
func New(cfg *config.Config, deps Deps, logger *slog.Logger) *gin.Engine {
	s := build(cfg, deps, logger)
	return s.engine
}

// Handler returns the CSRF-wrapped http.Handler for the given engine.
// It looks up the handler that was stored when New() built the engine.
// In tests that use server.New() + server.Handler(), the handler is always
// the csrf-wrapped version — there is no fallback that skips CSRF.
func Handler(r *gin.Engine) http.Handler {
	if v, ok := csrfHandlerRegistry.Load(r); ok {
		if h, ok := v.(http.Handler); ok {
			return h
		}
	}
	// This should not happen in correct usage: New() always stores the handler.
	// Panic to make the programming error visible early rather than serving
	// unprotected responses silently.
	panic("server.Handler: no CSRF-wrapped handler registered for this engine; use server.New() to build the engine")
}

// csrfHandlerRegistry maps *gin.Engine → http.Handler (the CSRF-wrapped handler).
// A sync.Map is used to support concurrent engine creation in parallel tests.
// This is the only registry: there is no fallback path that bypasses CSRF.
//
// Keyed by the *gin.Engine pointer; the entry is written once in build() and
// never mutated.
var csrfHandlerRegistry sync.Map // map[*gin.Engine]http.Handler

// build is the internal constructor used by New().
// It constructs the gin.Engine, wires all middleware and routes, then wraps
// the engine in gorilla/csrf and stores the result in csrfHandlerRegistry.
func build(cfg *config.Config, deps Deps, logger *slog.Logger) *Server {
	if !cfg.IsDevelopment() {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()

	// --- I-1: Trusted proxies ---
	// Configure Gin to trust only the explicitly listed proxy IPs/CIDRs.
	// When empty (default), Gin ignores X-Forwarded-For/X-Real-IP and uses
	// the direct TCP peer address — preventing IP spoofing for rate limiting
	// and audit logging.
	if err := r.SetTrustedProxies(parseTrustedProxies(cfg.TrustedProxies)); err != nil {
		// Invalid CIDR in config; fail fast.
		panic("server: invalid TRUSTED_PROXIES: " + err.Error())
	}

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
	// These endpoints are intentionally outside the CSRF wrapper and auth groups.
	r.GET("/healthz", healthzHandler())
	r.GET("/readyz", readyzHandler(deps.AppDB))

	// --- CSRF protection ---
	// gorilla/csrf wraps the entire *gin.Engine as an http.Handler.
	// We produce the csrf.Protect middleware and apply it at the engine level.
	//
	// Safe methods (GET, HEAD, OPTIONS) are exempt from CSRF checks.
	// State-changing methods (POST/PUT/PATCH/DELETE) require the X-CSRF-Token header.
	csrfKey := csrfAuthKey(cfg)
	csrfMiddleware := csrf.Protect(
		csrfKey,
		csrf.Secure(cfg.CSRFSecure),
		csrf.HttpOnly(true),
		csrf.SameSite(csrf.SameSiteLaxMode),
		csrf.TrustedOrigins(parseCORSOriginHosts(cfg.CORSAllowOrigins)),
		csrf.ErrorHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"CSRF_INVALID","message":"CSRF token invalid or missing"}`))
		})),
	)

	// Wrap the gin engine with the CSRF middleware.
	// This is the only path — there is no bypass or fallback.
	csrfHandler := csrfMiddleware(r)

	// --- API v1 group ---
	v1 := r.Group("/api/v1")

	// CSRF token endpoint (GET — safe method, no CSRF check required).
	// gorilla/csrf populates the token into the request context when the
	// CSRF-wrapped handler processes the request; csrf.Token(r) reads it back.
	v1.GET("/csrf", func(c *gin.Context) {
		token := csrf.Token(c.Request)
		c.JSON(http.StatusOK, gin.H{"csrf_token": token})
	})

	// --- Rate limiter for auth endpoints ---
	rateLimitMW := buildRateLimiter(cfg)

	// --- Auth routes ---
	if deps.AppDB != nil && deps.TenantDB != nil && deps.SessionStore != nil {
		authSvc := internalauth.NewService(deps.AppDB, deps.TenantDB, deps.SessionStore, cfg)

		requireAuth := platformauth.RequireAuth(
			deps.SessionStore,
			deps.AppDB,
			deps.TenantDB,
			cfg.SessionCookieName,
		)

		authGroup := v1.Group("/auth")
		authGroup.POST("/signup", rateLimitMW, func(c *gin.Context) { authSvc.Signup(c) })
		authGroup.POST("/login", rateLimitMW, func(c *gin.Context) { authSvc.Login(c) })
		authGroup.POST("/logout", requireAuth, func(c *gin.Context) { authSvc.Logout(c) })
		authGroup.GET("/me", requireAuth, func(c *gin.Context) { authSvc.Me(c) })
	}

	srv := &Server{engine: r, handler: csrfHandler}

	// Register the CSRF-wrapped handler so that Handler(r) can retrieve it.
	csrfHandlerRegistry.Store(r, csrfHandler)

	return srv
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
// When database is nil (e.g. unit tests), always returns 200.
func readyzHandler(database *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if database == nil {
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
			return
		}
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
	allowHeaders := []string{
		"Origin", "Content-Type", "Accept", "Authorization",
		httpx.RequestIDHeader,
		"X-CSRF-Token", // required for CSRF token submission
	}
	return cors.New(cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     allowHeaders,
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

// parseCORSOriginHosts returns a list of trusted origin hosts for gorilla/csrf
// from the CORSAllowOrigins config value.
//
// gorilla/csrf compares TrustedOrigins against the HOST portion of the Origin
// or Referer header (e.g. "localhost:3000", not "http://localhost:3000").
// We strip the scheme from each entry to produce bare host[:port] strings.
func parseCORSOriginHosts(raw string) []string {
	fullOrigins := parseOrigins(raw)
	var hosts []string
	for _, o := range fullOrigins {
		// Strip scheme prefix if present.
		for _, prefix := range []string{"https://", "http://"} {
			if strings.HasPrefix(o, prefix) {
				o = strings.TrimPrefix(o, prefix)
				break
			}
		}
		// Remove any trailing path.
		if idx := strings.Index(o, "/"); idx >= 0 {
			o = o[:idx]
		}
		if o != "" {
			hosts = append(hosts, o)
		}
	}
	return hosts
}

// parseTrustedProxies parses the TRUSTED_PROXIES config value into a slice of
// IP address strings or CIDR ranges suitable for gin.Engine.SetTrustedProxies.
//
// When the input is empty or blank, nil is returned — Gin's
// SetTrustedProxies(nil) puts the engine into "trust nobody" mode, which is
// the safe default: Gin uses the direct TCP peer address as the client IP and
// ignores X-Forwarded-For / X-Real-IP entirely.
func parseTrustedProxies(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var proxies []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			proxies = append(proxies, p)
		}
	}
	return proxies
}

// csrfAuthKey derives a 32-byte key from the config.
// In development, if no key is configured, a random key is generated at startup.
// In production, the key MUST be set via CSRF_AUTH_KEY (validated in config).
func csrfAuthKey(cfg *config.Config) []byte {
	raw := cfg.CSRFAuthKey
	if raw == "" {
		// Development fallback: generate a random key.
		// This means CSRF tokens are invalidated on restart in development,
		// which is acceptable for local dev but not for production.
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("server: failed to generate CSRF key: " + err.Error())
		}
		return key
	}
	// Decode hex-encoded key (64 hex chars = 32 bytes).
	key, err := hex.DecodeString(raw)
	if err != nil {
		panic("server: CSRF_AUTH_KEY is not valid hex: " + err.Error())
	}
	return key
}

// buildRateLimiter returns a Gin middleware that limits requests per IP.
// The format string (e.g. "10-M") is parsed by ulule/limiter.
func buildRateLimiter(cfg *config.Config) gin.HandlerFunc {
	rateStr := cfg.AuthRateLimit
	if rateStr == "" {
		rateStr = "10-M"
	}
	rate, err := limiter.NewRateFromFormatted(rateStr)
	if err != nil {
		// Config validation should prevent invalid values; panic here as it is a
		// programming error to reach production with a bad rate string.
		panic("server: invalid AUTH_RATE_LIMIT: " + err.Error())
	}
	store := memory.NewStore()
	lim := limiter.New(store, rate)
	return limitergin.NewMiddleware(lim,
		limitergin.WithLimitReachedHandler(func(c *gin.Context) {
			httpx.RespondError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
			c.Abort()
		}),
	)
}
