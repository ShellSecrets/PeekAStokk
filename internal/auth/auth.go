// Package auth hashes and verifies the UI password with Argon2id, so the
// config file can hold a hash instead of the plaintext password.
//
// Hashes use the standard PHC string format
// ($argon2id$v=19$m=...,t=...,p=...$salt$key), so they are interchangeable
// with other Argon2 tooling.
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

// Hash parameters follow RFC 9106's second recommended option (64 MiB),
// with t=3 for extra margin. Verification cost is ~100ms once per process;
// the server caches the verified credential.
const (
	hashTime    = 3
	hashMemory  = 64 * 1024 // KiB
	hashThreads = 4
	hashKeyLen  = 32
	saltLen     = 16
)

// maxVerifyMemory bounds the m= parameter accepted from a stored hash, so
// a corrupted or hostile config line cannot make every login attempt
// allocate gigabytes.
const maxVerifyMemory = 1 << 21 // KiB (2 GiB)

// HashPassword derives an Argon2id hash of password in PHC string format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, hashTime, hashMemory, hashThreads, hashKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, hashMemory, hashTime, hashThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// IsHash reports whether s looks like an Argon2id PHC string rather than a
// plaintext password.
func IsHash(s string) bool { return strings.HasPrefix(s, "$argon2id$") }

// VerifyPassword reports whether password matches the PHC-formatted
// Argon2id hash in encoded. The comparison is constant-time.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("not an argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("malformed version: %w", err)
	}
	if version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %d", version)
	}

	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, fmt.Errorf("malformed parameters: %w", err)
	}
	if memory == 0 || time == 0 || threads == 0 || memory > maxVerifyMemory {
		return false, fmt.Errorf("parameters out of range: %s", parts[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("malformed salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("malformed key")
	}

	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
