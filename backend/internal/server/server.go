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
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	ginsecure "github.com/gin-contrib/secure"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/csrf"
	"github.com/ulule/limiter/v3"
	limitergin "github.com/ulule/limiter/v3/drivers/middleware/gin"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/applicant"
	"github.com/your-org/hr-saas/internal/approval"
	"github.com/your-org/hr-saas/internal/attendance"
	internalauth "github.com/your-org/hr-saas/internal/auth"
	"github.com/your-org/hr-saas/internal/auth/sso"
	"github.com/your-org/hr-saas/internal/billing"
	"github.com/your-org/hr-saas/internal/department"
	"github.com/your-org/hr-saas/internal/employee"
	"github.com/your-org/hr-saas/internal/evaluation"
	"github.com/your-org/hr-saas/internal/goal"
	"github.com/your-org/hr-saas/internal/govfiling"
	"github.com/your-org/hr-saas/internal/hiring"
	"github.com/your-org/hr-saas/internal/interview"
	"github.com/your-org/hr-saas/internal/jobposting"
	"github.com/your-org/hr-saas/internal/leave"
	"github.com/your-org/hr-saas/internal/ledger"
	"github.com/your-org/hr-saas/internal/mynumber"
	"github.com/your-org/hr-saas/internal/notification"
	"github.com/your-org/hr-saas/internal/offer"
	"github.com/your-org/hr-saas/internal/onboarding"
	"github.com/your-org/hr-saas/internal/oneonone"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/db"
	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/reporting"
	"github.com/your-org/hr-saas/internal/selection"
	"github.com/your-org/hr-saas/internal/selfservice"
	"github.com/your-org/hr-saas/internal/talent"
	"github.com/your-org/hr-saas/internal/workrule"
	"github.com/your-org/hr-saas/internal/yearend"
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

	// FieldCipher is the AES-256-GCM cipher used to encrypt / decrypt sensitive
	// PII columns (マイナンバー, 口座番号, etc.).
	//
	// Construct via crypto.NewFieldCipherFromProvider, passing a KeyProvider
	// obtained from crypto.NewKeyProviderFromConfig.  The key provider is
	// selected by KEY_PROVIDER in config (default: "env", production: "aws-kms").
	//
	// Injection point: cmd/server/main.go or the integration test setup should
	// build the FieldCipher before calling server.New and pass it here.
	// See docs/key_rotation.md for the full key lifecycle.
	//
	// When nil, domain handlers that require field-level encryption will
	// return a startup error (fail-closed); do not deploy with nil in production.
	FieldCipher *crypto.FieldCipher

	// MetricsHandler is the Prometheus scrape http.Handler returned by
	// platformotel.Init.  When non-nil it is mounted at GET /metrics so that
	// Prometheus / CloudWatch agent can scrape SDK metrics (HTTP server
	// request duration, active requests, Go runtime, process stats, and any
	// domain-specific meters).
	//
	// Access to /metrics should be restricted at the infrastructure layer
	// (e.g. security-group / ALB listener rule) so it is not publicly reachable.
	// When nil (unit tests without OTel), the /metrics route is omitted.
	MetricsHandler http.Handler

	// SystemDB is a GORM connection for the hr_system role, which holds
	// BYPASSRLS privilege.  It is used exclusively for cross-tenant queries
	// that cannot be scoped to a single tenant — specifically the bounce-webhook
	// delivery lookup (findDeliveriesByProviderAndHash) where no tenant context
	// is available from the incoming SNS/SendGrid request.
	//
	// Source from: SYSTEM_DATABASE_URL environment variable.
	// Required DSN format: postgres://hr_system:<password>@<host>/<db>?sslmode=...
	//
	// Operational prerequisite: the hr_system PostgreSQL role must exist and be
	// granted BYPASSRLS before this connection can be used.  See
	// db/migrations/ for role creation (if present) or provision it manually:
	//   CREATE ROLE hr_system WITH LOGIN BYPASSRLS PASSWORD '...';
	//   GRANT SELECT ON email_deliveries TO hr_system;
	//
	// When nil (SYSTEM_DATABASE_URL not set), bounce cross-tenant lookups log a
	// warning and return empty results — the server starts normally (safe degrade).
	//
	// SECURITY: this connection MUST NOT be used for any purpose other than the
	// narrow cross-tenant bounce lookup.  All normal tenant-scoped operations
	// use AppDB/TenantDB.
	SystemDB *gorm.DB
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
	// OTel trace + metrics middleware: registers a span and HTTP server metrics
	// for each request.  Registered after RequestID so the request-ID is
	// available in the context when the span is started.
	// When OTel is disabled (no-op providers), this is effectively zero-cost.
	r.Use(httpx.OTelMiddleware())
	r.Use(httpx.RequestLogger(logger))

	// --- Health / readiness ---
	// These endpoints are intentionally outside the CSRF wrapper and auth groups.
	r.GET("/healthz", healthzHandler())
	r.GET("/readyz", readyzHandler(deps.AppDB))

	// --- Prometheus metrics scrape endpoint ---
	// Mounts the OTel SDK Prometheus exporter at GET /metrics.
	// Access MUST be restricted at the infrastructure layer (security group /
	// ALB listener rule) — this endpoint is not authenticated and must not be
	// publicly reachable in production.
	// When MetricsHandler is nil (unit tests without OTel init), the route is
	// omitted so existing tests continue to pass without changes.
	if deps.MetricsHandler != nil {
		h := deps.MetricsHandler
		r.GET("/metrics", func(c *gin.Context) {
			h.ServeHTTP(c.Writer, c.Request)
		})
	}

	// --- CSRF protection ---
	// gorilla/csrf wraps the entire *gin.Engine as an http.Handler.
	// We produce the csrf.Protect middleware and apply it at the engine level.
	//
	// Safe methods (GET, HEAD, OPTIONS) are exempt from CSRF checks.
	// State-changing methods (POST/PUT/PATCH/DELETE) require the X-CSRF-Token header.
	csrfKey := csrfAuthKey(cfg)
	// Security note — GO-2025-3884 (gorilla/csrf v1.7.3, no upstream fix as of 2026-06-03):
	// The vulnerability is a suffix-matching flaw in TrustedOrigins: if a caller passes
	// wildcard-style or subdomain-suffix patterns, a malicious origin such as
	// "evil.example.com" can match a trusted entry of "example.com".
	//
	// This codebase is NOT affected by that flaw because:
	//   1. parseCORSOriginHosts() strips schemes and paths, then forwards the resulting
	//      bare host[:port] strings verbatim from CORSAllowOrigins config — no patterns,
	//      no wildcards, no subdomain notation.
	//   2. The CORS origin allowlist is operator-configured (env var) and must list each
	//      trusted origin explicitly; wildcard entries are not accepted.
	//   3. csrf.SameSite(SameSiteLaxMode) provides an independent browser-enforced layer
	//      that rejects cross-site state-changing requests without a same-site cookie.
	//
	// No code change is needed; the existing exact-host TrustedOrigins call is safe.
	// When gorilla/csrf releases a version that fixes GO-2025-3884, upgrade via:
	//   go get github.com/gorilla/csrf@<fixed-version> && go mod tidy
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

			// --- SSO routes ---
			// OIDC and SAML providers are nil until credentials are provisioned.
			// The service returns HTTP 501 when the provider is not configured,
			// so the server starts cleanly without IdP credentials.
			ssoSvc := sso.NewService(
				nil, // oidcProvider: inject sso.NewOIDCProvider(...) once credentials are available
				nil, // samlProvider: inject sso.NewSAMLProvider(...) once credentials are available
				sso.NewPGProviderRepository(deps.TenantDB),
				sso.NewStateStore(deps.TenantDB),
				sso.NewPGJITProvisioner(deps.TenantDB),
				deps.SessionStore,
				deps.TenantDB,
				cfg,
			)
			ssoGroup := authGroup.Group("/sso")
			// OIDC: GET /api/v1/auth/sso/oidc/:idp_id → redirect to IdP
			ssoGroup.GET("/oidc/:idp_id", func(c *gin.Context) { ssoSvc.StartOIDC(c) })
			// OIDC callback: GET /api/v1/auth/sso/oidc/callback
			ssoGroup.GET("/oidc/callback", func(c *gin.Context) { ssoSvc.CallbackOIDC(c) })
			// SAML: GET /api/v1/auth/sso/saml/:idp_id → redirect to IdP
			ssoGroup.GET("/saml/:idp_id", func(c *gin.Context) { ssoSvc.StartSAML(c) })
			// SAML ACS: POST /api/v1/auth/sso/saml/acs
			//
			// CSRF EXEMPTION (security note):
			// The SAML ACS endpoint receives a POST directly from the IdP's browser
			// redirect with a SAMLResponse form field.  The IdP does not include a
			// gorilla/csrf CSRF token; applying CSRF protection here would break all
			// SAML login flows.  The exemption is scoped to this single route only —
			// all other POST/PUT/PATCH/DELETE routes remain CSRF-protected.
			//
			// Alternative mitigations ensure this route is not CSRF-exploitable:
			//   - The SAMLResponse is cryptographically signed by the IdP; any
			//     cross-site request without a valid assertion is rejected by
			//     ssoSvc.ACSSAML (HandleCallback verifies the signature).
			//   - The RelayState / authnRequestID is server-generated and consumed
			//     once (replay protection in StateStore).
			//   - SameSite=Lax on the session cookie prevents the session from being
			//     set by a cross-site top-level navigation without a real assertion.
			ssoGroup.POST("/saml/acs", csrfExempt(), func(c *gin.Context) { ssoSvc.ACSSAML(c) })

			// --- Department routes ---
		department.RegisterRoutes(v1, deps.TenantDB, requireAuth)

		// --- Employee / assignment / contract routes ---
		employee.RegisterRoutes(v1, deps.TenantDB, requireAuth)

		// --- Attendance routes (勤怠 / 36協定) ---
		attendance.RegisterRoutes(v1, deps.TenantDB, requireAuth)

		// --- Approval workflow routes (申請承認) ---
		approvalSvc := approval.NewService(deps.TenantDB)
		approval.RegisterRoutes(v1, deps.TenantDB, requireAuth)

		// --- Leave routes (休暇 / 年休 LM-040/041/042/043) ---
		leave.RegisterRoutes(v1, deps.TenantDB, approvalSvc, requireAuth)

		// --- Onboarding / offboarding routes (入退社 ST-LM-07) ---
		onboarding.RegisterRoutes(v1, deps.TenantDB, requireAuth)

		// --- Notification platform routes (通知) ---
		// Build the production MailSender from MAIL_PROVIDER env.
		notifMailer, notifProviderName := buildMailSender(logger)
		notification.SetProviderName(notifProviderName)

		// Build optional chat senders from environment variables.
		// Senders are only constructed when the required env vars are non-empty.
		// SECURITY: Webhook URLs and tokens are secrets; they are read from env
		// variables only and are NEVER logged, committed, or written to DB.
		chatSenders := buildChatSenders(logger)

		// Build the notification Service with the real mailer, system DB
		// (BYPASSRLS for bounce webhooks), and chat senders.
		// deps.SystemDB may be nil when SYSTEM_DATABASE_URL is not configured;
		// NewServiceFull accepts nil and degrades safely (bounce lookups log a
		// warning and return empty results — see Service.findDeliveriesByProviderAndHash).
		notifSvc := notification.NewServiceFull(deps.TenantDB, notifMailer, deps.SystemDB, chatSenders...)
		notification.RegisterRoutes(v1, deps.TenantDB, requireAuth,
			notification.WithService(notifSvc))

		// --- Bounce / complaint webhook routes (unauthenticated; SNS / SendGrid) ---
		// These routes are intentionally placed outside the CSRF wrapper because
		// AWS SNS and SendGrid call them directly without session cookies.
		// They are registered on the raw gin.Engine (r), not on the v1 group.
		webhooksGroup := r.Group("/webhooks")
		bounceCfg := buildBounceWebhookConfig(logger)
		// deps.SystemDB (BYPASSRLS) is passed so RegisterBounceRoutes can perform
		// cross-tenant delivery lookups.  When nil the handler degrades safely.
		notification.RegisterBounceRoutes(webhooksGroup, deps.TenantDB, deps.SystemDB, notifMailer, bounceCfg)

		// --- My Number routes (マイナンバー) ---
		mynumber.RegisterRoutes(v1, deps.TenantDB, requireAuth)

		// --- Newly wired domain routes ---
		jobposting.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		goal.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		reporting.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		// Wire the mynumber provider adapter so govfiling can provide 個人番号
		// for social-insurance filings (利用提供ログ付き).
		mnSvc := mynumber.NewService(deps.TenantDB)
		govfiling.RegisterRoutes(v1, deps.TenantDB, requireAuth, NewMynumberProviderAdapter(mnSvc))
		ledger.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		billing.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		selfservice.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		applicant.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		workrule.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		selection.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		offer.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		evaluation.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		oneonone.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		interview.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		hiring.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		talent.RegisterRoutes(v1, deps.TenantDB, requireAuth)
		yearend.RegisterRoutes(v1, deps.TenantDB, requireAuth)
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

// buildMailSender constructs a MailSender and returns the provider name from
// MAIL_PROVIDER (valid values: "ses", "sendgrid", "mock").  On configuration
// errors the function logs a warning and falls back to MockSender so the
// server starts cleanly.  The returned providerName is the canonical string
// written to email_deliveries.provider rows.
//
// SECURITY: API keys / credentials are read from environment variables only
// and are NEVER logged, committed, or written to persistent storage.
func buildMailSender(logger *slog.Logger) (notification.MailSender, string) {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("MAIL_PROVIDER")))

	switch provider {
	case "ses":
		cfg := notification.SESConfig{
			Region:          os.Getenv("AWS_REGION"),
			FromAddress:     os.Getenv("NOTIFICATION_SES_FROM_ADDRESS"),
			AccessKeyID:     os.Getenv("NOTIFICATION_SES_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("NOTIFICATION_SES_SECRET_ACCESS_KEY"),
		}
		sender, err := notification.NewSESSender(cfg)
		if err != nil {
			logger.Warn("server: SES mail sender misconfigured; falling back to mock",
				"error", err.Error(),
			)
			return notification.MockSender{}, "mock"
		}
		logger.Info("server: mail provider: ses",
			"region", cfg.Region,
			"from_address", cfg.FromAddress,
		)
		return sender, "ses"

	case "sendgrid":
		cfg := notification.SendGridConfig{
			APIKey:      os.Getenv("NOTIFICATION_SENDGRID_API_KEY"),
			FromAddress: os.Getenv("NOTIFICATION_SENDGRID_FROM_ADDRESS"),
		}
		sender, err := notification.NewSendGridSender(cfg)
		if err != nil {
			logger.Warn("server: SendGrid mail sender misconfigured; falling back to mock",
				"error", err.Error(),
			)
			return notification.MockSender{}, "mock"
		}
		logger.Info("server: mail provider: sendgrid",
			"from_address", cfg.FromAddress,
		)
		return sender, "sendgrid"

	default:
		if provider != "" && provider != "mock" {
			logger.Warn("server: unknown MAIL_PROVIDER value; using mock",
				"value", provider,
			)
		} else {
			logger.Info("server: mail provider: mock (development)")
		}
		return notification.MockSender{}, "mock"
	}
}

// buildChatSenders constructs the enabled chat senders from environment variables.
//
// Each sender is only built when the minimum required env vars are present.
// Misconfigured senders log a warning and are skipped (fail-open for chat so
// email / in-app delivery is never blocked by a missing chat credential).
//
// SECURITY: all env var values treated as secrets are NEVER logged.
// The function logs only channel names and non-PII configuration metadata.
//
// Environment variables used:
//
//	Slack:      NOTIFICATION_SLACK_WEBHOOK_URL
//	Teams:      NOTIFICATION_TEAMS_WEBHOOK_URL
//	LINE WORKS (pre-issued token mode):
//	            NOTIFICATION_LINE_WORKS_BOT_ID
//	            NOTIFICATION_LINE_WORKS_CHANNEL_ID
//	            NOTIFICATION_LINE_WORKS_CHANNEL_TOKEN
//	LINE WORKS (Service Account / OAuth2 mode):
//	            NOTIFICATION_LINE_WORKS_BOT_ID
//	            NOTIFICATION_LINE_WORKS_CHANNEL_ID
//	            NOTIFICATION_LINE_WORKS_CLIENT_ID
//	            NOTIFICATION_LINE_WORKS_SERVICE_ACCOUNT_ID
//	            NOTIFICATION_LINE_WORKS_PRIVATE_KEY
func buildChatSenders(logger *slog.Logger) []notification.ChatSender {
	var senders []notification.ChatSender

	// --- Slack ---
	if slackURL := os.Getenv("NOTIFICATION_SLACK_WEBHOOK_URL"); slackURL != "" {
		sender, err := notification.NewSlackSender(notification.SlackConfig{
			WebhookURL: slackURL,
		})
		if err != nil {
			logger.Warn("server: Slack chat sender misconfigured; skipping",
				"error", err.Error(),
			)
		} else {
			logger.Info("server: chat sender enabled: slack")
			senders = append(senders, sender)
		}
	}

	// --- Microsoft Teams ---
	if teamsURL := os.Getenv("NOTIFICATION_TEAMS_WEBHOOK_URL"); teamsURL != "" {
		sender, err := notification.NewTeamsSender(notification.TeamsConfig{
			WebhookURL: teamsURL,
		})
		if err != nil {
			logger.Warn("server: Teams chat sender misconfigured; skipping",
				"error", err.Error(),
			)
		} else {
			logger.Info("server: chat sender enabled: teams")
			senders = append(senders, sender)
		}
	}

	// --- LINE WORKS ---
	// Two modes:
	//   (a) Pre-issued token: NOTIFICATION_LINE_WORKS_CHANNEL_TOKEN is set.
	//       The static token is used directly.  No dynamic refresh occurs; rotate
	//       manually when the token expires.
	//   (b) Service Account OAuth2 (recommended for production): CLIENT_ID +
	//       SERVICE_ACCOUNT_ID + PRIVATE_KEY are all set.  A LineWorksTokenProvider
	//       is constructed and attached to the sender so tokens are fetched and
	//       cached automatically at runtime without requiring a restart.
	lwBotID := os.Getenv("NOTIFICATION_LINE_WORKS_BOT_ID")
	lwChannelID := os.Getenv("NOTIFICATION_LINE_WORKS_CHANNEL_ID")
	if lwBotID != "" && lwChannelID != "" {
		lwStaticToken := os.Getenv("NOTIFICATION_LINE_WORKS_CHANNEL_TOKEN")

		// Try Service Account OAuth2 mode first when SA credentials are present.
		tokenProvider := buildLineWorksTokenProvider(logger)

		// We need at least one token source (static or SA provider) to construct
		// the sender.  Use a placeholder in config when the provider handles tokens.
		// NewLineWorksSender requires ChannelToken non-empty as a safety check;
		// supply the static token when available, else a placeholder so the
		// provider-based flow is used at Send time.
		effectiveToken := lwStaticToken
		if effectiveToken == "" && tokenProvider != nil {
			// SA provider will supply the real token at Send time.
			// Use a sentinel so NewLineWorksSender construction succeeds.
			effectiveToken = "provider-managed"
		}

		if effectiveToken != "" {
			sender, err := notification.NewLineWorksSender(notification.LineWorksConfig{
				BotID:        lwBotID,
				ChannelID:    lwChannelID,
				ChannelToken: effectiveToken,
			})
			if err != nil {
				logger.Warn("server: LINE WORKS chat sender misconfigured; skipping",
					"error", err.Error(),
				)
			} else {
				// Attach the dynamic token provider when SA credentials were given.
				// When tokenProvider is non-nil it takes precedence over the static
				// ChannelToken at Send time (see LineWorksSender.Send).
				if tokenProvider != nil {
					sender.TokenProvider = tokenProvider
					logger.Info("server: chat sender enabled: line_works (SA OAuth2 token provider)")
				} else {
					logger.Info("server: chat sender enabled: line_works (static token)")
				}
				senders = append(senders, sender)
			}
		} else {
			logger.Warn("server: LINE WORKS bot/channel IDs configured but no token source available; skipping",
				"bot_id_set", lwBotID != "",
				"channel_id_set", lwChannelID != "",
			)
		}
	}

	if len(senders) == 0 {
		logger.Info("server: no chat senders configured (set NOTIFICATION_SLACK_WEBHOOK_URL, " +
			"NOTIFICATION_TEAMS_WEBHOOK_URL, or LINE WORKS env vars to enable)")
	}
	return senders
}

// buildLineWorksTokenProvider constructs a LineWorksTokenProvider from the
// Service Account environment variables.  Returns nil when the required env
// vars are absent (SA mode not configured).
//
// When non-nil, the provider is attached to LineWorksSender.TokenProvider so
// that tokens are fetched and cached dynamically on each Send call — no manual
// rotation or server restart is needed when the token expires.
//
// SECURITY: the private key is read from the env variable and is never logged.
func buildLineWorksTokenProvider(logger *slog.Logger) *notification.LineWorksTokenProvider {
	clientID := os.Getenv("NOTIFICATION_LINE_WORKS_CLIENT_ID")
	saID := os.Getenv("NOTIFICATION_LINE_WORKS_SERVICE_ACCOUNT_ID")
	privKey := os.Getenv("NOTIFICATION_LINE_WORKS_PRIVATE_KEY")
	if clientID == "" || saID == "" || privKey == "" {
		return nil
	}
	provider, err := notification.NewLineWorksTokenProvider(notification.LineWorksServiceAccountConfig{
		ClientID:         clientID,
		ServiceAccountID: saID,
		PrivateKeyPEM:    privKey,
	})
	if err != nil {
		logger.Warn("server: LINE WORKS token provider construction failed; SA mode disabled",
			"error", err.Error(),
		)
		return nil
	}
	// Probe the token at startup to surface misconfigured credentials early.
	// Failure is non-fatal: log a Warn and skip the SA provider (fall back to
	// static token or no sender), mirroring the original buildLineWorksOAuth2Token
	// behaviour.
	probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.Token(probeCtx); err != nil {
		logger.Warn("server: LINE WORKS SA token fetch at startup failed; skipping sender",
			"error", err.Error(),
		)
		return nil
	}
	return provider
}

// buildBounceWebhookConfig reads bounce/complaint webhook configuration from
// environment variables.
//
// SECURITY: SendGridWebhookSigningKey is treated as a secret and is NEVER
// logged.
func buildBounceWebhookConfig(logger *slog.Logger) notification.BounceWebhookConfig {
	cfg := notification.BounceWebhookConfig{
		SNSTopicARN:               os.Getenv("NOTIFICATION_SES_SNS_TOPIC_ARN"),
		SendGridWebhookSigningKey: os.Getenv("NOTIFICATION_SENDGRID_WEBHOOK_SIGNING_KEY"),
	}

	actorIDStr := os.Getenv("NOTIFICATION_WEBHOOK_ACTOR_ID")
	if actorIDStr != "" {
		parsed, err := uuid.Parse(actorIDStr)
		if err != nil {
			logger.Warn("server: NOTIFICATION_WEBHOOK_ACTOR_ID is not a valid UUID; using nil UUID",
				"error", err.Error(),
			)
		} else {
			cfg.SystemActorID = parsed
		}
	}

	if cfg.SNSTopicARN != "" {
		logger.Info("server: bounce webhook: SNS topic ARN configured",
			"arn_prefix", cfg.SNSTopicARN[:min(len(cfg.SNSTopicARN), 20)],
		)
	}
	if cfg.SendGridWebhookSigningKey != "" {
		logger.Info("server: bounce webhook: SendGrid signing key configured")
	}
	return cfg
}

// csrfExempt returns a Gin middleware that marks the current request as exempt
// from gorilla/csrf token checking.  It calls csrf.UnsafeSkipCheck, which sets
// a context flag that causes the csrf middleware to pass the request through
// without a token check.
//
// SECURITY: Apply this ONLY to routes that receive unauthenticated POST requests
// from external parties (e.g. IdP-initiated SAML ACS, SNS webhooks).  Do NOT
// apply to any route that involves user-initiated state changes — those must
// remain CSRF-protected.  The routes using this middleware MUST implement their
// own equivalent of CSRF protection (e.g. cryptographic assertion signature
// verification for SAML ACS).
func csrfExempt() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Replace the request in the Gin context with a copy that carries the
		// gorilla/csrf skip flag.  All subsequent middleware and handlers in this
		// request lifecycle see the flag and the csrf middleware skips the check.
		c.Request = csrf.UnsafeSkipCheck(c.Request)
		c.Next()
	}
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
