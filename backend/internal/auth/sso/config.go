package sso

// OIDCConfig holds the configuration required to connect to an OpenID Connect
// Identity Provider (e.g. Google, Microsoft Entra ID, Okta).
//
// All secrets (ClientSecret) must be loaded from environment variables or a
// Secret Manager and must never be hard-coded, logged, or returned in API
// responses. Only placeholder / zero values are stored in this struct
// definition — real values are injected at runtime.
//
// Algorithm note: the implementation MUST specify the expected signing
// algorithm explicitly when validating ID tokens. The "alg=none" value and
// algorithm-confusion attacks (e.g. RS256→HS256) are rejected by always
// passing the expected algorithm to the JWT library rather than accepting
// whatever algorithm the token header claims.
//
// See: https://auth0.com/blog/critical-vulnerabilities-in-json-web-token-libraries/
type OIDCConfig struct {
	// IssuerURL is the OpenID Connect issuer URL (the "iss" claim value and
	// discovery endpoint base). Must be HTTPS in production.
	//
	// Examples:
	//   Google:       https://accounts.google.com
	//   Entra ID:     https://login.microsoftonline.com/{tenant-id}/v2.0
	//   Okta:         https://your-org.okta.com
	IssuerURL string

	// ClientID is the OAuth2 client identifier issued by the IdP.
	// Not a secret; may appear in redirect URLs.
	ClientID string

	// ClientSecret is the OAuth2 client secret.
	//
	// SECURITY: must be loaded from an environment variable or Secret Manager.
	// Never hard-code, log, or include in API responses.
	// TODO: replace with a SecretRef type that loads the value lazily from
	// the configured secrets backend (Vault, AWS SM, GCP SM, etc.).
	ClientSecret string // loaded from env / Secret Manager at runtime

	// RedirectURL is the callback URL registered with the IdP.
	// Must match exactly what is registered in the IdP application settings.
	// Example: https://app.example.com/api/v1/auth/sso/oidc/callback
	RedirectURL string

	// Scopes lists the OAuth2 scopes to request. "openid" is always required.
	// Typical additional scopes: "email", "profile".
	// Default (when empty): ["openid", "email", "profile"]
	Scopes []string

	// ExpectedAlgorithm is the JWT signing algorithm the server expects.
	// MUST be one of: "RS256", "ES256", "RS384", "ES384", "RS512", "ES512".
	// "HS256" is only acceptable when the IdP documentation explicitly requires
	// it for confidential clients and the shared secret is stored securely.
	// The value "none" is NEVER accepted and will be rejected at validation.
	//
	// Google uses RS256; Entra ID uses RS256.
	// Default (when empty): "RS256"
	ExpectedAlgorithm string

	// AllowedAudiences lists the "aud" claim values that are accepted.
	// If empty, only ClientID is accepted. Providing an explicit allow-list
	// is recommended to prevent token confusion across applications sharing
	// the same IdP tenant.
	AllowedAudiences []string

	// ExpectedTenantID is the Azure AD / Entra ID tenant GUID that must appear
	// in the "tid" claim of every ID token when this application is registered
	// as a multi-tenant app (issuer contains "common" or "organizations").
	//
	// SECURITY: When set, HandleCallback verifies BOTH:
	//   1. id_token["tid"] == ExpectedTenantID  (exact, case-insensitive)
	//   2. id_token.Issuer contains ExpectedTenantID  (defence-in-depth)
	// An ID token from a different tenant is rejected with ErrInvalidAssertion
	// even when its signature is valid against the IdP's public keys.
	//
	// Leave empty for:
	//   - Single-tenant Entra ID apps (issuer already encodes the tenant ID).
	//   - Google / Okta (the "tid" claim is not issued by these providers).
	ExpectedTenantID string
}

// SAMLConfig holds the configuration required to act as a SAML 2.0 Service
// Provider (SP) against an external Identity Provider (IdP).
//
// SAML credentials (SPPrivateKey, IDPCertificate) must be loaded from a
// Secret Manager or secure key store and must never be hard-coded.
//
// # Security contract for SAML assertion validation
//
//  1. The InResponseTo attribute must match an outstanding AuthnRequest ID
//     stored server-side (prevents unsolicited response / IdP-initiated
//     replay attacks).
//  2. The Conditions element's NotBefore / NotOnOrAfter window is enforced
//     with a configurable clock skew tolerance (default: 30 s).
//  3. The AudienceRestriction/Audience value must match SPentityid.
//  4. SubjectConfirmation Method must be "bearer".
//  5. The Response and/or Assertion signature must be verified against
//     IDPCertificate. An unsigned or self-signed assertion is rejected.
type SAMLConfig struct {
	// SPentityid is the SP Entity ID (URI) registered with the IdP.
	// Example: https://app.example.com/saml/metadata
	SPENTITYID string

	// IDPMETADATAURL is the URL of the IdP SAML metadata document.
	// The server fetches and caches this on startup; the certificate embedded
	// in the metadata is used for assertion signature verification.
	//
	// Alternative: supply IDPCertificate directly when the metadata URL is
	// unavailable (e.g. Entra ID with a static certificate).
	IDPMetadataURL string

	// IDPCertificate is the PEM-encoded X.509 certificate used by the IdP to
	// sign SAML assertions. Required when IDPMetadataURL is not set or when
	// certificate pinning is desired.
	//
	// SECURITY: loaded from env / Secret Manager. Never log this value.
	IDPCertificate string // loaded from env / Secret Manager at runtime

	// ACSURL is the Assertion Consumer Service URL (SP callback endpoint).
	// Must match what is registered in the IdP.
	// Example: https://app.example.com/api/v1/auth/sso/saml/acs
	ACSURL string

	// SPPrivateKey is the PEM-encoded RSA or EC private key used to sign
	// AuthnRequests (optional — required only when the IdP mandates signed
	// requests).
	//
	// SECURITY: loaded from env / Secret Manager. Never log this value.
	SPPrivateKey string // loaded from env / Secret Manager at runtime

	// SPCertificate is the PEM-encoded X.509 certificate corresponding to
	// SPPrivateKey, included in AuthnRequests for the IdP to verify them.
	SPCertificate string

	// NameIDFormat is the SAML NameID format to request from the IdP.
	// Default (when empty): "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
	NameIDFormat string

	// AllowedClockSkew is the tolerance (in seconds) for assertion
	// NotBefore / NotOnOrAfter conditions. Default: 30.
	AllowedClockSkewSeconds int

	// AttributeMap maps IdP-specific attribute names to normalised claim names
	// used by UserClaims (e.g. "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress" → "email").
	AttributeMap map[string]string
}

// RoleMappingRule maps an IdP group/role claim value to an application role
// name. Rules are evaluated in order; the first match wins.
//
// Example:
//
//	{IDPGroup: "hr-admins", AppRole: "admin"}
//	{IDPGroup: "hr-users",  AppRole: "employee"}
type RoleMappingRule struct {
	// IDPGroup is the group or role value from UserClaims.Groups.
	IDPGroup string

	// AppRole is the application role name to assign to the user.
	AppRole string
}

// JITConfig controls Just-In-Time provisioning behaviour when a user
// authenticates via SSO for the first time.
type JITConfig struct {
	// Enabled controls whether JIT provisioning is active.
	// When false, only pre-provisioned users may log in via SSO.
	Enabled bool

	// DefaultRole is the application role assigned to JIT-provisioned users
	// when no RoleMappingRule matches. Defaults to the least-privileged role.
	// MUST be set explicitly; an empty value blocks JIT provisioning.
	DefaultRole string

	// RoleMappingRules are evaluated in order against UserClaims.Groups.
	// The first matching rule's AppRole overrides DefaultRole.
	RoleMappingRules []RoleMappingRule

	// AllowedEmailDomains restricts JIT provisioning to specific email
	// domains (e.g. ["example.com"]). Empty means all domains are allowed.
	AllowedEmailDomains []string
}
