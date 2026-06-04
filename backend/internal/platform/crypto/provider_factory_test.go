package crypto_test

// provider_factory_test.go — tests for NewKeyProviderFromConfig (provider selection).
//
// These tests verify that the factory correctly selects the key provider based
// on the keyProviderName argument.  No real KMS is available in CI, so the
// "aws-kms" path is tested for error behaviour when the key ID is absent.
// All key material used here is synthetic (no real keys or PII).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-org/hr-saas/internal/platform/crypto"
)

// TestNewKeyProviderFromConfig_DefaultsToEnv verifies that an empty provider
// name selects the EnvKeyProvider.  We verify this by checking that DataKey
// succeeds when FIELD_ENCRYPTION_KEY is set to a valid synthetic value.
func TestNewKeyProviderFromConfig_DefaultsToEnv(t *testing.T) {
	// Synthetic 32-byte key encoded in base64 — clearly not a real secret.
	const syntheticB64 = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVoxMjM0NTY="
	t.Setenv("FIELD_ENCRYPTION_KEY", syntheticB64)

	p, err := crypto.NewKeyProviderFromConfig(context.Background(), "", false, "", "")
	require.NoError(t, err, "empty keyProviderName must select EnvKeyProvider without error")

	key, err := p.DataKey(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 32, len(key), "DataKey must return exactly 32 bytes")
}

// TestNewKeyProviderFromConfig_ExplicitEnv verifies that "env" explicitly
// selects the EnvKeyProvider (same as empty string).
func TestNewKeyProviderFromConfig_ExplicitEnv(t *testing.T) {
	const syntheticB64 = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVoxMjM0NTY="
	t.Setenv("FIELD_ENCRYPTION_KEY", syntheticB64)

	p, err := crypto.NewKeyProviderFromConfig(context.Background(), "env", false, "", "")
	require.NoError(t, err, "\"env\" provider name must select EnvKeyProvider")

	key, err := p.DataKey(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 32, len(key))
}

// TestNewKeyProviderFromConfig_UnknownProvider verifies that an unrecognised
// provider name returns an error at factory time (fast startup failure).
func TestNewKeyProviderFromConfig_UnknownProvider(t *testing.T) {
	_, err := crypto.NewKeyProviderFromConfig(context.Background(), "hashicorp-vault", false, "", "")
	assert.Error(t, err, "unknown provider name must return an error")
	assert.Contains(t, err.Error(), "hashicorp-vault",
		"error message should contain the unrecognised name for debuggability")
}

// TestNewKeyProviderFromConfig_AWSKMS_EmptyKeyID verifies that requesting the
// aws-kms provider with an empty key ID returns an error (fast fail at startup).
// No real KMS is called in this test — the error occurs during construction.
func TestNewKeyProviderFromConfig_AWSKMS_EmptyKeyID(t *testing.T) {
	// NewKMSKeyProvider should reject an empty kmsKeyID.
	_, err := crypto.NewKeyProviderFromConfig(context.Background(), "aws-kms", false, "", "")
	// We expect an error: either from KMS construction (empty key ID) or an SDK
	// initialisation error in a no-credentials environment.
	// Either way, a non-nil error is the correct startup-fast-fail behaviour.
	assert.Error(t, err, "aws-kms with empty key ID must return an error")
}
