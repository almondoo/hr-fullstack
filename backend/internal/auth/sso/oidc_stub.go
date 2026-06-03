package sso

import (
	"context"
	"errors"
	"strings"
)

// OIDCProvider is a stub implementation of SSOProvider for the OIDC protocol.
//
// # Current state
//
// All methods return ErrNotImplemented. The struct documents the security
// contract and algorithm constraints that MUST be followed when the real
// implementation is added.
//
// # Implementation guide (TODO)
//
//  1. Add a dependency on an OIDC library that supports algorithm pinning,
//     e.g. github.com/coreos/go-oidc/v3 (uses go-jose which enforces alg).
//     Do NOT use a bare JWT library that trusts the token header's "alg"
//     field without external validation — that enables alg-confusion attacks.
//
//  2. In NewOIDCProvider, call provider.Verifier(&oidc.Config{
//       ClientID: cfg.ClientID,
//     }) — this pins the audience.  Additionally pass the expected algorithm
//     via the verifier config once go-oidc exposes it (or verify alg yourself
//     before calling Verify).
//
//  3. Reject tokens where header.alg == "none" or header.alg is not in the
//     AllowedAlgorithms list.  Never call jwt.ParseUnverified before checking
//     the algorithm.
//
//  4. Use PKCE (RFC 7636) for the authorisation code flow to prevent code
//     interception attacks.
//
//  5. Verify the "nonce" claim when present (OIDC hybrid / implicit flows).
//
//  6. For Entra ID (multi-tenant): verify "tid" and "iss" match the expected
//     Entra tenant to prevent cross-tenant token replay.
//
// # Environment variables required (not yet validated)
//
//   - OIDC_ISSUER_URL       — IdP discovery URL (e.g. https://accounts.google.com)
//   - OIDC_CLIENT_ID        — OAuth2 client identifier
//   - OIDC_CLIENT_SECRET    — OAuth2 client secret (Secret Manager preferred)
//   - OIDC_REDIRECT_URL     — callback URL registered with the IdP
//
// These are intentionally NOT loaded in this stub; they are documented here
// as the environment surface that the real implementation will consume.
type OIDCProvider struct {
	// TODO: inject *oidc.Provider, oauth2.Config, and OIDCConfig here
	// when wiring the real implementation.
}

// allowedOIDCAlgorithms is the set of JWT signing algorithms this server
// accepts. "none" is explicitly excluded.
//
// When the real implementation validates ID tokens it MUST verify that the
// token header's "alg" field is one of these values before processing any
// claims. If the library does not enforce this automatically, add an explicit
// check in HandleCallback.
var allowedOIDCAlgorithms = map[string]bool{
	"RS256": true,
	"RS384": true,
	"RS512": true,
	"ES256": true,
	"ES384": true,
	"ES512": true,
	// HS256 is intentionally omitted from the default set because it requires
	// sharing the client secret with the server and is vulnerable to offline
	// brute-force.  Enable only if the IdP documentation explicitly requires it
	// AND the secret is loaded from a Secret Manager.
}

// Protocol implements SSOProvider.
func (p *OIDCProvider) Protocol() Protocol { return ProtocolOIDC }

// AuthRedirectURL implements SSOProvider.
//
// TODO: build an OAuth2 authorisation URL with PKCE code_challenge and a
// cryptographically random state parameter. Return ErrProviderDisabled when
// idp.Enabled == false.
func (p *OIDCProvider) AuthRedirectURL(_ context.Context, idp IdentityProvider, _ string) (string, error) {
	if !idp.Enabled {
		return "", ErrProviderDisabled
	}
	// TODO: implement using oidc.Provider + oauth2.Config.AuthCodeURL
	return "", ErrNotImplemented
}

// HandleCallback implements SSOProvider.
//
// TODO: exchange the authorisation code for tokens, validate the ID token,
// and return normalised UserClaims. The validation MUST:
//   - Verify the "alg" header against allowedOIDCAlgorithms before decoding.
//   - Verify the signature using the IdP's public keys (JWKS endpoint).
//   - Verify "iss" matches OIDCConfig.IssuerURL.
//   - Verify "aud" contains OIDCConfig.ClientID (or AllowedAudiences).
//   - Verify "exp" > now and "nbf" <= now.
//   - Verify "nonce" when the nonce was included in the AuthnRequest.
//   - Reject tokens where "alg" == "none" unconditionally.
func (p *OIDCProvider) HandleCallback(_ context.Context, idp IdentityProvider, _ CallbackParams) (UserClaims, error) {
	if !idp.Enabled {
		return UserClaims{}, ErrProviderDisabled
	}
	// TODO: implement token exchange and ID token validation
	return UserClaims{}, ErrNotImplemented
}

// validateOIDCAlgorithm is a helper that the real HandleCallback MUST call
// before trusting any claims from an ID token.  It returns an error if the
// algorithm is not in the allowed set or is explicitly "none".
//
// This function is exported for use in unit tests against the real
// implementation; the stub itself does not call it because all paths return
// ErrNotImplemented first.
func validateOIDCAlgorithm(alg string) error {
	if strings.EqualFold(alg, "none") {
		return errors.New("sso: ID token algorithm 'none' is never accepted")
	}
	if !allowedOIDCAlgorithms[alg] {
		return errors.New("sso: ID token algorithm not in allowed set: " + alg)
	}
	return nil
}
