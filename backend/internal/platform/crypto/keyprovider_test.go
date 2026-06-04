package crypto_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-org/hr-saas/internal/platform/crypto"
)

// ---------------------------------------------------------------------------
// EnvKeyProvider
// ---------------------------------------------------------------------------

func TestEnvKeyProvider_DataKey_FromEnv(t *testing.T) {
	// Arrange: set the env var to a valid synthetic base64 key.
	syntheticB64 := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVoxMjM0NTY=" // 32 bytes synthetic
	t.Setenv("FIELD_ENCRYPTION_KEY", syntheticB64)

	p := crypto.NewEnvKeyProvider("FIELD_ENCRYPTION_KEY", false)
	key, err := p.DataKey(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 32, len(key), "DataKey must return exactly 32 bytes")
}

func TestEnvKeyProvider_DataKey_MissingEnvNoEphemeral(t *testing.T) {
	t.Setenv("FIELD_ENCRYPTION_KEY", "")

	p := crypto.NewEnvKeyProvider("FIELD_ENCRYPTION_KEY", false)
	_, err := p.DataKey(context.Background())
	assert.Error(t, err, "missing env var with allowEphemeral=false must return error")
}

func TestEnvKeyProvider_DataKey_MissingEnvWithEphemeral(t *testing.T) {
	t.Setenv("FIELD_ENCRYPTION_KEY", "")

	p := crypto.NewEnvKeyProvider("FIELD_ENCRYPTION_KEY", true)
	key, err := p.DataKey(context.Background())
	require.NoError(t, err, "missing env var with allowEphemeral=true must return ephemeral key")
	assert.Equal(t, 32, len(key), "ephemeral key must be 32 bytes")
}

func TestEnvKeyProvider_DataKey_InvalidBase64(t *testing.T) {
	t.Setenv("FIELD_ENCRYPTION_KEY", "not-valid-base64!!!")

	p := crypto.NewEnvKeyProvider("FIELD_ENCRYPTION_KEY", false)
	_, err := p.DataKey(context.Background())
	assert.Error(t, err, "invalid base64 must return error")
}

func TestEnvKeyProvider_DataKey_WrongKeyLength(t *testing.T) {
	// 16 bytes (too short) encoded as base64.
	import_b64 := "QUJDREVGR0hJSktMTU5PUDE=" // 16 bytes synthetic — wrong length
	t.Setenv("FIELD_ENCRYPTION_KEY", import_b64)

	p := crypto.NewEnvKeyProvider("FIELD_ENCRYPTION_KEY", false)
	_, err := p.DataKey(context.Background())
	assert.Error(t, err, "16-byte key must be rejected")
}

func TestEnvKeyProvider_DefaultEnvVarName(t *testing.T) {
	// Passing empty envVar should default to "FIELD_ENCRYPTION_KEY".
	syntheticB64 := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVoxMjM0NTY="
	t.Setenv("FIELD_ENCRYPTION_KEY", syntheticB64)

	p := crypto.NewEnvKeyProvider("", false) // empty → defaults to FIELD_ENCRYPTION_KEY
	key, err := p.DataKey(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 32, len(key))
}

// ---------------------------------------------------------------------------
// NewFieldCipherFromProvider
// ---------------------------------------------------------------------------

func TestNewFieldCipherFromProvider_RoundTrip(t *testing.T) {
	// Use a fixed synthetic key via a stub provider.
	fixedKey := bytes.Repeat([]byte{0x55}, 32) // synthetic 32-byte key
	provider := &stubKeyProvider{key: fixedKey}

	fc, err := crypto.NewFieldCipherFromProvider(context.Background(), provider)
	require.NoError(t, err)

	plaintext := []byte("合成テストデータ — KeyProvider roundtrip")
	ct, err := fc.Encrypt(plaintext)
	require.NoError(t, err)

	got, err := fc.Decrypt(ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

func TestNewFieldCipherFromProvider_ProviderError(t *testing.T) {
	provider := &errKeyProvider{}

	_, err := crypto.NewFieldCipherFromProvider(context.Background(), provider)
	assert.Error(t, err, "provider error must propagate")
}

// ---------------------------------------------------------------------------
// Stub implementations of KeyProvider for tests
// ---------------------------------------------------------------------------

// stubKeyProvider always returns a fixed key.
type stubKeyProvider struct {
	key []byte
}

func (s *stubKeyProvider) DataKey(_ context.Context) ([]byte, error) {
	// Return a copy so that zeroBytes in the production code does not affect tests.
	out := make([]byte, len(s.key))
	copy(out, s.key)
	return out, nil
}

// errKeyProvider always returns an error.
type errKeyProvider struct{}

func (e *errKeyProvider) DataKey(_ context.Context) ([]byte, error) {
	return nil, errProviderFailure
}

var errProviderFailure = assert.AnError
