package crypto_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-org/hr-saas/internal/platform/crypto"
)

// syntheticKey returns a synthetic 32-byte test key.
// This is NOT a real key — it is a fixed constant used only in unit tests
// to exercise the encrypt/decrypt logic.  It is never used for real PII.
func syntheticKey() []byte {
	// 32 bytes of repeating 0x42 ('B') — clearly synthetic, not a real secret.
	return bytes.Repeat([]byte{0x42}, 32)
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)

	plaintext := []byte("合成口座番号 1234567890 — テスト用データ")

	ciphertext, err := fc.Encrypt(plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, plaintext, ciphertext, "ciphertext must not equal plaintext")
	assert.Greater(t, len(ciphertext), len(plaintext), "ciphertext must be longer than plaintext")

	decrypted, err := fc.Decrypt(ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted, "decrypted value must equal original plaintext")
}

func TestEncryptProducesDistinctCiphertexts(t *testing.T) {
	// Two encryptions of the same plaintext must produce different ciphertexts
	// because each uses a fresh random nonce.
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)

	plain := []byte("合成データ")
	ct1, err := fc.Encrypt(plain)
	require.NoError(t, err)
	ct2, err := fc.Encrypt(plain)
	require.NoError(t, err)

	assert.NotEqual(t, ct1, ct2, "two encryptions of the same plaintext must differ (nonce randomness)")
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)

	ct, err := fc.Encrypt([]byte("合成テストデータ"))
	require.NoError(t, err)

	// Flip the last byte of the ciphertext (GCM tag) to simulate tampering.
	tampered := make([]byte, len(ct))
	copy(tampered, ct)
	tampered[len(tampered)-1] ^= 0xFF

	_, err = fc.Decrypt(tampered)
	assert.ErrorIs(t, err, crypto.ErrDecryptFailed, "tampered ciphertext must return ErrDecryptFailed")
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	key1 := bytes.Repeat([]byte{0x11}, 32)
	key2 := bytes.Repeat([]byte{0x22}, 32)

	fc1, err := crypto.NewFieldCipher(key1)
	require.NoError(t, err)
	fc2, err := crypto.NewFieldCipher(key2)
	require.NoError(t, err)

	ct, err := fc1.Encrypt([]byte("合成データ"))
	require.NoError(t, err)

	_, err = fc2.Decrypt(ct)
	assert.ErrorIs(t, err, crypto.ErrDecryptFailed, "decryption with wrong key must return ErrDecryptFailed")
}

func TestDecryptTooShortFails(t *testing.T) {
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)

	// A slice shorter than the nonce size (12 bytes) must fail.
	_, err = fc.Decrypt([]byte("short"))
	assert.ErrorIs(t, err, crypto.ErrDecryptFailed)
}

func TestNewFieldCipherBadKeyLength(t *testing.T) {
	_, err := crypto.NewFieldCipher([]byte("tooshort"))
	assert.Error(t, err, "key shorter than 32 bytes must be rejected")

	_, err = crypto.NewFieldCipher(bytes.Repeat([]byte{0x01}, 33))
	assert.Error(t, err, "key longer than 32 bytes must be rejected")
}

func TestGlobalEncryptDecrypt(t *testing.T) {
	// Reset the global singleton so we can inject a synthetic key.
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)

	plain := []byte("グローバル暗号化テスト — 合成データ")
	ct, err := crypto.Encrypt(plain)
	require.NoError(t, err)

	got, err := crypto.Decrypt(ct)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
}
