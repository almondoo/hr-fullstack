// Package crypto — KeyProvider abstraction for field-level encryption keys.
//
// # Why a KeyProvider interface?
//
// The current [FieldCipher] loads its 32-byte key from the FIELD_ENCRYPTION_KEY
// environment variable.  For production hardening, the key must come from a
// Hardware Security Module or a managed KMS service so that:
//
//   - The raw key material never touches application memory longer than necessary.
//   - Key rotation is performed without redeploying the application.
//   - Access to the key is audited independently of the application logs.
//
// This file defines [KeyProvider] — a single-method interface that decouples
// *how a key is obtained* from *how ciphertext is produced*.  The rest of the
// crypto package (and callers) program against [KeyProvider]; only the
// bootstrap code selects a concrete implementation.
//
// # Provided implementations
//
//   - [EnvKeyProvider] — reads FIELD_ENCRYPTION_KEY (or a named env var) at
//     construction time.  This is the default implementation used today and in
//     local / CI environments.
//
// # Plugging in a real KMS (TODO)
//
// To add AWS KMS, GCP Cloud KMS, Azure Key Vault, or HashiCorp Vault, create
// a new file (e.g. crypto/aws_kms.go) that satisfies [KeyProvider]:
//
//	type AWSKMSKeyProvider struct { /* KMS client, key ARN, etc. */ }
//
//	func (p *AWSKMSKeyProvider) DataKey(ctx context.Context) ([]byte, error) {
//	    // Call kms.GenerateDataKeyWithoutPlaintext / Decrypt as appropriate.
//	    // Return the plaintext DEK; zero it in the caller after NewFieldCipher.
//	}
//
// Then pass it to [NewFieldCipherFromProvider] instead of the env provider.
// The rest of the codebase remains unchanged.
//
// # Security invariants (must be preserved by every implementation)
//
//   - The returned key slice MUST be exactly 32 bytes (AES-256).
//   - The key value MUST NOT be written to any log, trace, or audit record.
//   - The caller MUST zero the returned slice after constructing [FieldCipher].
//   - In production, [KeyProvider] implementations SHOULD use envelope
//     encryption: the KMS holds only the Key Encryption Key (KEK); the DEK is
//     decrypted at startup and kept in process memory.
package crypto

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
)

// KeyProvider is the abstraction between the application's field-encryption
// logic and the key-management backend.
//
// DataKey returns a 32-byte plaintext Data Encryption Key (DEK).  The caller
// is responsible for:
//
//  1. Passing the key to [NewFieldCipher].
//  2. Zeroing the returned slice immediately after [NewFieldCipher] returns.
//
// Implementations MUST be safe for concurrent use.
type KeyProvider interface {
	// DataKey fetches (or derives) the current 32-byte DEK.
	// ctx is forwarded to any underlying RPC calls (KMS, HTTP, gRPC).
	// A non-nil error means the key is temporarily or permanently unavailable;
	// callers should treat this as a fatal startup error in production.
	DataKey(ctx context.Context) ([]byte, error)
}

// ---------------------------------------------------------------------------
// EnvKeyProvider — default implementation (env var or ephemeral fallback)
// ---------------------------------------------------------------------------

// EnvKeyProvider implements [KeyProvider] by reading a base64-encoded 32-byte
// key from an environment variable.
//
// This is the current production-facing implementation.  In environments where
// a secrets manager injects the variable at container startup (e.g.
// AWS Secrets Manager → ECS task definition, GCP Secret Manager → Cloud Run
// secret volume), this satisfies the requirement without requiring a KMS SDK.
//
// Construct via [NewEnvKeyProvider]; the zero value is not usable.
type EnvKeyProvider struct {
	// envVar is the name of the environment variable to read.
	// Default: "FIELD_ENCRYPTION_KEY".
	envVar string

	// allowEphemeral controls whether an ephemeral random key is generated
	// when envVar is absent.  Should be true only in local development.
	allowEphemeral bool
}

// NewEnvKeyProvider returns an [EnvKeyProvider] that reads key material from
// the environment variable named by envVar.
//
// If allowEphemeral is true and the variable is absent, a random ephemeral key
// is generated (development fallback).  Set allowEphemeral=false in production
// to cause a hard failure when the variable is missing.
func NewEnvKeyProvider(envVar string, allowEphemeral bool) *EnvKeyProvider {
	if envVar == "" {
		envVar = envKey // "FIELD_ENCRYPTION_KEY"
	}
	return &EnvKeyProvider{envVar: envVar, allowEphemeral: allowEphemeral}
}

// DataKey implements [KeyProvider].  It reads the env var and decodes the
// base64 payload.  The returned slice is exactly 32 bytes; callers must zero
// it after use.
func (p *EnvKeyProvider) DataKey(_ context.Context) ([]byte, error) {
	raw := os.Getenv(p.envVar)
	if raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("crypto: %s is not valid base64: %w", p.envVar, err)
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("crypto: %s must decode to %d bytes, got %d", p.envVar, keySize, len(key))
		}
		return key, nil
	}

	if !p.allowEphemeral {
		return nil, fmt.Errorf(
			"crypto: %s must be set; configure your secrets manager or set a base64-encoded 32-byte key",
			p.envVar,
		)
	}

	// Development fallback: generate ephemeral key.  Logged by the caller
	// (loadFromEnv / NewFieldCipherFromProvider) so we do not duplicate here.
	return generateEphemeralKey()
}

// ---------------------------------------------------------------------------
// NewFieldCipherFromProvider — constructor that accepts any KeyProvider
// ---------------------------------------------------------------------------

// NewFieldCipherFromProvider constructs a [FieldCipher] using key material
// obtained from provider.
//
// The DEK is fetched once, used to initialise the AES-256-GCM cipher, and
// then zeroed immediately so it does not linger in the heap.
//
// This is the preferred constructor for production code; pass an
// [EnvKeyProvider] for the current behaviour or a KMS implementation once
// the cloud provider is selected.
//
// TODO(#10): Replace [EnvKeyProvider] with a KMS-backed provider after the
// cloud provider is chosen (GAP-01 resolved).  Candidate implementations:
//
//   - AWS KMS: use kms.GenerateDataKeyWithoutPlaintext + Decrypt (envelope).
//   - GCP Cloud KMS: use CryptoKeyVersion.AsymmetricDecrypt or DEK wrapping.
//   - Azure Key Vault: use keyvault.WrapKey / UnwrapKey with AES-KW.
//   - HashiCorp Vault: use Transit secrets engine (encrypt/decrypt endpoints).
func NewFieldCipherFromProvider(ctx context.Context, provider KeyProvider) (*FieldCipher, error) {
	key, err := provider.DataKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("crypto: key provider: %w", err)
	}
	// Zero the key material immediately after cipher construction.
	defer zeroBytes(key)

	return NewFieldCipher(key)
}

// ---------------------------------------------------------------------------
// zeroBytes — helper to wipe key material from memory
// ---------------------------------------------------------------------------

// zeroBytes overwrites b with zeros to prevent key material from lingering on
// the heap after it is no longer needed.
//
// Note: the Go runtime may have already copied the slice to other heap
// locations during GC compaction; zeroing is best-effort.  A proper
// implementation would use a memory-locked buffer (e.g. via mlock(2)).
// This is a known limitation documented here for future hardening.
//
// TODO(#10): Consider using a mlock-backed buffer library (e.g.
// github.com/awnumar/memguard) for key material in production.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
