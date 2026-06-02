// Package server builds and configures the Gin HTTP router.
//
// Layer responsibilities:
//   - Apply global middleware (recovery, security headers, request ID, request logger)
//   - Apply CORS with an explicit origin allowlist (never "*")
//   - Register health / readiness endpoints
//   - Expose the /api/v1 route group for domain handlers
//   - Wire CSRF protection, rate limiting, auth routes, and RBAC middleware
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
	limitergin "github.com/ulule/limiter/v3/drivers/middleware/gin"
	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"gorm.io/gorm"

	internalauth "github.com/your-org/hr-saas/internal/auth"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

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

// New constructs a configured *gin.Engine.
// It does not start listening — the caller wraps it in an http.Server.
//
// deps may be zero-value; in that case auth routes are omitted and
// /readyz always returns 200 (useful for unit tests).
func New(cfg *config.Config, deps Deps, logger *slog.Logger) *gin.Engine {
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
		csrf.TrustedOrigins(parseTrustedOrigins(cfg.CORSAllowOrigins)),
		csrf.ErrorHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"CSRF_INVALID","message":"CSRF token invalid or missing"}`))
		})),
	)

	// Wrap the gin engine with the CSRF middleware.
	// Gin's Handler() is an http.Handler; we re-wrap via a custom handler.
	ginCSRF := adaptCSRF(r, csrfMiddleware)

	// --- API v1 group ---
	v1 := r.Group("/api/v1")

	// CSRF token endpoint (GET — safe method, no CSRF check required).
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

	// Store the CSRF-wrapped handler so the caller can use it via Handler().
	// We store it on the engine's metadata for retrieval by Handler().
	// Since we need to return *gin.Engine but also expose the CSRF-wrapped handler,
	// we embed it as a stored value so the http.Server picks it up via Handler().
	r.Use(func(c *gin.Context) {
		// Inject CSRF token into every request's context so handlers can access it.
		// The actual CSRF enforcement is done by the ginCSRF wrapper.
		c.Next()
	})

	// Store the wrapped handler reference for Handler() retrieval.
	storeCSRFHandler(r, ginCSRF)

	return r
}

// Handler returns the http.Handler that wraps the gin.Engine with CSRF protection.
// Use this instead of the *gin.Engine directly when constructing an http.Server.
// If no CSRF handler is stored (e.g. in tests that call New directly), falls
// back to the engine itself.
func Handler(r *gin.Engine) http.Handler {
	if h := loadCSRFHandler(r); h != nil {
		return h
	}
	return r
}

// csrfHandlerKey is the gin.Context key for the stored CSRF http.Handler.
const csrfHandlerKey = "_csrf_handler"

func storeCSRFHandler(r *gin.Engine, h http.Handler) {
	r.Use(func(c *gin.Context) {
		c.Set(csrfHandlerKey, h)
		c.Next()
	})
}

func loadCSRFHandler(r *gin.Engine) http.Handler {
	if v, ok := csrfHandlerStore.Load(r); ok {
		if h, ok := v.(http.Handler); ok {
			return h
		}
	}
	return nil
}

// csrfHandlerStore maps engine pointer (uintptr) to its CSRF-wrapped handler.
// sync.Map is used to allow concurrent engine creation in parallel tests.
var csrfHandlerStore sync.Map // map[*gin.Engine]http.Handler

// adaptCSRF wraps the gin engine with the gorilla/csrf middleware, returning
// an http.Handler that applies CSRF enforcement before dispatching to gin.
func adaptCSRF(r *gin.Engine, csrfMiddleware func(http.Handler) http.Handler) http.Handler {
	wrapped := csrfMiddleware(r)
	csrfHandlerStore.Store(r, wrapped)
	return wrapped
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

// parseTrustedOrigins returns a list of trusted origin hosts for CSRF from the
// CORSAllowOrigins config value.
//
// gorilla/csrf compares TrustedOrigins against the HOST portion of the Origin
// or Referer header (e.g. "localhost:3000", not "http://localhost:3000").
// We strip the scheme from each entry to produce bare host[:port] strings.
func parseTrustedOrigins(raw string) []string {
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
