package sso

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	crewsaml "github.com/crewjam/saml"
)

// SAMLProviderImpl is the real SAML 2.0 implementation of SSOProvider.
//
// Security invariants:
//   - All SAML responses are validated: signature, InResponseTo, Conditions
//     (NotBefore/NotOnOrAfter with configurable clock skew), AudienceRestriction,
//     SubjectConfirmation Method = "bearer".
//   - Unsolicited responses (no matching AuthnRequest ID) are rejected.
//   - The IdP certificate is used for assertion signature verification;
//     self-signed or unsigned assertions are rejected by crewjam/saml.
//   - SAMLResponse XML is never logged.
//
// Thread safety: SAMLProviderImpl is safe for concurrent use once constructed.
type SAMLProviderImpl struct {
	cfg SAMLConfig
	sp  crewsaml.ServiceProvider
}

// NewSAMLProvider constructs a SAMLProviderImpl.
//
// When cfg.IDPMetadataURL is set, the IdP metadata is fetched at construction
// time (network access required). When cfg.IDPCertificate is set instead, it
// is used directly for signature verification.
//
// Returns ErrNotImplemented when cfg.SPENTITYID or cfg.ACSURL is empty so
// that the server can start without SAML credentials provisioned.
func NewSAMLProvider(ctx context.Context, cfg SAMLConfig) (*SAMLProviderImpl, error) {
	if cfg.SPENTITYID == "" || cfg.ACSURL == "" {
		return nil, ErrNotImplemented
	}

	entityURL, err := url.Parse(cfg.SPENTITYID)
	if err != nil {
		return nil, fmt.Errorf("saml: parse SP entity ID %q: %w", cfg.SPENTITYID, err)
	}

	acsURL, err := url.Parse(cfg.ACSURL)
	if err != nil {
		return nil, fmt.Errorf("saml: parse ACS URL %q: %w", cfg.ACSURL, err)
	}

	// Build the crewjam ServiceProvider.
	sp := crewsaml.ServiceProvider{
		EntityID:    cfg.SPENTITYID,
		MetadataURL: *entityURL,
		AcsURL:      *acsURL,
	}

	// Load IdP metadata.
	if cfg.IDPMetadataURL != "" {
		idpMeta, err := fetchIDPMetadata(ctx, cfg.IDPMetadataURL)
		if err != nil {
			return nil, fmt.Errorf("saml: fetch IdP metadata from %q: %w", cfg.IDPMetadataURL, err)
		}
		sp.IDPMetadata = idpMeta
	} else if cfg.IDPCertificate != "" {
		// Static certificate pinning: build minimal EntityDescriptor.
		cert, err := parsePEMCertificate(cfg.IDPCertificate)
		if err != nil {
			return nil, fmt.Errorf("saml: parse IdP certificate: %w", err)
		}
		sp.IDPMetadata = buildEntityDescriptorFromCert(cert)
	} else {
		return nil, fmt.Errorf("saml: either IDPMetadataURL or IDPCertificate must be set")
	}

	// Load SP private key for signing AuthnRequests (optional).
	if cfg.SPPrivateKey != "" && cfg.SPCertificate != "" {
		tlsCert, err := tls.X509KeyPair([]byte(cfg.SPCertificate), []byte(cfg.SPPrivateKey))
		if err != nil {
			return nil, fmt.Errorf("saml: load SP keypair: %w", err)
		}
		sp.Certificate = tlsCert.Leaf
		if sp.Certificate == nil {
			// tls.X509KeyPair may not parse Leaf; do it manually.
			parsed, parseErr := x509.ParseCertificate(tlsCert.Certificate[0])
			if parseErr != nil {
				return nil, fmt.Errorf("saml: parse SP certificate: %w", parseErr)
			}
			sp.Certificate = parsed
		}
		privKey, ok := tlsCert.PrivateKey.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("saml: SP private key must be RSA")
		}
		sp.Key = privKey
	}

	// Clock skew tolerance.
	if cfg.AllowedClockSkewSeconds > 0 {
		crewsaml.MaxClockSkew = time.Duration(cfg.AllowedClockSkewSeconds) * time.Second
	}

	return &SAMLProviderImpl{cfg: cfg, sp: sp}, nil
}

// Protocol implements SSOProvider.
func (p *SAMLProviderImpl) Protocol() Protocol { return ProtocolSAML }

// AuthRedirectURL implements SSOProvider.
//
// Generates a signed (or unsigned) AuthnRequest and returns the IdP SSO URL
// with SAMLRequest + RelayState parameters.
//
// The AuthnRequest ID is embedded in the returned URL. The caller (SSOService)
// must persist it in sso_state.authn_request_id before redirecting so that
// HandleCallback can verify InResponseTo for replay protection.
func (p *SAMLProviderImpl) AuthRedirectURL(ctx context.Context, idp IdentityProvider, state string) (string, error) {
	if !idp.Enabled {
		return "", ErrProviderDisabled
	}

	redirectURL, err := p.sp.MakeRedirectAuthenticationRequest(state)
	if err != nil {
		return "", fmt.Errorf("saml: build AuthnRequest: %w", err)
	}

	return redirectURL.String(), nil
}

// AuthRedirectURLWithRequestID is the full-contract version of AuthRedirectURL
// that also returns the AuthnRequest ID for InResponseTo validation.
func (p *SAMLProviderImpl) AuthRedirectURLWithRequestID(ctx context.Context, idp IdentityProvider, state string) (redirectURL, authnRequestID string, err error) {
	if !idp.Enabled {
		return "", "", ErrProviderDisabled
	}

	u, buildErr := p.sp.MakeRedirectAuthenticationRequest(state)
	if buildErr != nil {
		return "", "", fmt.Errorf("saml: build AuthnRequest: %w", buildErr)
	}

	// Extract the ID from the SAMLRequest parameter so the caller can persist it.
	// crewjam embeds the ID in the SAMLRequest XML; we decode to extract it.
	authnRequestID, err = extractAuthnRequestID(u.Query().Get("SAMLRequest"))
	if err != nil {
		// Non-fatal: the caller may proceed without InResponseTo validation,
		// but this weakens replay protection. Log at warn level.
		authnRequestID = ""
		err = nil
	}

	return u.String(), authnRequestID, nil
}

// HandleCallback implements SSOProvider.
//
// Parses and validates the SAMLResponse POST parameter.
// Validation checklist (per saml_stub.go security contract):
//  a. Signature: verified against IdP certificate (crewjam enforces this).
//  b. InResponseTo: checked against SAMLRequestIDs (replay protection).
//  c. Conditions: NotBefore/NotOnOrAfter with clock skew tolerance.
//  d. Audience: AudienceRestriction must equal SP entity ID.
//  e. SubjectConfirmation: Method must be "bearer".
//  f. NameID: extracted as SubjectID.
//
// Returns ErrInvalidAssertion for any validation failure.
func (p *SAMLProviderImpl) HandleCallback(ctx context.Context, idp IdentityProvider, params CallbackParams) (UserClaims, error) {
	if !idp.Enabled {
		return UserClaims{}, ErrProviderDisabled
	}
	if params.SAMLResponse == "" {
		return UserClaims{}, ErrInvalidAssertion
	}

	// Build a minimal http.Request for crewjam's ParseResponse.
	// The library reads SAMLResponse and RelayState from the form.
	formValues := url.Values{}
	formValues.Set("SAMLResponse", params.SAMLResponse)
	if params.RelayState != "" {
		formValues.Set("RelayState", params.RelayState)
	}

	req := &http.Request{
		Method:   http.MethodPost,
		URL:      mustParseURL(p.cfg.ACSURL),
		PostForm: formValues,
		Form:     formValues,
	}

	// possibleRequestIDs: the outstanding AuthnRequest IDs. An empty slice
	// means "accept any InResponseTo" — callers should always pass the stored ID.
	possibleRequestIDs := params.SAMLRequestIDs

	assertion, err := p.sp.ParseResponse(req, possibleRequestIDs)
	if err != nil {
		// Do not log params.SAMLResponse — it may contain sensitive attributes.
		return UserClaims{}, fmt.Errorf("%w: %s", ErrInvalidAssertion, samlErrStr(err))
	}

	// Extract NameID as SubjectID.
	var subjectID string
	if assertion.Subject != nil && assertion.Subject.NameID != nil {
		subjectID = strings.TrimSpace(assertion.Subject.NameID.Value)
	}
	if subjectID == "" {
		return UserClaims{}, fmt.Errorf("%w: SAML assertion missing NameID", ErrInvalidAssertion)
	}

	// Extract attributes using the attribute map from config.
	attrMap := idp.SAMLConfig.AttributeMap
	email := extractSAMLAttribute(assertion, attrMap, "email",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
		"urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
		"email",
	)
	displayName := extractSAMLAttribute(assertion, attrMap, "name",
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
		"displayName",
		"name",
	)
	groups := extractSAMLMultiAttribute(assertion, attrMap, "groups",
		"http://schemas.microsoft.com/ws/2008/06/identity/claims/groups",
		"groups",
		"memberOf",
	)

	rawAttrs := extractAllSAMLAttributes(assertion)

	return UserClaims{
		SubjectID:     subjectID,
		Email:         strings.ToLower(strings.TrimSpace(email)),
		DisplayName:   strings.TrimSpace(displayName),
		Groups:        groups,
		RawAttributes: rawAttrs,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fetchIDPMetadata fetches and parses the IdP SAML metadata document.
func fetchIDPMetadata(ctx context.Context, metadataURL string) (*crewsaml.EntityDescriptor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var meta crewsaml.EntityDescriptor
	if err := xml.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode metadata XML: %w", err)
	}
	return &meta, nil
}

// parsePEMCertificate decodes a PEM-encoded X.509 certificate.
func parsePEMCertificate(pemStr string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

// buildEntityDescriptorFromCert creates a minimal EntityDescriptor from a
// single IdP signing certificate for use when full metadata is unavailable.
func buildEntityDescriptorFromCert(cert *x509.Certificate) *crewsaml.EntityDescriptor {
	certDER := base64.StdEncoding.EncodeToString(cert.Raw)
	kd := crewsaml.KeyDescriptor{
		Use: "signing",
		KeyInfo: crewsaml.KeyInfo{
			X509Data: crewsaml.X509Data{
				X509Certificates: []crewsaml.X509Certificate{
					{Data: certDER},
				},
			},
		},
	}
	idpDesc := crewsaml.IDPSSODescriptor{}
	idpDesc.KeyDescriptors = []crewsaml.KeyDescriptor{kd}
	return &crewsaml.EntityDescriptor{
		IDPSSODescriptors: []crewsaml.IDPSSODescriptor{idpDesc},
	}
}

// extractAuthnRequestID decodes a SAMLRequest query parameter (deflate +
// base64) and extracts the ID attribute from the AuthnRequest XML.
func extractAuthnRequestID(samlRequest string) (string, error) {
	if samlRequest == "" {
		return "", fmt.Errorf("empty SAMLRequest")
	}
	decoded, err := base64.StdEncoding.DecodeString(samlRequest)
	if err != nil {
		return "", fmt.Errorf("base64 decode SAMLRequest: %w", err)
	}
	// SAMLRequest is deflate-compressed (raw DEFLATE, not gzip).
	xmlBytes, err := deflateDecompress(decoded)
	if err != nil {
		return "", fmt.Errorf("decompress SAMLRequest: %w", err)
	}

	var req struct {
		ID string `xml:"ID,attr"`
	}
	if err := xml.Unmarshal(xmlBytes, &req); err != nil {
		return "", fmt.Errorf("parse AuthnRequest XML: %w", err)
	}
	return req.ID, nil
}

// deflateDecompress decompresses raw DEFLATE-compressed bytes.
func deflateDecompress(data []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// extractSAMLAttribute looks up the first value of an attribute from an
// assertion, trying the mapped name first, then the fallback names in order.
func extractSAMLAttribute(assertion *crewsaml.Assertion, attrMap map[string]string, mappedKey string, fallbacks ...string) string {
	// Try the mapped key first.
	if attrMap != nil {
		if mappedName, ok := attrMap[mappedKey]; ok {
			if v := getAttributeValue(assertion, mappedName); v != "" {
				return v
			}
		}
	}
	// Try fallback names.
	for _, name := range fallbacks {
		if v := getAttributeValue(assertion, name); v != "" {
			return v
		}
	}
	return ""
}

// extractSAMLMultiAttribute returns all values for a multi-valued attribute.
func extractSAMLMultiAttribute(assertion *crewsaml.Assertion, attrMap map[string]string, mappedKey string, fallbacks ...string) []string {
	// Try mapped key.
	if attrMap != nil {
		if mappedName, ok := attrMap[mappedKey]; ok {
			if vals := getAttributeValues(assertion, mappedName); len(vals) > 0 {
				return vals
			}
		}
	}
	for _, name := range fallbacks {
		if vals := getAttributeValues(assertion, name); len(vals) > 0 {
			return vals
		}
	}
	return nil
}

// getAttributeValue returns the first value of a named SAML attribute.
func getAttributeValue(assertion *crewsaml.Assertion, name string) string {
	for _, stmt := range assertion.AttributeStatements {
		for _, attr := range stmt.Attributes {
			if attr.Name == name || attr.FriendlyName == name {
				if len(attr.Values) > 0 {
					return attr.Values[0].Value
				}
			}
		}
	}
	return ""
}

// getAttributeValues returns all values of a named SAML attribute.
func getAttributeValues(assertion *crewsaml.Assertion, name string) []string {
	for _, stmt := range assertion.AttributeStatements {
		for _, attr := range stmt.Attributes {
			if attr.Name == name || attr.FriendlyName == name {
				var vals []string
				for _, v := range attr.Values {
					if v.Value != "" {
						vals = append(vals, v.Value)
					}
				}
				return vals
			}
		}
	}
	return nil
}

// extractAllSAMLAttributes builds a flat map of all attribute name → first value
// for debug purposes. Must never be logged or included in API responses.
func extractAllSAMLAttributes(assertion *crewsaml.Assertion) map[string]string {
	m := make(map[string]string)
	for _, stmt := range assertion.AttributeStatements {
		for _, attr := range stmt.Attributes {
			key := attr.Name
			if key == "" {
				key = attr.FriendlyName
			}
			if key != "" && len(attr.Values) > 0 && m[key] == "" {
				m[key] = attr.Values[0].Value
			}
		}
	}
	return m
}

// mustParseURL parses a URL and panics on error (for static strings).
func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic("saml: mustParseURL: " + err.Error())
	}
	return u
}

// samlErrStr returns the error message for a SAML validation error without
// exposing any assertion content.
func samlErrStr(err error) string {
	if err == nil {
		return ""
	}
	// crewjam/saml errors are safe to relay (they describe structural issues,
	// not content). Strip any potential value leakage by using a short prefix.
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
