package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastParams uses minimal argon2id cost so unit tests complete quickly
// without sacrificing correctness verification.
var fastParams = argon2Params{
	memory:      4 * 1024, // 4 MiB — still correct, just fast
	iterations:  1,
	parallelism: 1,
	saltLen:     16,
	keyLen:      32,
}

func hashFast(t *testing.T, password string) string {
	t.Helper()
	h, err := hashWithParams(password, fastParams)
	require.NoError(t, err)
	return h
}

func TestHashPassword_Format(t *testing.T) {
	encoded := hashFast(t, "hunter2")
	assert.True(t, strings.HasPrefix(encoded, "$argon2id$"), "encoded must start with $argon2id$")
	parts := strings.Split(encoded, "$")
	assert.Len(t, parts, 6, "encoded must have 6 $-delimited parts")
}

func TestVerifyPassword_CorrectPassword(t *testing.T) {
	encoded := hashFast(t, "correct-horse-battery-staple")
	ok, err := VerifyPassword(encoded, "correct-horse-battery-staple")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestVerifyPassword_WrongPassword(t *testing.T) {
	encoded := hashFast(t, "correct-horse-battery-staple")
	ok, err := VerifyPassword(encoded, "wrong-password")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestVerifyPassword_TamperedHash(t *testing.T) {
	encoded := hashFast(t, "secret")
	// Truncate the last character to corrupt the key segment.
	tampered := encoded[:len(encoded)-1]
	_, err := VerifyPassword(tampered, "secret")
	// Should return an error (malformed base64) or a false match — never silently succeed.
	// We accept either error or (false, nil); what is forbidden is (true, nil).
	if err == nil {
		// If no parse error, the comparison must still fail.
		ok, err2 := VerifyPassword(tampered, "secret")
		require.NoError(t, err2)
		assert.False(t, ok, "tampered hash must not verify as true")
	}
}

func TestVerifyPassword_TamperedHashBadFormat(t *testing.T) {
	_, err := VerifyPassword("not-a-valid-hash", "password")
	assert.ErrorIs(t, err, ErrInvalidHash)
}

func TestVerifyPassword_IncompatibleVersion(t *testing.T) {
	// Construct a hash with a bogus version number (999).
	bad := "$argon2id$v=999$m=4096,t=1,p=1$c2FsdHNhbHRzYWx0c2Fs$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2U"
	_, err := VerifyPassword(bad, "any")
	assert.ErrorIs(t, err, ErrIncompatibleVersion)
}

func TestHashPassword_UniquePerCall(t *testing.T) {
	// Same password → different encoded strings (different salts).
	h1 := hashFast(t, "same-password")
	h2 := hashFast(t, "same-password")
	assert.NotEqual(t, h1, h2, "two hashes of the same password must differ (unique salts)")
}

func TestHashPassword_VariousInputs(t *testing.T) {
	cases := []struct {
		name     string
		password string
	}{
		{"empty", ""},
		{"ascii", "hello world"},
		{"unicode", "パスワード"},
		{"special", "!@#$%^&*()-_=+[]{}|;':\",./<>?"},
		{"long", strings.Repeat("a", 512)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := hashWithParams(tc.password, fastParams)
			require.NoError(t, err)
			ok, err := VerifyPassword(encoded, tc.password)
			require.NoError(t, err)
			assert.True(t, ok)

			// Wrong password must not verify.
			ok2, err2 := VerifyPassword(encoded, tc.password+"WRONG")
			require.NoError(t, err2)
			assert.False(t, ok2)
		})
	}
}
