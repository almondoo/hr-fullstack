package notification

// ---------------------------------------------------------------------------
// LINE WORKS OAuth2 Client Credentials (Service Account JWT) token fetcher.
//
// LINE WORKS API 2.0 uses OAuth2 Client Credentials with a Service Account
// JWT assertion.  This implementation uses only stdlib (crypto/rsa, encoding/*)
// and net/http — no new go.mod dependency is added.
//
// Flow (https://developers.worksmobile.com/jp/reference/authorization-sa):
//   1. Build a JWT signed with the Service Account private key (RS256).
//   2. POST to https://auth.worksmobile.com/oauth2/v2.0/token
//      with grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer.
//   3. Cache the received access_token until `expires_in` seconds - 60s.
//
// Security posture:
//   - The private key MUST be injected via LineWorksServiceAccountConfig and
//     loaded from an environment variable or Secret Manager at startup.
//     It is NEVER logged, committed, or persisted.
//   - The access token is treated as a secret; it is not logged.
//   - Cached credentials are held in-process only and are cleared on restart.
//
// External dependencies remaining before production use:
//   - LineWorksServiceAccountConfig must be populated from env / Secret Manager.
//     See .env.example for placeholder names.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const lineWorksTokenURL = "https://auth.worksmobile.com/oauth2/v2.0/token"

// LineWorksServiceAccountConfig holds Service Account credentials for the
// LINE WORKS OAuth2 Client Credentials flow.
//
// All fields must be populated from environment variables or a Secret Manager.
// See .env.example for placeholder variable names.
//
// SECURITY: PrivateKeyPEM is an RSA private key — treat as a secret; never
// log, commit, or persist.
type LineWorksServiceAccountConfig struct {
	// ClientID is the OAuth2 Client ID from the LINE WORKS Developer Console.
	// Source from: NOTIFICATION_LINE_WORKS_CLIENT_ID
	ClientID string
	// ServiceAccountID is the Service Account ID (e.g. "serviceAccount@...").
	// Source from: NOTIFICATION_LINE_WORKS_SERVICE_ACCOUNT_ID
	ServiceAccountID string
	// PrivateKeyPEM is the PEM-encoded RSA private key for JWT signing.
	// SECURITY: never log or commit this value.
	// Source from: NOTIFICATION_LINE_WORKS_PRIVATE_KEY (newlines may be \\n-encoded)
	PrivateKeyPEM string
	// Scope is the space-separated OAuth2 scope.
	// Defaults to "bot" when empty.
	Scope string
}

// lineWorksTokenCache caches a fetched access token for a single SA config.
type lineWorksTokenCache struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// get returns the cached token if it is still valid (> 60 s remaining).
// Returns empty string when no valid cached token exists.
func (c *lineWorksTokenCache) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expiresAt) > 60*time.Second {
		return c.token
	}
	return ""
}

// set stores a token with the given TTL.
func (c *lineWorksTokenCache) set(token string, expiresIn int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	c.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
}

// LineWorksTokenProvider fetches and caches LINE WORKS API 2.0 access tokens
// using the OAuth2 Service Account JWT assertion flow.
//
// Production wiring: inject this into LineWorksSender so it can obtain fresh
// tokens without a pre-issued ChannelToken.
type LineWorksTokenProvider struct {
	cfg   LineWorksServiceAccountConfig
	cache lineWorksTokenCache
	// httpClient is overridable for testing; defaults to package-level httpClient.
	httpClient *http.Client
}

// NewLineWorksTokenProvider constructs a LineWorksTokenProvider.
// All cfg fields must be non-empty.
func NewLineWorksTokenProvider(cfg LineWorksServiceAccountConfig) (*LineWorksTokenProvider, error) {
	if cfg.ClientID == "" || cfg.ServiceAccountID == "" || cfg.PrivateKeyPEM == "" {
		return nil, fmt.Errorf(
			"notification: LineWorksTokenProvider: ClientID, ServiceAccountID, and PrivateKeyPEM are all required",
		)
	}
	if cfg.Scope == "" {
		cfg.Scope = "bot"
	}
	return &LineWorksTokenProvider{cfg: cfg, httpClient: httpClient}, nil
}

// Token returns a valid access token, fetching a new one from the token endpoint
// when the cached token has expired.
//
// SECURITY: the returned token is a secret; callers must not log it.
func (p *LineWorksTokenProvider) Token(ctx context.Context) (string, error) {
	if t := p.cache.get(); t != "" {
		return t, nil
	}
	return p.fetch(ctx)
}

// fetch performs the JWT assertion → token exchange.
func (p *LineWorksTokenProvider) fetch(ctx context.Context) (string, error) {
	privateKey, err := parseRSAPrivateKey(p.cfg.PrivateKeyPEM)
	if err != nil {
		return "", fmt.Errorf("notification: line_works token: parse private key: %w", err)
	}

	now := time.Now().UTC()
	jwtStr, err := buildRS256JWT(map[string]any{
		"iss": p.cfg.ClientID,
		"sub": p.cfg.ServiceAccountID,
		"iat": now.Unix(),
		"exp": now.Add(30 * time.Minute).Unix(),
	}, privateKey)
	if err != nil {
		return "", fmt.Errorf("notification: line_works token: build JWT: %w", err)
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {jwtStr},
		"client_id":  {p.cfg.ClientID},
		"scope":      {p.cfg.Scope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lineWorksTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("notification: line_works token: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("notification: line_works token: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("notification: line_works token: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Do NOT include the body in the error (may contain secret context).
		return "", fmt.Errorf("notification: line_works token: unexpected status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("notification: line_works token: parse response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("notification: line_works token: empty access_token in response")
	}

	ttl := tokenResp.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	p.cache.set(tokenResp.AccessToken, ttl)
	return tokenResp.AccessToken, nil
}

// ---------------------------------------------------------------------------
// Minimal RS256 JWT builder (stdlib only; no jwt library dependency)
// ---------------------------------------------------------------------------

// buildRS256JWT builds a compact signed JWT (RS256) from the given claims.
// Only the fields required for the LINE WORKS Service Account assertion are
// included (iss, sub, iat, exp).
func buildRS256JWT(claims map[string]any, key *rsa.PrivateKey) (string, error) {
	header := base64URLEncode(mustJSON(map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	}))
	payload := base64URLEncode(mustJSON(claims))
	signingInput := header + "." + payload

	hash := sha256.New()
	hash.Write([]byte(signingInput))
	digest := hash.Sum(nil)

	sig, err := rsa.SignPKCS1v15(rand.Reader, key, 0x04 /* crypto.SHA256 */, digest)
	if err != nil {
		return "", fmt.Errorf("jwt: sign: %w", err)
	}
	return signingInput + "." + base64URLEncode(sig), nil
}

func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// parseRSAPrivateKey parses a PEM-encoded PKCS#8 or PKCS#1 RSA private key.
// LINE WORKS Developer Console issues PKCS#8 keys.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	// Support \\n-escaped newlines from env var injection.
	pemStr = strings.ReplaceAll(pemStr, `\n`, "\n")
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS8: %w", err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is not RSA")
		}
		return rsaKey, nil
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block type: %s", block.Type)
	}
}
