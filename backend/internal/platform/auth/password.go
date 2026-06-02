// Package auth provides password hashing, session management, and
// authentication middleware for the HR SaaS backend.
//
// Security note: raw passwords and session tokens must never be logged or
// persisted beyond their immediate use.  All logging in this package uses
// structured slog and omits any value derived from user credentials.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2Params holds the Argon2id cost parameters.
// Exported for test-time override via NewParams.
type argon2Params struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLen     uint32
	keyLen      uint32
}

// defaultArgon2Params returns the production-safe defaults.
// Values satisfy OWASP (2023) minimum for Argon2id:
//   - memory  ≥ 19 MiB (64 MiB here for margin)
//   - time    ≥ 1  (2 here)
//   - threads = 1
//
// Adjust upward when profiling shows the latency budget allows it.
var defaultArgon2Params = argon2Params{
	memory:      64 * 1024, // 64 MiB
	iterations:  2,
	parallelism: 1,
	saltLen:     16,
	keyLen:      32,
}

// Errors returned by password functions.
var (
	// ErrInvalidHash is returned when Verify receives an encoded string that
	// does not conform to the expected argon2id format.
	ErrInvalidHash = errors.New("auth: invalid password hash format")

	// ErrIncompatibleVersion is returned when the encoded hash was produced
	// with an argon2 version different from the one this binary supports.
	ErrIncompatibleVersion = errors.New("auth: incompatible argon2 version")
)

// HashPassword derives an Argon2id hash for password using the default cost
// parameters and a cryptographically-random salt.
//
// The returned string is self-describing and encodes the algorithm version,
// parameters, salt (base64), and key (base64):
//
//	$argon2id$v=19$m=65536,t=2,p=1$<salt-b64>$<key-b64>
//
// The same password hashed twice yields different encoded strings (different
// salts) — Verify must be used for comparisons.
func HashPassword(password string) (string, error) {
	return hashWithParams(password, defaultArgon2Params)
}

// hashWithParams is the internal implementation that accepts explicit params.
// Tests can call this with reduced parameters to keep test runtime fast.
func hashWithParams(password string, p argon2Params) (string, error) {
	salt := make([]byte, p.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	key := argon2.IDKey(
		[]byte(password),
		salt,
		p.iterations,
		p.memory,
		p.parallelism,
		p.keyLen,
	)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Key := base64.RawStdEncoding.EncodeToString(key)

	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		p.memory,
		p.iterations,
		p.parallelism,
		b64Salt,
		b64Key,
	)
	return encoded, nil
}

// VerifyPassword checks whether password matches the argon2id encoded hash.
//
// Returns (true, nil) on match, (false, nil) on mismatch, and (false, err)
// when encoded is malformed or uses an incompatible algorithm version.
//
// The comparison is performed with crypto/subtle.ConstantTimeCompare to
// prevent timing side-channels.
func VerifyPassword(encoded, password string) (bool, error) {
	p, salt, expectedKey, err := decodeArgon2Hash(encoded)
	if err != nil {
		return false, err
	}

	actualKey := argon2.IDKey(
		[]byte(password),
		salt,
		p.iterations,
		p.memory,
		p.parallelism,
		p.keyLen,
	)

	if subtle.ConstantTimeCompare(expectedKey, actualKey) != 1 {
		return false, nil
	}
	return true, nil
}

// decodeArgon2Hash parses an encoded argon2id string produced by hashWithParams.
// Returns the parameters, salt bytes, and key bytes.
func decodeArgon2Hash(encoded string) (argon2Params, []byte, []byte, error) {
	// Expected format:
	//   $argon2id$v=<V>$m=<M>,t=<T>,p=<P>$<salt-b64>$<key-b64>
	// Split on '$'; leading '$' means parts[0] is always "".
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return argon2Params{}, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return argon2Params{}, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argon2Params{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return argon2Params{}, nil, nil, ErrIncompatibleVersion
	}

	var p argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism); err != nil {
		return argon2Params{}, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argon2Params{}, nil, nil, ErrInvalidHash
	}
	p.saltLen = uint32(len(salt))

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argon2Params{}, nil, nil, ErrInvalidHash
	}
	p.keyLen = uint32(len(key))

	return p, salt, key, nil
}
