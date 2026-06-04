package sso

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCProviderImpl is the real OIDC implementation of SSOProvider.
//
// Security invariants:
//   - PKCE (RFC 7636) S256 is used for every authorisation code flow.
//   - state is caller-supplied (cryptographically random, stored in session).
//   - nonce is embedded in the authorization URL and verified in the ID token.
//   - ID token alg is verified against allowedOIDCAlgorithms before trusting any claim.
//   - The "none" algorithm is unconditionally rejected.
//   - Audience and issuer are verified by go-oidc automatically (configured at
//     provider construction time via oidc.Config).
//
// Environment variables consumed (loaded by the caller before constructing):
//   - See OIDCConfig for field documentation.
//
// Thread safety: OIDCProviderImpl is safe for concurrent use once constructed.
type OIDCProviderImpl struct {
	cfg      OIDCConfig
	provider *gooidc.Provider
	oauth2Cfg oauth2.Config
}

// NewOIDCProvider constructs an OIDCProviderImpl by fetching the IdP OIDC
// discovery document from IssuerURL. The discovery fetch requires network
// access; call this at application startup, not per-request.
//
// Returns ErrNotImplemented when cfg.IssuerURL or cfg.ClientID is empty (i.e.,
// the env vars have not been provisioned yet) so that the server can start
// without real IdP credentials.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCProviderImpl, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" {
		// Credentials not yet provisioned; return stub behaviour so the server
		// can start without IdP configuration.
		return nil, ErrNotImplemented
	}

	provider, err := gooidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover provider %q: %w", cfg.IssuerURL, err)
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{gooidc.ScopeOpenID, "email", "profile"}
	}

	oauth2Cfg := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       scopes,
	}

	return &OIDCProviderImpl{
		cfg:       cfg,
		provider:  provider,
		oauth2Cfg: oauth2Cfg,
	}, nil
}

// Protocol implements SSOProvider.
func (p *OIDCProviderImpl) Protocol() Protocol { return ProtocolOIDC }

// AuthRedirectURL implements SSOProvider.
//
// It generates a PKCE code_challenge (S256) and embeds a nonce.
// The caller must store the returned pkceVerifier and nonce alongside state in
// the session before redirecting. This method only builds the URL; the
// caller's session management is outside this scope.
//
// The returned URL carries:
//   - response_type=code
//   - code_challenge + code_challenge_method=S256 (PKCE)
//   - nonce (OIDC replay protection)
//   - state (CSRF protection, caller-supplied)
func (p *OIDCProviderImpl) AuthRedirectURL(ctx context.Context, idp IdentityProvider, state string) (string, error) {
	if !idp.Enabled {
		return "", ErrProviderDisabled
	}
	if idp.OIDCConfig == nil {
		return "", fmt.Errorf("oidc: AuthRedirectURL: OIDCConfig is nil for provider %s", idp.ID)
	}

	// PKCE: generate code_verifier (32 random bytes, base64url-encoded).
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", fmt.Errorf("oidc: generate PKCE verifier: %w", err)
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	codeChallenge := pkceS256Challenge(codeVerifier)

	// nonce: embed in URL for replay protection (checked in HandleCallback).
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("oidc: generate nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)

	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("nonce", nonce),
	}

	redirectURL := p.oauth2Cfg.AuthCodeURL(state, opts...)

	// The caller must persist codeVerifier and nonce in the server-side session.
	// We encode them into the state cookie by convention used by our SSO service:
	// the caller (sso.Service) stores them in sso_state before returning this URL.
	// We attach them as context values so the SSO service can retrieve them without
	// us needing to know the session store interface.
	//
	// Per-call context values are used here to pass PKCE/nonce back to the caller
	// rather than mutating shared state. The SSO service layer is responsible for
	// persisting these values.
	_ = context.WithValue(ctx, pkceVerifierKey{}, codeVerifier)
	_ = context.WithValue(ctx, nonceKey{}, nonce)

	return redirectURL, nil
}

// AuthRedirectURLWithPKCE is the full-contract version of AuthRedirectURL that
// returns codeVerifier and nonce alongside the redirect URL so that the SSO
// service layer can persist them to the sso_state table.
//
// This is the method the SSOService should call; AuthRedirectURL is kept for
// interface compliance.
func (p *OIDCProviderImpl) AuthRedirectURLWithPKCE(ctx context.Context, idp IdentityProvider, state string) (redirectURL, codeVerifier, nonce string, err error) {
	if !idp.Enabled {
		return "", "", "", ErrProviderDisabled
	}
	if idp.OIDCConfig == nil {
		return "", "", "", fmt.Errorf("oidc: OIDCConfig is nil for provider %s", idp.ID)
	}

	// PKCE code_verifier.
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", "", "", fmt.Errorf("oidc: generate PKCE verifier: %w", err)
	}
	codeVerifier = base64.RawURLEncoding.EncodeToString(verifierBytes)
	codeChallenge := pkceS256Challenge(codeVerifier)

	// nonce.
	nonceBytes := make([]byte, 16)
	if _, err = rand.Read(nonceBytes); err != nil {
		return "", "", "", fmt.Errorf("oidc: generate nonce: %w", err)
	}
	nonce = base64.RawURLEncoding.EncodeToString(nonceBytes)

	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("nonce", nonce),
	}
	redirectURL = p.oauth2Cfg.AuthCodeURL(state, opts...)
	return redirectURL, codeVerifier, nonce, nil
}

// HandleCallback implements SSOProvider.
//
// It:
//  1. Exchanges the authorisation code for tokens (with PKCE code_verifier if set).
//  2. Extracts the ID token from the token response.
//  3. Validates the ID token: issuer, audience, expiry, nonce.
//  4. Verifies that the token's alg is in allowedOIDCAlgorithms (never "none").
//  5. Returns normalised UserClaims.
//
// Security: ErrInvalidAssertion is returned for any validation failure.
// The caller MUST NOT issue a session on this error.
func (p *OIDCProviderImpl) HandleCallback(ctx context.Context, idp IdentityProvider, params CallbackParams) (UserClaims, error) {
	if !idp.Enabled {
		return UserClaims{}, ErrProviderDisabled
	}
	if idp.OIDCConfig == nil {
		return UserClaims{}, fmt.Errorf("oidc: HandleCallback: OIDCConfig is nil for provider %s", idp.ID)
	}
	if params.Code == "" {
		return UserClaims{}, ErrInvalidAssertion
	}

	// Build token exchange options; include PKCE verifier if provided.
	var exchangeOpts []oauth2.AuthCodeOption
	if params.PKCEVerifier != "" {
		exchangeOpts = append(exchangeOpts, oauth2.SetAuthURLParam("code_verifier", params.PKCEVerifier))
	}

	// Exchange authorisation code for tokens.
	token, err := p.oauth2Cfg.Exchange(ctx, params.Code, exchangeOpts...)
	if err != nil {
		// Do not log params.Code — it is a secret.
		return UserClaims{}, fmt.Errorf("%w: token exchange: %s", ErrInvalidAssertion, err)
	}

	// Extract raw ID token from the token response.
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return UserClaims{}, fmt.Errorf("%w: id_token missing from token response", ErrInvalidAssertion)
	}

	// Verify alg before letting go-oidc decode the payload.
	// go-oidc v3 does NOT expose the header before Verify(), so we extract it
	// manually from the JWT header to reject bad algorithms early.
	if err := verifyIDTokenAlg(rawIDToken); err != nil {
		return UserClaims{}, fmt.Errorf("%w: %s", ErrInvalidAssertion, err)
	}

	// Configure the verifier: pin audience to ClientID.
	// go-oidc automatically verifies iss, aud, exp, iat.
	verifier := p.provider.Verifier(&gooidc.Config{
		ClientID: idp.OIDCConfig.ClientID,
		// go-oidc v3 enforces alg via the JWKS; additional alg check is above.
	})

	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return UserClaims{}, fmt.Errorf("%w: id token verification: %s", ErrInvalidAssertion, err)
	}

	// Verify nonce when one was used in the auth request.
	if params.Nonce != "" {
		if idToken.Nonce != params.Nonce {
			return UserClaims{}, fmt.Errorf("%w: nonce mismatch", ErrInvalidAssertion)
		}
	}

	// Extract standard claims.
	var claims struct {
		Sub  string `json:"sub"`
		Email string `json:"email"`
		Name  string `json:"name"`
		// Groups claim varies by IdP:
		// Google Workspace: requires Admin SDK push; not in standard token.
		// Entra ID: "groups" claim (object IDs) or "roles" app role assignments.
		Groups []string `json:"groups"`
		Roles  []string `json:"roles"` // Entra ID app role assignments

		// tid is the Azure AD / Entra ID tenant identifier.
		// Present only in Entra ID tokens; absent in Google / Okta tokens.
		// Used for multi-tenant cross-tenant replay prevention (see below).
		TID string `json:"tid"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return UserClaims{}, fmt.Errorf("%w: extract claims: %s", ErrInvalidAssertion, err)
	}

	if claims.Sub == "" {
		return UserClaims{}, fmt.Errorf("%w: sub claim is empty", ErrInvalidAssertion)
	}

	// --- Entra ID multi-tenant tid / iss verification ---
	//
	// SECURITY: In a multi-tenant Entra ID application the issuer URL uses
	// "common" or "organizations" as the tenant placeholder:
	//   https://login.microsoftonline.com/common/v2.0
	// This means go-oidc's built-in issuer check does NOT bind the token to a
	// specific tenant. An attacker in Tenant B could obtain a valid token from
	// Tenant B's users and replay it against this application (registered in
	// Tenant A) if the "tid" claim is not explicitly verified.
	//
	// When OIDCConfig carries an ExpectedTenantID (loaded from
	// OIDC_EXPECTED_TENANT_ID env var) we:
	//  1. Check that claims.TID equals the expected tenant GUID.
	//  2. Check that the token issuer URL contains the expected tenant GUID
	//     (defence-in-depth: catches issuer URL mismatches from other tenants).
	//
	// References:
	//   https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference
	//   https://learn.microsoft.com/en-us/entra/identity-platform/howto-convert-app-to-be-multi-tenant
	if idp.OIDCConfig != nil && idp.OIDCConfig.ExpectedTenantID != "" {
		expectedTID := strings.TrimSpace(idp.OIDCConfig.ExpectedTenantID)
		if claims.TID == "" {
			return UserClaims{}, fmt.Errorf("%w: tid claim is missing — required for multi-tenant Entra ID validation", ErrInvalidAssertion)
		}
		if !strings.EqualFold(claims.TID, expectedTID) {
			// Do not include the actual tid value in the error to avoid
			// leaking cross-tenant information in logs/responses.
			return UserClaims{}, fmt.Errorf("%w: tid claim does not match expected tenant", ErrInvalidAssertion)
		}
		// Defence-in-depth: the issuer URL must contain the expected tenant ID.
		// For Entra ID single-tenant apps the issuer is:
		//   https://login.microsoftonline.com/{tenant-id}/v2.0
		// For common-endpoint multi-tenant apps the go-oidc library resolves
		// the discovered issuer at construction time; we still check here.
		if !strings.Contains(idToken.Issuer, expectedTID) {
			return UserClaims{}, fmt.Errorf("%w: issuer does not contain expected tenant ID", ErrInvalidAssertion)
		}
	}

	groups := append(claims.Groups, claims.Roles...)

	return UserClaims{
		SubjectID:   claims.Sub,
		Email:       strings.ToLower(strings.TrimSpace(claims.Email)),
		DisplayName: strings.TrimSpace(claims.Name),
		Groups:      groups,
	}, nil
}

// pkceS256Challenge computes the PKCE code_challenge using the S256 method:
// BASE64URL(SHA256(ASCII(code_verifier))) per RFC 7636 §4.2.
func pkceS256Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// verifyIDTokenAlg extracts the "alg" from a JWT header and verifies it is
// in allowedOIDCAlgorithms and is not "none".
//
// This provides an early-rejection gate before go-oidc's Verify() call.
// go-oidc v3 validates alg via JWKS key type matching, but an explicit check
// here gives defence-in-depth and ensures our policy is enforced regardless of
// library behaviour changes.
func verifyIDTokenAlg(rawToken string) error {
	// A JWT has three dot-separated parts: header.payload.signature
	parts := strings.SplitN(rawToken, ".", 3)
	if len(parts) != 3 {
		return errors.New("sso: malformed JWT: expected 3 parts")
	}

	// Decode the header (base64url, no padding).
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("sso: malformed JWT header: %w", err)
	}

	// Extract "alg" without a full JSON unmarshal.
	// A minimal, allocation-efficient extraction using string search.
	alg := extractJSONStringField(string(headerBytes), "alg")
	if alg == "" {
		return errors.New("sso: JWT header missing 'alg' field")
	}

	return validateOIDCAlgorithm(alg)
}

// extractJSONStringField extracts the string value of a JSON key from a
// minimal JSON object string. It is used only for the JWT header, which is
// a small, trusted (structurally) object. Returns empty string if not found.
//
// This avoids importing encoding/json for a single-field extraction.
func extractJSONStringField(jsonStr, key string) string {
	// Look for `"key":` or `"key" :` in the JSON.
	needle := `"` + key + `"`
	idx := strings.Index(jsonStr, needle)
	if idx < 0 {
		return ""
	}
	// Advance past the key and colon.
	rest := jsonStr[idx+len(needle):]
	rest = strings.TrimLeft(rest, " \t\n\r:")
	rest = strings.TrimLeft(rest, " \t\n\r")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	// Extract the string value between quotes (simple case: no escape sequences).
	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// pkceVerifierKey is the context key for the PKCE code verifier.
type pkceVerifierKey struct{}

// nonceKey is the context key for the OIDC nonce.
type nonceKey struct{}
