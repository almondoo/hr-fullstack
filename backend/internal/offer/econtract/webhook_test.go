package econtract_test

// webhook_test.go — tests for WebhookHandler security paths (Issue #14 / security fix).
//
// These tests exercise the security-critical paths of HandleWebhook without
// requiring a database connection (no testdb.NewHarness call):
//   - Fail-closed: missing WebhookSigningKey → 503
//   - Missing timestamp header → 400
//   - Non-integer timestamp → 400
//   - Stale/future timestamp (outside 5-min window) → 400
//   - Bad HMAC signature → 403
//   - Valid HMAC signature → 200 (malformed JSON body returns 200 to prevent
//     provider retry storms per the handler contract)
//   - RegisterWebhookRoutes guard: non-stub provider without signing key → error
//   - RegisterWebhookRoutes guard: stub provider without signing key → no error
//
// All test data is synthetic (no PII, no real secrets).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-org/hr-saas/internal/offer/econtract"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// syntheticSigningKey is a fixed 32-byte test key — clearly synthetic, not a real secret.
const syntheticSigningKey = "synthetic-test-signing-key-00001"

// makeHMAC produces the hex-encoded HMAC-SHA256 of "<timestamp>.<body>" using
// the given key, mirroring the production verifyHMACSHA256 logic.
func makeHMAC(timestamp string, body []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// newWebhookRouter returns a *gin.Engine wired with the webhook handler using
// the given config and adapter. tdb is nil — these tests never reach the DB.
func newWebhookRouter(cfg econtract.Config, adapter econtract.Adapter) *gin.Engine {
	r := gin.New()
	h := econtract.NewWebhookHandler(nil, cfg, adapter)
	r.POST("/webhooks/econtract", h.HandleWebhook)
	return r
}

// validBody is a minimal normalised webhook event JSON.
// tenant_id and offer_letter_id are synthetic UUIDs (no real PII / data).
// These UUIDs are well-formed but do not exist in any database — the tests that
// pass a valid HMAC only reach the DB layer when processEvent is called.
// Since tdb is nil, any test that progresses past HMAC verification will panic
// inside WithinTenant unless we stop before that point.
// For this reason the "valid signature" test uses a malformed JSON body so that
// the handler returns 200 before calling processEvent.
const validBodyJSON = `{"envelope_id":"TEST-001","status":"completed","offer_letter_id":"00000000-0000-0000-0000-000000000001","tenant_id":"00000000-0000-0000-0000-000000000002"}`

// TestHandleWebhook_FailClosed verifies that the handler returns 503 when
// WebhookSigningKey is not configured (fail-closed security property).
func TestHandleWebhook_FailClosed(t *testing.T) {
	cfg := econtract.Config{WebhookSigningKey: ""} // key intentionally missing
	r := newWebhookRouter(cfg, econtract.NewStubAdapter())

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/econtract",
		bytes.NewBufferString(validBodyJSON))
	require.NoError(t, err)

	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"missing signing key must return 503 (fail-closed)")
}

// TestHandleWebhook_MissingTimestampHeader verifies that the handler returns
// 400 when the X-EContract-Timestamp header is absent.
func TestHandleWebhook_MissingTimestampHeader(t *testing.T) {
	cfg := econtract.Config{WebhookSigningKey: syntheticSigningKey}
	r := newWebhookRouter(cfg, econtract.NewStubAdapter())

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/econtract",
		bytes.NewBufferString(validBodyJSON))
	require.NoError(t, err)
	// X-EContract-Timestamp intentionally omitted

	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code,
		"missing timestamp header must return 400")
}

// TestHandleWebhook_NonIntegerTimestamp verifies that a non-integer timestamp
// header value returns 400.
func TestHandleWebhook_NonIntegerTimestamp(t *testing.T) {
	cfg := econtract.Config{WebhookSigningKey: syntheticSigningKey}
	r := newWebhookRouter(cfg, econtract.NewStubAdapter())

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/econtract",
		bytes.NewBufferString(validBodyJSON))
	require.NoError(t, err)
	req.Header.Set("X-EContract-Timestamp", "not-a-number")

	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code,
		"non-integer timestamp must return 400")
}

// TestHandleWebhook_StaleTimestamp verifies that a timestamp more than 5 minutes
// in the past is rejected with 400 (replay protection).
func TestHandleWebhook_StaleTimestamp(t *testing.T) {
	cfg := econtract.Config{WebhookSigningKey: syntheticSigningKey}
	r := newWebhookRouter(cfg, econtract.NewStubAdapter())

	staleTs := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	body := []byte(validBodyJSON)
	sig := makeHMAC(staleTs, body, syntheticSigningKey)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/econtract",
		bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-EContract-Timestamp", staleTs)
	req.Header.Set("X-EContract-Signature", sig)

	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code,
		"stale timestamp (>5 min ago) must return 400 (replay protection)")
}

// TestHandleWebhook_FutureTimestamp verifies that a timestamp more than 5 minutes
// in the future is rejected (clock-skew / replay protection).
func TestHandleWebhook_FutureTimestamp(t *testing.T) {
	cfg := econtract.Config{WebhookSigningKey: syntheticSigningKey}
	r := newWebhookRouter(cfg, econtract.NewStubAdapter())

	futureTs := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	body := []byte(validBodyJSON)
	sig := makeHMAC(futureTs, body, syntheticSigningKey)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/econtract",
		bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-EContract-Timestamp", futureTs)
	req.Header.Set("X-EContract-Signature", sig)

	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code,
		"far-future timestamp must return 400 (replay protection)")
}

// TestHandleWebhook_InvalidSignature verifies that a valid timestamp but wrong
// HMAC signature returns 403 (not 200).
func TestHandleWebhook_InvalidSignature(t *testing.T) {
	cfg := econtract.Config{WebhookSigningKey: syntheticSigningKey}
	r := newWebhookRouter(cfg, econtract.NewStubAdapter())

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(validBodyJSON)
	// Use a different key to produce a wrong signature.
	wrongSig := makeHMAC(ts, body, "wrong-key-00000000000000000000001")

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/econtract",
		bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-EContract-Timestamp", ts)
	req.Header.Set("X-EContract-Signature", wrongSig)

	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"invalid HMAC signature must return 403")
}

// TestHandleWebhook_MissingSignatureHeader verifies that an absent signature
// header returns 403 (verifyHMACSHA256 returns false for empty sig).
func TestHandleWebhook_MissingSignatureHeader(t *testing.T) {
	cfg := econtract.Config{WebhookSigningKey: syntheticSigningKey}
	r := newWebhookRouter(cfg, econtract.NewStubAdapter())

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(validBodyJSON)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/econtract",
		bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-EContract-Timestamp", ts)
	// X-EContract-Signature intentionally absent

	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"missing signature header must return 403")
}

// TestHandleWebhook_ValidSignature_MalformedJSON verifies that a correctly
// signed request with invalid JSON body returns 200 (to prevent provider
// retry storms on permanently malformed payloads).
func TestHandleWebhook_ValidSignature_MalformedJSON(t *testing.T) {
	cfg := econtract.Config{WebhookSigningKey: syntheticSigningKey}
	r := newWebhookRouter(cfg, econtract.NewStubAdapter())

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{invalid-json}`)
	sig := makeHMAC(ts, body, syntheticSigningKey)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/econtract",
		bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-EContract-Timestamp", ts)
	req.Header.Set("X-EContract-Signature", sig)

	r.ServeHTTP(w, req)

	// The handler returns 200 for unparseable bodies to prevent provider retries.
	assert.Equal(t, http.StatusOK, w.Code,
		"valid HMAC but malformed JSON must return 200 (no-retry contract)")
}

// ---------------------------------------------------------------------------
// RegisterWebhookRoutes guard tests
// ---------------------------------------------------------------------------

// fakeNonStubAdapter is a minimal Adapter implementation that reports
// IsStubProvider=false, simulating a "real" production provider without
// actual credentials.
type fakeNonStubAdapter struct{}

func (fakeNonStubAdapter) SendSignRequest(_ context.Context, _ econtract.SendSignRequest) (econtract.SignStatus, error) {
	return econtract.SignStatus{}, fmt.Errorf("not implemented")
}
func (fakeNonStubAdapter) GetSignStatus(_ context.Context, _ string) (econtract.SignStatus, error) {
	return econtract.SignStatus{}, fmt.Errorf("not implemented")
}
func (fakeNonStubAdapter) ProviderLabel() string { return "fake" }
func (fakeNonStubAdapter) IsStubProvider() bool  { return false }

// TestRegisterWebhookRoutes_StubProvider_NoKeyRequired verifies that a stub
// adapter can register routes even without a WebhookSigningKey.
func TestRegisterWebhookRoutes_StubProvider_NoKeyRequired(t *testing.T) {
	r := gin.New()
	g := r.Group("/webhooks")
	cfg := econtract.Config{WebhookSigningKey: ""}

	err := econtract.RegisterWebhookRoutes(g, nil, cfg, econtract.NewStubAdapter())
	assert.NoError(t, err,
		"stub provider must register routes without a signing key")
}

// TestRegisterWebhookRoutes_RealProvider_RequiresKey verifies that a non-stub
// adapter cannot register routes without a WebhookSigningKey (fail-safe guard).
func TestRegisterWebhookRoutes_RealProvider_RequiresKey(t *testing.T) {
	r := gin.New()
	g := r.Group("/webhooks")
	cfg := econtract.Config{WebhookSigningKey: ""}

	err := econtract.RegisterWebhookRoutes(g, nil, cfg, fakeNonStubAdapter{})
	assert.Error(t, err,
		"non-stub provider without signing key must return error")
}

// TestRegisterWebhookRoutes_RealProvider_WithKey verifies that a non-stub
// adapter registers routes successfully when a signing key is configured.
func TestRegisterWebhookRoutes_RealProvider_WithKey(t *testing.T) {
	r := gin.New()
	g := r.Group("/webhooks")
	cfg := econtract.Config{WebhookSigningKey: syntheticSigningKey}

	err := econtract.RegisterWebhookRoutes(g, nil, cfg, fakeNonStubAdapter{})
	assert.NoError(t, err,
		"non-stub provider with signing key must register routes without error")
}
