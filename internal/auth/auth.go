// Package auth holds the password and token primitives shared by the HTTP layer and
// the seeder. It is small on purpose: one way to hash a password and one way to mint
// a token, so a future change happens in one place.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// MinPasswordLength is deliberately modest: this is a family calendar behind an
// invite-only signup and a rate limiter, not a bank. A long minimum here mostly
// produces passwords written on the fridge.
const MinPasswordLength = 8

// RFC 9106 parameters for the memory-constrained case, which is the right profile for
// a small home server: 64 MiB, three passes, four lanes.
const (
	argonMemory  = 64 * 1024
	argonTime    = 3
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns a PHC-format argon2id string. The parameters travel inside the
// hash, so raising them later leaves existing passwords verifiable.
func HashPassword(plain string) (string, error) {
	if len(plain) < MinPasswordLength {
		return "", fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether plain matches the stored PHC hash. A malformed hash
// is an error, not a silent false: it means the row is corrupt, and treating that as
// "wrong password" would send someone round a password-reset loop forever.
func VerifyPassword(encoded, plain string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("unrecognised password hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	var memory uint32
	var times uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &times, &threads); err != nil {
		return false, fmt.Errorf("unreadable argon2 parameters %q", parts[3])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("unreadable salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("unreadable hash: %w", err)
	}

	// argon2.IDKey panics rather than erring on a profile it cannot run: fewer than
	// one pass, fewer than one lane, or a tag of no length (blake2b has no
	// zero-size constructor). Those are all shapes a corrupt row can hold, and this
	// function's contract is that a malformed hash is an error — a panic here is a
	// 500 on every login attempt for that account instead.
	if times < 1 || threads < 1 {
		return false, fmt.Errorf("argon2 parameters %q are out of range: t and p must be at least 1", parts[3])
	}
	if len(want) == 0 {
		return false, fmt.Errorf("stored argon2 hash is empty")
	}

	got := argon2.IDKey([]byte(plain), salt, times, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NewToken mints a 256-bit token, returning the value to hand out and the SHA-256 to
// store. Sessions, invites and password resets all work this way: the database never
// holds anything that would let a reader impersonate someone, and the plaintext exists
// only in a cookie or a link.
//
// SHA-256 rather than argon2 is deliberate — these are full-entropy random values, so
// there is nothing to brute-force, and a session lookup happens on every request.
func NewToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken is how a presented token is looked up.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
