// Package sso provides the scaffolding for SSO/IdP integration (OIDC and
// SAML 2.0). It defines the IdentityProvider and SSOProvider interfaces, safe
// stub implementations, and the configuration structures needed when real
// external credentials are available.
//
// # Current state
//
// All concrete implementations are stubs that return ErrNotImplemented. No
// external IdP credentials are required to compile or run the server. Real
// implementations must be wired in once credentials are provisioned (see TODO
// comments in each stub file).
//
// # Security invariants
//
//   - JWT/ID token verification always specifies a fixed, expected algorithm
//     (HS256 / RS256 / ES256). The "alg=none" and algorithm-confusion attacks
//     are explicitly rejected (see oidc_stub.go).
//   - Client secrets and private keys are never logged, stored in plain text,
//     or returned in API responses.
//   - SAML assertions are validated for audience, conditions, and subject
//     confirmation before any claims are trusted (stub documents the contract).
//   - Tenant isolation is enforced: every IdP configuration is scoped to a
//     single tenant_id; cross-tenant look-ups are not permitted.
//   - JIT-provisioned users default to the least-privileged role and require
//     explicit role-mapping configuration to receive elevated permissions.
//
// # Wiring
//
// TODO(server): Register SSO HTTP routes and middleware in
// internal/server/server.go once an SSOProvider implementation is injected.
// The suggested route group is /api/v1/auth/sso/:provider.
package sso

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrNotImplemented is returned by all stub implementations. Replace with a
// real implementation once external IdP credentials are available.
var ErrNotImplemented = errors.New("sso: not implemented — stub only")

// ErrProviderDisabled is returned when the requested SSO provider is disabled
// for the tenant.
var ErrProviderDisabled = errors.New("sso: provider is disabled for this tenant")

// ErrInvalidAssertion is returned when an incoming SAML assertion or OIDC ID
// token fails validation (wrong audience, expired, bad signature, etc.).
var ErrInvalidAssertion = errors.New("sso: invalid assertion or ID token")

// Protocol identifies the SSO protocol used by a given provider configuration.
type Protocol string

const (
	// ProtocolOIDC represents OpenID Connect (RFC 8414 / RFC 7519).
	ProtocolOIDC Protocol = "oidc"

	// ProtocolSAML represents SAML 2.0 (OASIS).
	ProtocolSAML Protocol = "saml"
)

// IdentityProvider represents a tenant-scoped IdP configuration.
// It carries the tenant association and protocol type so that the
// application layer can dispatch to the correct SSOProvider implementation
// without knowledge of protocol-level details.
type IdentityProvider struct {
	// ID is the unique identifier for this IdP configuration row.
	ID uuid.UUID

	// TenantID scopes this configuration to a single tenant.
	// Cross-tenant usage is a security violation; callers must verify this
	// field matches the authenticated tenant before trusting any claims.
	TenantID uuid.UUID

	// Protocol is the SSO protocol (OIDC or SAML).
	Protocol Protocol

	// Enabled controls whether this IdP is currently active for the tenant.
	// Disabled providers return ErrProviderDisabled without attempting auth.
	Enabled bool

	// OIDCConfig holds OIDC-specific settings. Nil when Protocol != OIDC.
	OIDCConfig *OIDCConfig

	// SAMLConfig holds SAML-specific settings. Nil when Protocol != SAML.
	SAMLConfig *SAMLConfig
}

// UserClaims carries the normalised identity attributes extracted from a
// successful SSO assertion. The raw provider response is intentionally
// discarded to avoid leaking protocol-specific details into the application
// layer.
//
// All string fields are sanitised (trimmed, lower-cased email) before
// construction. PII contained here must not be logged.
type UserClaims struct {
	// SubjectID is the stable, unique identifier for the user at the IdP
	// (OIDC "sub" claim, SAML NameID). Never empty.
	SubjectID string

	// Email is the user's email address as reported by the IdP.
	// May be empty if the IdP does not include it in the assertion.
	Email string

	// DisplayName is the user's display name (optional).
	DisplayName string

	// Groups contains IdP group/role memberships used for role mapping.
	// May be empty; callers must not assume any particular value.
	Groups []string

	// RawAttributes holds additional IdP-specific attributes for debugging.
	// Must never be included in API responses or logs.
	RawAttributes map[string]string
}

// SSOProvider is the interface that a concrete OIDC or SAML implementation
// must satisfy. The application layer calls only this interface; it has no
// knowledge of the underlying protocol.
//
// All methods must:
//   - Enforce tenant isolation (reject requests where idp.TenantID does not
//     match the caller's authenticated tenant).
//   - Return ErrProviderDisabled when idp.Enabled == false.
//   - Return ErrNotImplemented in stub implementations.
//   - Never log credentials, tokens, or PII.
type SSOProvider interface {
	// Protocol returns the SSO protocol handled by this provider.
	Protocol() Protocol

	// AuthRedirectURL returns the URL to which the browser should be redirected
	// to initiate the SSO flow (OAuth2 authorisation endpoint / SAML AuthnRequest).
	//
	// state is an opaque, cryptographically random value that the caller must
	// store in the session and verify on callback to prevent CSRF.
	//
	// TODO: implement PKCE (RFC 7636) for OIDC flows.
	AuthRedirectURL(ctx context.Context, idp IdentityProvider, state string) (redirectURL string, err error)

	// HandleCallback processes the IdP callback (OIDC code exchange or SAML
	// response POST). On success it returns normalised UserClaims.
	//
	// For OIDC: exchanges the authorisation code for tokens, validates the
	// ID token (alg fixed, audience verified, expiry checked).
	//
	// For SAML: parses and validates the SAML Response XML (signature,
	// conditions, audience, subject confirmation method).
	//
	// Security contract: ErrInvalidAssertion is returned for any validation
	// failure; the caller must NOT issue a session on this error.
	HandleCallback(ctx context.Context, idp IdentityProvider, callbackParams CallbackParams) (UserClaims, error)
}

// CallbackParams holds the raw parameters received in the IdP callback
// request. The SSOProvider implementation selects the relevant fields
// based on the protocol.
type CallbackParams struct {
	// OIDC fields
	Code  string // authorisation code
	State string // must match the state stored in the user's session

	// SAML fields
	SAMLResponse string // base64-encoded SAMLResponse POST parameter
	RelayState   string // SAML equivalent of OIDC state
}

// ProviderRepository is the read-only data-access interface for loading
// tenant-scoped IdentityProvider configurations from the data store.
//
// TODO(db): implement a PostgreSQL-backed ProviderRepository once the
// identity_providers table migration is applied (migration 00033 or later).
// The table must have RLS enabled (ENABLE ROW LEVEL SECURITY + FORCE ROW
// LEVEL SECURITY) and a composite FK on (id, tenant_id) matching the
// cross-tenant hardening pattern in 00027.
type ProviderRepository interface {
	// FindByID returns the IdentityProvider with the given id, scoped to
	// tenantID. Returns an error if the provider does not belong to the tenant.
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (IdentityProvider, error)

	// ListByTenant returns all active IdentityProviders for a tenant.
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]IdentityProvider, error)
}
