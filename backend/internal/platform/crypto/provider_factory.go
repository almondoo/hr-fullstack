// Package crypto — factory for selecting the appropriate KeyProvider at startup.
package crypto

import (
	"context"
	"fmt"
)

// NewKeyProviderFromConfig constructs the [KeyProvider] selected by keyProviderName.
//
// Accepted values for keyProviderName:
//
//   - "env" (default) — [EnvKeyProvider] reads FIELD_ENCRYPTION_KEY.
//     allowEphemeral should be true only in development (enables ephemeral
//     fallback when the env var is absent).
//
//   - "aws-kms" — [KMSKeyProvider] calls AWS KMS GenerateDataKey.
//     kmsKeyID must be a non-empty CMK ARN or alias; awsRegion is optional
//     (empty string uses the standard AWS SDK credential / region chain).
//
// Any other value returns an error so that typos in KEY_PROVIDER cause a
// fast startup failure rather than a silent misconfiguration.
//
// This function is the single wiring point for key-provider selection.
// Callers (typically the composition root in internal/server/server.go or
// cmd/server/main.go) pass the config values through and receive a
// [KeyProvider] that they pass to [NewFieldCipherFromProvider].
func NewKeyProviderFromConfig(
	ctx context.Context,
	keyProviderName string,
	allowEphemeral bool,
	kmsKeyID string,
	awsRegion string,
) (KeyProvider, error) {
	switch keyProviderName {
	case "", "env":
		return NewEnvKeyProvider("", allowEphemeral), nil
	case "aws-kms":
		return NewKMSKeyProvider(ctx, kmsKeyID, awsRegion)
	default:
		return nil, fmt.Errorf(
			"crypto: unknown KEY_PROVIDER %q; accepted values: env, aws-kms",
			keyProviderName,
		)
	}
}
