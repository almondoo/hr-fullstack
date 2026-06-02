// Package crypto provides AES-256-GCM field-level encryption for sensitive PII
// columns (口座番号, マイナンバー等).
//
// # Key management
//
// The encryption key is loaded from the FIELD_ENCRYPTION_KEY environment
// variable.  The value must be a standard base64-encoded 32-byte key.
//
//   - Production: the variable is injected at deploy time from a secrets manager
//     (AWS Secrets Manager, GCP Secret Manager, Vault, …).  The actual key value
//     MUST NOT be committed to the repository.
//   - Development: if the variable is absent, a random ephemeral key is generated
//     at startup with a warning.  Ciphertext encrypted with this key cannot be
//     decrypted after restart, which is acceptable for local development but MUST
//     NOT be used in any shared or persistent environment.
//
// # TODO — KMS integration
//
// The current implementation uses a single symmetric key loaded from the
// environment.  For production hardening, replace LoadKey / NewCipher with a
// call to your cloud KMS (e.g. AWS KMS GenerateDataKey / Decrypt).
// The interface (Encrypt / Decrypt) is intentionally stable so the internals
// can be swapped without changing callers.
//
// # Security invariants
//
//   - Nonce: 12 bytes, randomly generated per encryption — never reuse a nonce
//     with the same key.
//   - Ciphertext layout: nonce (12 bytes) || ciphertext+tag (variable).
//   - The key value is NEVER written to logs or audit records.
//   - Decryption failures (tampered or wrong key) surface as ErrDecryptFailed;
//     the internal error detail is NOT propagated to callers.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

const (
	// keySize is the required AES key length in bytes (AES-256).
	keySize = 32

	// nonceSize is the GCM standard nonce size in bytes.
	nonceSize = 12

	// envKey is the name of the environment variable holding the base64-encoded key.
	envKey = "FIELD_ENCRYPTION_KEY"
)

// ErrDecryptFailed is returned when decryption fails (tampered ciphertext or
// wrong key).  The internal cause is intentionally suppressed to prevent
// oracle attacks.
var ErrDecryptFailed = errors.New("crypto: decryption failed")

// globalCipher is the package-level FieldCipher initialised once by init().
// All package-level Encrypt / Decrypt calls delegate to it.
var (
	globalCipher     *FieldCipher
	globalCipherErr  error // captures the initialisation error from globalCipherOnce.Do
	globalCipherOnce sync.Once
)

// FieldCipher performs AES-256-GCM encryption and decryption for a single key.
// Create one per key via NewFieldCipher; use package-level Encrypt / Decrypt
// for the global key loaded from FIELD_ENCRYPTION_KEY.
type FieldCipher struct {
	gcm cipher.AEAD
}

// NewFieldCipher constructs a FieldCipher from a raw 32-byte key.
// Returns an error when key length != 32.
//
// The key slice is consumed immediately; callers should zero it after this call
// if the key was derived from a secret source.
func NewFieldCipher(key []byte) (*FieldCipher, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: create GCM: %w", err)
	}
	return &FieldCipher{gcm: gcm}, nil
}

// Encrypt encrypts plaintext with AES-256-GCM.
// Returns nonce || ciphertext+tag as a byte slice.
// A fresh random nonce is generated per call.
func (fc *FieldCipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}
	ciphertext := fc.gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt decrypts ciphertext produced by Encrypt.
// ciphertext must be at least nonceSize bytes (nonce || encrypted payload).
// Returns ErrDecryptFailed for any authentication or length failure.
func (fc *FieldCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < nonceSize {
		return nil, ErrDecryptFailed
	}
	nonce := ciphertext[:nonceSize]
	payload := ciphertext[nonceSize:]
	plaintext, err := fc.gcm.Open(nil, nonce, payload, nil)
	if err != nil {
		// Do NOT propagate err — it may contain oracle-useful details.
		return nil, ErrDecryptFailed
	}
	return plaintext, nil
}

// ---------------------------------------------------------------------------
// Package-level helpers (delegate to the global FieldCipher)
// ---------------------------------------------------------------------------

// Encrypt encrypts plaintext using the global field-encryption key.
// The key is loaded from FIELD_ENCRYPTION_KEY on first call.
func Encrypt(plaintext []byte) ([]byte, error) {
	fc, err := globalCipherInstance()
	if err != nil {
		return nil, err
	}
	return fc.Encrypt(plaintext)
}

// Decrypt decrypts ciphertext using the global field-encryption key.
// Returns ErrDecryptFailed when decryption fails.
func Decrypt(ciphertext []byte) ([]byte, error) {
	fc, err := globalCipherInstance()
	if err != nil {
		return nil, err
	}
	return fc.Decrypt(ciphertext)
}

// globalCipherInstance returns the singleton FieldCipher, initialising it on
// first call.  Thread-safe via sync.Once.
//
// Error persistence: the initialisation error is stored in the package-level
// globalCipherErr so that every call after a failed initialisation returns the
// original error rather than the misleading "not initialised" message.
// sync.Once guarantees that loadFromEnv is called exactly once; subsequent
// calls skip the Do body and read globalCipherErr directly.
func globalCipherInstance() (*FieldCipher, error) {
	globalCipherOnce.Do(func() {
		globalCipher, globalCipherErr = loadFromEnv()
	})
	if globalCipherErr != nil {
		return nil, globalCipherErr
	}
	if globalCipher == nil {
		return nil, fmt.Errorf("crypto: global cipher not initialised")
	}
	return globalCipher, nil
}

// loadFromEnv reads FIELD_ENCRYPTION_KEY and constructs a FieldCipher.
//
//   - If the variable is set: it must be a base64-standard-encoded 32-byte key.
//     An incorrect value causes a startup failure (fail-fast).
//   - If the variable is absent in development mode (APP_ENV=development or
//     unset): a random ephemeral key is generated with a warning.
//     Ciphertext encrypted with this key is lost on restart.
//   - If the variable is absent in non-development mode: the application fails
//     to start.  PII must never be stored without a real key in production.
//
// The key value is NEVER written to any log output.
func loadFromEnv() (*FieldCipher, error) {
	raw := os.Getenv(envKey)
	if raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("crypto: %s is not valid base64: %w", envKey, err)
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("crypto: %s must decode to %d bytes, got %d", envKey, keySize, len(key))
		}
		return NewFieldCipher(key)
	}

	appEnv := os.Getenv("APP_ENV")
	if appEnv != "" && appEnv != "development" {
		// Fail fast in any non-development environment — never run production
		// without a real field encryption key.
		return nil, fmt.Errorf(
			"crypto: %s must be set in non-development environments (APP_ENV=%s); "+
				"set a base64-encoded 32-byte key or configure your secrets manager",
			envKey, appEnv,
		)
	}

	// Development fallback: generate a random ephemeral key.
	// NOTE: This key is lost on restart; all previously encrypted values become
	// unreadable.  Use only for local development where no real PII is stored.
	slog.Warn("crypto: FIELD_ENCRYPTION_KEY not set; using ephemeral random key — "+
		"encrypted data will be unreadable after restart. "+
		"Set FIELD_ENCRYPTION_KEY for persistent encryption.",
		"env", envKey,
	)
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("crypto: generate ephemeral key: %w", err)
	}
	return NewFieldCipher(key)
}

// ResetGlobalForTest resets the package-level singleton so tests can inject
// a specific key via SetGlobalForTest.  Must only be called from tests.
func ResetGlobalForTest() {
	globalCipherOnce = sync.Once{}
	globalCipher = nil
	globalCipherErr = nil
}

// SetGlobalForTest directly sets the global FieldCipher instance for tests.
// Must only be called from tests after ResetGlobalForTest.
func SetGlobalForTest(fc *FieldCipher) {
	globalCipherOnce.Do(func() {
		globalCipher = fc
	})
}
