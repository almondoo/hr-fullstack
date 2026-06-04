package sso

import (
	"context"
)

// SAMLProvider is a stub implementation of SSOProvider for the SAML 2.0
// protocol.
//
// # Current state
//
// All methods return ErrNotImplemented. The struct documents the security
// contract and validation steps that MUST be followed when the real
// implementation is added.
//
// # Implementation guide (TODO)
//
//  1. Add a dependency on a SAML library with robust assertion validation,
//     e.g. github.com/crewjam/saml. Verify it enforces signature validation,
//     audience restriction, and conditions checks by default.
//
//  2. In AuthRedirectURL, generate a signed (or unsigned, per IdP requirement)
//     AuthnRequest with a unique ID. Store the ID server-side (e.g. in the
//     session store) to verify InResponseTo on callback and prevent
//     unsolicited-response / replay attacks.
//
//  3. In HandleCallback (ACS endpoint), validate:
//       a. Signature: Response or Assertion must be signed with IDPCertificate.
//          An unsigned or self-signed assertion MUST be rejected.
//       b. InResponseTo: must match a pending AuthnRequest ID in the store.
//          After consuming, remove the ID to prevent replay.
//       c. Conditions: NotBefore and NotOnOrAfter with AllowedClockSkewSeconds.
//       d. Audience: AudienceRestriction/Audience must equal SPENTITYID.
//       e. SubjectConfirmation: Method must be
//          "urn:oasis:names:tc:SAML:2.0:cm:bearer".
//       f. NameID: extract and normalise for use as UserClaims.SubjectID.
//
//  4. Apply attribute mapping from SAMLConfig.AttributeMap before constructing
//     UserClaims.
//
//  5. Never log the raw SAMLResponse XML; it may contain sensitive attributes.
//
// # Environment variables required (not yet validated)
//
//   - SAML_SP_ENTITY_ID      — SP Entity ID URI
//   - SAML_IDP_METADATA_URL  — IdP metadata URL (or SAML_IDP_CERTIFICATE for
//     static certificate pinning)
//   - SAML_ACS_URL            — Assertion Consumer Service URL
//   - SAML_SP_PRIVATE_KEY     — PEM private key for signing AuthnRequests
//                               (Secret Manager preferred; optional)
//   - SAML_SP_CERTIFICATE     — PEM certificate matching SAML_SP_PRIVATE_KEY
//
// These are intentionally NOT loaded in this stub; they are documented here
// as the environment surface that the real implementation will consume.
type SAMLProvider struct {
	// TODO: inject saml.ServiceProvider and SAMLConfig here when wiring the
	// real implementation.
}

// Protocol implements SSOProvider.
func (p *SAMLProvider) Protocol() Protocol { return ProtocolSAML }

// AuthRedirectURL implements SSOProvider.
//
// TODO: generate a signed AuthnRequest XML, base64-deflate-encode it, and
// return the IdP SSO URL with SAMLRequest + RelayState query parameters.
// Store the AuthnRequest ID in the session store to validate InResponseTo.
func (p *SAMLProvider) AuthRedirectURL(_ context.Context, idp IdentityProvider, _ string) (string, error) {
	if !idp.Enabled {
		return "", ErrProviderDisabled
	}
	// TODO: implement SAML AuthnRequest generation
	return "", ErrNotImplemented
}

// HandleCallback implements SSOProvider.
//
// TODO: parse and validate the SAMLResponse POST parameter. See the
// implementation guide above for the full validation checklist.
// Return ErrInvalidAssertion for any validation failure.
func (p *SAMLProvider) HandleCallback(_ context.Context, idp IdentityProvider, _ CallbackParams) (UserClaims, error) {
	if !idp.Enabled {
		return UserClaims{}, ErrProviderDisabled
	}
	// TODO: implement SAML assertion validation
	return UserClaims{}, ErrNotImplemented
}
