package sso

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/config"
	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Service orchestrates SSO flows: it resolves provider config, initiates the
// auth redirect, handles the callback, provisions the user (JIT), and issues
// a session using the existing session store.
//
// Security invariants:
//   - State is generated server-side (32 random bytes) and verified on callback.
//   - PKCE code_verifier and SAML AuthnRequest ID are persisted in sso_state
//     and consumed (deleted) on callback — one use only.
//   - SSO success issues a session via the standard platformauth.SessionStore.
//   - ErrInvalidAssertion from any SSOProvider causes a 401; no session is issued.
//   - Tenant isolation: all DB calls use WithinTenant.
type Service struct {
	oidcProvider SSOProvider // may be nil when OIDC is not configured
	samlProvider SSOProvider // may be nil when SAML is not configured
	providerRepo ProviderRepository
	stateStore   *StateStore
	jitProvisioner JITProvisioner
	sessionStore *platformauth.SessionStore
	tdb          *tenantdb.TenantDB
	cfg          *config.Config
	cookieOpts   platformauth.CookieOptions
}

// NewService constructs an SSO Service.
// oidcProvider and samlProvider may be nil; the corresponding protocol is
// disabled in that case (returns ErrNotImplemented from handler).
func NewService(
	oidcProvider SSOProvider,
	samlProvider SSOProvider,
	providerRepo ProviderRepository,
	stateStore *StateStore,
	jitProvisioner JITProvisioner,
	sessionStore *platformauth.SessionStore,
	tdb *tenantdb.TenantDB,
	cfg *config.Config,
) *Service {
	return &Service{
		oidcProvider:   oidcProvider,
		samlProvider:   samlProvider,
		providerRepo:   providerRepo,
		stateStore:     stateStore,
		jitProvisioner: jitProvisioner,
		sessionStore:   sessionStore,
		tdb:            tdb,
		cfg:            cfg,
		cookieOpts:     platformauth.CookieOptionsFromConfig(cfg),
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// StartOIDC handles GET /api/v1/auth/sso/oidc/:idp_id
//
// Looks up the IdP configuration, generates state + PKCE, persists them, and
// redirects the browser to the IdP authorization endpoint.
func (s *Service) StartOIDC(c *gin.Context) {
	if s.oidcProvider == nil {
		httpx.RespondError(c, http.StatusNotImplemented, "SSO_NOT_CONFIGURED", "OIDC provider not configured")
		return
	}

	tenantID, idpID, err := s.resolveTenantAndIDP(c)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_IDP", err.Error())
		return
	}

	idp, err := s.providerRepo.FindByID(c.Request.Context(), tenantID, idpID)
	if err != nil {
		httpx.RespondError(c, http.StatusNotFound, "IDP_NOT_FOUND", "identity provider not found")
		return
	}
	if !idp.Enabled {
		httpx.RespondError(c, http.StatusForbidden, "IDP_DISABLED", "identity provider is disabled")
		return
	}
	if idp.Protocol != ProtocolOIDC {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_PROTOCOL", "identity provider is not OIDC")
		return
	}

	// Generate cryptographically random state.
	state, err := generateRandomState()
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	var redirectURL, codeVerifier, nonce string

	// Use the concrete type's PKCE method when available.
	if impl, ok := s.oidcProvider.(*OIDCProviderImpl); ok {
		redirectURL, codeVerifier, nonce, err = impl.AuthRedirectURLWithPKCE(c.Request.Context(), idp, state)
	} else {
		redirectURL, err = s.oidcProvider.AuthRedirectURL(c.Request.Context(), idp, state)
	}
	if err != nil {
		httpx.RespondError(c, http.StatusInternalServerError, "SSO_ERROR", "failed to build redirect URL")
		return
	}

	// Persist state, PKCE verifier, and nonce.
	if saveErr := s.stateStore.Save(c.Request.Context(), tenantID, idpID, state, codeVerifier, nonce); saveErr != nil {
		httpx.RespondInternalError(c)
		return
	}

	c.Redirect(http.StatusFound, redirectURL)
}

// CallbackOIDC handles GET /api/v1/auth/sso/oidc/callback
//
// Verifies the state, exchanges the code, validates the ID token, provisions
// the user (JIT), and issues a session.
func (s *Service) CallbackOIDC(c *gin.Context) {
	if s.oidcProvider == nil {
		httpx.RespondError(c, http.StatusNotImplemented, "SSO_NOT_CONFIGURED", "OIDC provider not configured")
		return
	}

	state := c.Query("state")
	code := c.Query("code")

	if state == "" || code == "" {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_CALLBACK", "missing state or code")
		return
	}

	// Infer tenant from the state (stored with tenant scope).
	// The tenant is part of the state lookup key (tenant_id, state).
	// To find the tenant we require it as a query param or header set by the IdP
	// as RelayState-equivalent. For OIDC we encode tenantID into the state prefix.
	tenantID, err := s.resolveTenantFromRequest(c)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_TENANT", "cannot resolve tenant")
		return
	}

	// Consume state (verifies and deletes in one operation).
	codeVerifier, nonce, idpID, err := s.stateStore.Consume(c.Request.Context(), tenantID, state)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_STATE", "invalid or expired state")
		return
	}

	idp, err := s.providerRepo.FindByID(c.Request.Context(), tenantID, idpID)
	if err != nil {
		httpx.RespondError(c, http.StatusInternalServerError, "SSO_ERROR", "failed to load IdP config")
		return
	}

	claims, err := s.oidcProvider.HandleCallback(c.Request.Context(), idp, CallbackParams{
		Code:         code,
		State:        state,
		PKCEVerifier: codeVerifier,
		Nonce:        nonce,
	})
	if err != nil {
		httpx.RespondError(c, http.StatusUnauthorized, "SSO_INVALID_TOKEN", "SSO authentication failed")
		return
	}

	user, err := s.provisionAndIssueSession(c, tenantID, idpID, claims, idp)
	if err != nil {
		return // handler already wrote the response
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":   user.UserID,
		"tenant_id": user.TenantID,
		"is_new":    user.IsNew,
	})
}

// StartSAML handles GET /api/v1/auth/sso/saml/:idp_id
func (s *Service) StartSAML(c *gin.Context) {
	if s.samlProvider == nil {
		httpx.RespondError(c, http.StatusNotImplemented, "SSO_NOT_CONFIGURED", "SAML provider not configured")
		return
	}

	tenantID, idpID, err := s.resolveTenantAndIDP(c)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_IDP", err.Error())
		return
	}

	idp, err := s.providerRepo.FindByID(c.Request.Context(), tenantID, idpID)
	if err != nil {
		httpx.RespondError(c, http.StatusNotFound, "IDP_NOT_FOUND", "identity provider not found")
		return
	}
	if !idp.Enabled {
		httpx.RespondError(c, http.StatusForbidden, "IDP_DISABLED", "identity provider is disabled")
		return
	}
	if idp.Protocol != ProtocolSAML {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_PROTOCOL", "identity provider is not SAML")
		return
	}

	state, err := generateRandomState()
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	var redirectURL, authnRequestID string
	if impl, ok := s.samlProvider.(*SAMLProviderImpl); ok {
		redirectURL, authnRequestID, err = impl.AuthRedirectURLWithRequestID(c.Request.Context(), idp, state)
	} else {
		redirectURL, err = s.samlProvider.AuthRedirectURL(c.Request.Context(), idp, state)
	}
	if err != nil {
		httpx.RespondError(c, http.StatusInternalServerError, "SSO_ERROR", "failed to build SAML request")
		return
	}

	if saveErr := s.stateStore.Save(c.Request.Context(), tenantID, idpID, state, "", authnRequestID); saveErr != nil {
		httpx.RespondInternalError(c)
		return
	}

	c.Redirect(http.StatusFound, redirectURL)
}

// ACSSAML handles POST /api/v1/auth/sso/saml/acs (Assertion Consumer Service)
func (s *Service) ACSSAML(c *gin.Context) {
	if s.samlProvider == nil {
		httpx.RespondError(c, http.StatusNotImplemented, "SSO_NOT_CONFIGURED", "SAML provider not configured")
		return
	}

	samlResponse := c.PostForm("SAMLResponse")
	relayState := c.PostForm("RelayState")

	if samlResponse == "" {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ACS", "missing SAMLResponse")
		return
	}

	tenantID, err := s.resolveTenantFromRequest(c)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_TENANT", "cannot resolve tenant")
		return
	}

	// RelayState acts as the state value for SAML.
	_, authnRequestID, idpID, err := s.stateStore.Consume(c.Request.Context(), tenantID, relayState)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_STATE", "invalid or expired relay state")
		return
	}

	idp, err := s.providerRepo.FindByID(c.Request.Context(), tenantID, idpID)
	if err != nil {
		httpx.RespondError(c, http.StatusInternalServerError, "SSO_ERROR", "failed to load IdP config")
		return
	}

	var requestIDs []string
	if authnRequestID != "" {
		requestIDs = []string{authnRequestID}
	}

	claims, err := s.samlProvider.HandleCallback(c.Request.Context(), idp, CallbackParams{
		SAMLResponse:   samlResponse,
		RelayState:     relayState,
		SAMLRequestIDs: requestIDs,
	})
	if err != nil {
		httpx.RespondError(c, http.StatusUnauthorized, "SSO_INVALID_ASSERTION", "SAML authentication failed")
		return
	}

	user, err := s.provisionAndIssueSession(c, tenantID, idpID, claims, idp)
	if err != nil {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":   user.UserID,
		"tenant_id": user.TenantID,
		"is_new":    user.IsNew,
	})
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// provisionAndIssueSession runs JIT provisioning and issues a session cookie.
// On error it writes the HTTP response and returns a non-nil error so the
// caller knows to stop processing.
func (s *Service) provisionAndIssueSession(c *gin.Context, tenantID, idpID uuid.UUID, claims UserClaims, idp IdentityProvider) (ProvisionedUser, error) {
	// Load JIT config from the idp row (not currently in the IdentityProvider
	// domain model — the JIT config is loaded alongside the IdP config from DB).
	// For now we load the JIT config by fetching the raw DB row separately.
	// TODO: embed JITConfig into IdentityProvider domain model for clarity.
	jitCfg, err := s.loadJITConfig(c.Request.Context(), tenantID, idpID)
	if err != nil {
		httpx.RespondInternalError(c)
		return ProvisionedUser{}, err
	}

	user, err := s.jitProvisioner.ProvisionOrGet(c.Request.Context(), tenantID, idpID, claims, jitCfg)
	if err != nil {
		switch {
		case err == ErrJITDisabled:
			httpx.RespondError(c, http.StatusForbidden, "JIT_DISABLED", "JIT provisioning is disabled")
		case err == ErrEmailDomainNotAllowed:
			httpx.RespondError(c, http.StatusForbidden, "EMAIL_DOMAIN_NOT_ALLOWED", "email domain not permitted")
		default:
			httpx.RespondError(c, http.StatusForbidden, "PROVISIONING_FAILED", "user provisioning failed")
		}
		return ProvisionedUser{}, err
	}

	// Issue session using the standard session store.
	ip := parseClientIP(c)
	rawToken, sessionErr := s.sessionStore.Create(c.Request.Context(), s.tdb, tenantID, user.UserID, s.cfg.SessionTTL, ip)
	if sessionErr != nil {
		httpx.RespondInternalError(c)
		return ProvisionedUser{}, sessionErr
	}

	platformauth.SetSessionCookie(c.Writer, rawToken, s.cookieOpts)
	return user, nil
}

// loadJITConfig retrieves the jit_config JSONB column for the given IdP.
func (s *Service) loadJITConfig(ctx context.Context, tenantID, idpID uuid.UUID) (JITConfig, error) {
	var raw []byte
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT jit_config FROM identity_providers WHERE id = ? AND tenant_id = ? LIMIT 1`,
			idpID, tenantID,
		).Scan(&raw).Error
	})
	if err != nil {
		return JITConfig{}, fmt.Errorf("sso: load JIT config: %w", err)
	}

	var j jitConfigJSON
	if len(raw) > 0 && string(raw) != "null" && string(raw) != "{}" {
		if err := json.Unmarshal(raw, &j); err != nil {
			return JITConfig{}, fmt.Errorf("sso: unmarshal jit_config: %w", err)
		}
	}

	rules := make([]RoleMappingRule, 0, len(j.RoleMappingRules))
	for _, r := range j.RoleMappingRules {
		rules = append(rules, RoleMappingRule{IDPGroup: r.IDPGroup, AppRole: r.AppRole})
	}

	return JITConfig{
		Enabled:             j.Enabled,
		DefaultRole:         j.DefaultRole,
		RoleMappingRules:    rules,
		AllowedEmailDomains: j.AllowedEmailDomains,
	}, nil
}

// resolveTenantAndIDP extracts tenant_id (query param) and idp_id (path param).
func (s *Service) resolveTenantAndIDP(c *gin.Context) (uuid.UUID, uuid.UUID, error) {
	tenantIDStr := c.Query("tenant_id")
	idpIDStr := c.Param("idp_id")

	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("invalid tenant_id")
	}
	idpID, err := uuid.Parse(idpIDStr)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("invalid idp_id")
	}
	return tenantID, idpID, nil
}

// resolveTenantFromRequest extracts tenant_id from the request.
func (s *Service) resolveTenantFromRequest(c *gin.Context) (uuid.UUID, error) {
	tenantIDStr := c.Query("tenant_id")
	if tenantIDStr == "" {
		// Fall back to the authenticated session tenant (if the user is already
		// partially authenticated).
		tenantIDStr = c.GetHeader("X-Tenant-ID")
	}
	return uuid.Parse(tenantIDStr)
}

// generateRandomState generates a 32-byte cryptographically random state value
// encoded as base64url (no padding).
func generateRandomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("sso: generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// parseClientIP extracts the client IP from a Gin context for session creation.
func parseClientIP(c *gin.Context) net.IP {
	raw := c.ClientIP()
	if raw == "" {
		return nil
	}
	return net.ParseIP(raw)
}
