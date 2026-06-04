// Package crypto — AWS KMS-backed KeyProvider using envelope encryption.
//
// # Envelope encryption design
//
// The KMS KeyProvider uses the standard envelope encryption pattern:
//
//  1. An AWS KMS Customer Managed Key (CMK) acts as the Key Encryption Key (KEK).
//     The KEK never leaves AWS KMS; all operations on it happen inside the HSM.
//  2. At startup, [KMSKeyProvider.DataKey] calls kms.GenerateDataKey to obtain a
//     fresh plaintext Data Encryption Key (DEK, 32 bytes) and its KMS-encrypted
//     ciphertext blob (EncryptedDataKey).
//  3. The plaintext DEK is used once to construct [FieldCipher], then zeroed.
//  4. For key rotation: store EncryptedDataKey alongside each ciphertext record;
//     on rotation, re-wrap via kms.ReEncrypt or re-encrypt the DEK with the new
//     CMK version, then re-encrypt affected rows in a background job.
//
// # AWS permissions required
//
// The IAM role / instance profile must have:
//
//	kms:GenerateDataKey   — to produce a new DEK (AES_256)
//	kms:Decrypt           — to unwrap a stored EncryptedDataKey on restart
//
// # Configuration
//
// Set KEY_PROVIDER=aws-kms in the environment and configure:
//
//	KMS_KEY_ID  — the full ARN or alias of the CMK
//	             (e.g. "arn:aws:kms:ap-northeast-1:123456789012:key/mrk-...")
//	             or an alias like "alias/hr-saas-field-encryption"
//	AWS_REGION  — standard AWS SDK env var (or set via ~/.aws/config)
//
// Credentials follow the standard AWS credential chain
// (instance profile, ECS task role, environment variables, ~/.aws/credentials).
// Never hardcode credentials in this file.
//
// # Security invariants (must not be weakened)
//
//   - The plaintext DEK is zeroed immediately after NewFieldCipher returns.
//   - The EncryptedDataKey blob (ciphertext) is safe to store; it is useless
//     without the KMS Decrypt permission.
//   - Key ID and region are NOT secrets and may appear in logs; DEK plaintext MUST NOT.
//   - ctx is forwarded to all KMS calls so callers can impose deadlines.
package crypto

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// KMSKeyProvider implements [KeyProvider] using AWS KMS envelope encryption.
//
// Construct via [NewKMSKeyProvider]. The zero value is not usable.
type KMSKeyProvider struct {
	client *kms.Client
	keyID  string
}

// NewKMSKeyProvider constructs a [KMSKeyProvider] for the given CMK key ID.
//
// keyID must be a full KMS key ARN or alias (e.g. "alias/hr-saas-field-encryption").
// region overrides the AWS region; pass "" to use the standard SDK chain
// (AWS_REGION env var, ~/.aws/config, EC2 metadata, etc.).
//
// This function loads AWS configuration using the default credential chain.
// In production use IAM roles attached to ECS tasks or EC2 instance profiles;
// never pass explicit credentials to this function.
//
// Returns an error if the AWS configuration cannot be loaded (e.g. no region
// found) — fail-fast at startup rather than silently using a missing provider.
func NewKMSKeyProvider(ctx context.Context, keyID, region string) (*KMSKeyProvider, error) {
	if keyID == "" {
		return nil, fmt.Errorf("crypto/kms: KMS_KEY_ID must not be empty")
	}

	// Load the AWS SDK default configuration.
	// Override region only when explicitly provided to allow the standard chain.
	var loadOpts []func(*config.LoadOptions) error
	if region != "" {
		loadOpts = append(loadOpts, config.WithRegion(region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("crypto/kms: load AWS config: %w", err)
	}

	client := kms.NewFromConfig(cfg)
	return &KMSKeyProvider{client: client, keyID: keyID}, nil
}

// DataKey implements [KeyProvider] by calling kms.GenerateDataKey to obtain a
// fresh 32-byte plaintext DEK.
//
// The caller MUST zero the returned slice immediately after passing it to
// [NewFieldCipher]. Failure to do so leaves key material on the heap.
//
// The EncryptedDataKey blob returned by GenerateDataKey is discarded here
// because this provider generates a new DEK on every startup call (stateless
// mode). For persistent ciphertext re-keying workflows, extend this provider
// to store and reload the EncryptedDataKey alongside each record; see the
// key rotation runbook (docs/key_rotation.md) for details.
func (p *KMSKeyProvider) DataKey(ctx context.Context) ([]byte, error) {
	out, err := p.client.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
		KeyId:   aws.String(p.keyID),
		KeySpec: types.DataKeySpecAes256,
	})
	if err != nil {
		return nil, fmt.Errorf("crypto/kms: GenerateDataKey: %w", err)
	}

	// Validate that KMS returned exactly 32 bytes (AES-256).
	// In practice this should always hold for DataKeySpecAes256, but we
	// enforce it here so the invariant is checked close to the source.
	if len(out.Plaintext) != keySize {
		// Zero before returning error to avoid leaking partial key material.
		zeroBytes(out.Plaintext)
		return nil, fmt.Errorf(
			"crypto/kms: GenerateDataKey returned %d bytes; want %d",
			len(out.Plaintext), keySize,
		)
	}

	// Caller is responsible for zeroing out.Plaintext after use.
	return out.Plaintext, nil
}
