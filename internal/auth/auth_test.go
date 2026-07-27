package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// The parameters and lengths asserted in this file are written out as literals rather
// than read from the constants in auth.go. A test that compares argonMemory with
// argonMemory agrees with whatever someone puts there, which is exactly the change
// this package needs to be told about.

const testPassword = "correct horse battery staple"

func mustHash(t *testing.T, plain string) string {
	t.Helper()
	encoded, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword(%q) returned %v", plain, err)
	}
	return encoded
}

func mustDecode(t *testing.T, b64 string) []byte {
	t.Helper()
	raw, err := base64.RawStdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode %q: %v", b64, err)
	}
	return raw
}

// ---------------------------------------------------------------------------
// Password hashing
// ---------------------------------------------------------------------------

// TestHashPasswordUsesRFC9106Parameters is the reason this file exists. Weakening
// argon2's cost is a one-character edit that breaks no feature, fails no other test,
// and is invisible in the running app — every password still verifies, just faster,
// and so does every password an attacker guesses offline. So the numbers are pinned.
//
// The recomputation at the end matters as much as the string comparison: the encoded
// parameters are produced from the same constants as the key, so a hash could
// truthfully advertise m=65536,t=3,p=4 while having been computed by argon2i, or with
// the arguments to IDKey transposed. Deriving the key independently here is what
// proves the advertised profile is the one that was actually paid for.
func TestHashPasswordUsesRFC9106Parameters(t *testing.T) {
	// RFC 9106's second recommended option, the memory-constrained one: 64 MiB
	// (65536 KiB), three passes, four lanes. Version 19 is 0x13, argon2 v1.3.
	const (
		wantPrefix  = "$argon2id$v=19$m=65536,t=3,p=4$"
		wantMemory  = 64 * 1024
		wantTime    = 3
		wantThreads = 4
		wantSaltLen = 16 // 128 bits, the RFC's recommended salt
		wantKeyLen  = 32 // 256 bits of tag
	)

	encoded := mustHash(t, testPassword)
	if !strings.HasPrefix(encoded, wantPrefix) {
		t.Fatalf("hash = %q, want it to start with %q", encoded, wantPrefix)
	}
	// A hash that still contained the password would be a catastrophe that no
	// login test could see, because login would keep working.
	if strings.Contains(encoded, testPassword) {
		t.Fatalf("hash %q contains the plaintext password", encoded)
	}

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		t.Fatalf("hash %q has %d $-separated fields, want 6", encoded, len(parts))
	}
	salt := mustDecode(t, parts[4])
	key := mustDecode(t, parts[5])
	if len(salt) != wantSaltLen {
		t.Errorf("salt is %d bytes, want %d", len(salt), wantSaltLen)
	}
	if len(key) != wantKeyLen {
		t.Errorf("key is %d bytes, want %d", len(key), wantKeyLen)
	}

	want := argon2.IDKey([]byte(testPassword), salt, wantTime, wantMemory, wantThreads, wantKeyLen)
	if !bytes.Equal(key, want) {
		t.Errorf("the stored key is not argon2id over this salt at m=%d,t=%d,p=%d:\n"+
			"the hash advertises those parameters but was computed with something else",
			wantMemory, wantTime, wantThreads)
	}
}

// TestHashPasswordVerifies is the round trip everything else depends on.
func TestHashPasswordVerifies(t *testing.T) {
	cases := []struct {
		name     string
		password string
		attempt  string
		want     bool
	}{
		{"the same password", testPassword, testPassword, true},
		{"a different password", testPassword, "correct horse battery stapl", false},
		{"one character shorter", testPassword, testPassword[:len(testPassword)-1], false},
		{"one character longer", testPassword, testPassword + "!", false},
		{"case matters", testPassword, strings.ToUpper(testPassword), false},
		{"a leading space is not trimmed away", testPassword, " " + testPassword, false},
		{"the empty attempt", testPassword, "", false},
		// Verification must not apply the minimum-length rule: it guards what may be
		// stored, not what may be typed at a login form.
		{"a short stored password still verifies", "12345678", "12345678", true},
		// Passwords are bytes, not letters: a hash must survive a round trip through
		// anything a phone keyboard can produce.
		{"non-ascii", "mot de passe très sûr 🔐", "mot de passe très sûr 🔐", true},
		{"non-ascii near miss", "mot de passe très sûr 🔐", "mot de passe tres sur 🔐", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(mustHash(t, tc.password), tc.attempt)
			if err != nil {
				t.Fatalf("VerifyPassword returned %v", err)
			}
			if ok != tc.want {
				t.Errorf("VerifyPassword = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestHashPasswordSaltsEveryHash guards the property that makes stolen hashes worth
// less than stolen passwords. Two accounts that happen to share a password must not
// share a row: identical rows say so out loud, one cracked hash cracks both, and a
// precomputed table becomes worth building.
func TestHashPasswordSaltsEveryHash(t *testing.T) {
	first, second := mustHash(t, testPassword), mustHash(t, testPassword)
	if first == second {
		t.Fatalf("hashing the same password twice produced the same string %q: the salt is not random", first)
	}
	if firstSalt, secondSalt := strings.Split(first, "$")[4], strings.Split(second, "$")[4]; firstSalt == secondSalt {
		t.Errorf("both hashes carry the salt %q", firstSalt)
	}
	// Different output, same answer: the salt is only useful if it travels inside
	// the hash, so both rows must still accept the password they were made from.
	for _, encoded := range []string{first, second} {
		ok, err := VerifyPassword(encoded, testPassword)
		if err != nil || !ok {
			t.Errorf("VerifyPassword(%q) = %v, %v; want true, nil", encoded, ok, err)
		}
	}
}

// TestVerifyPasswordHonoursTheStoredParameters is the upgrade path. The cost is meant
// to be raised over the life of the install, and everyone's stored hash predates the
// raise, so verification must use the parameters written in the row rather than
// today's constants. Getting this wrong locks out every existing account at once.
func TestVerifyPasswordHonoursTheStoredParameters(t *testing.T) {
	cases := []struct {
		name          string
		memory, times uint32
		threads       uint8
		keyLen        uint32
	}{
		// A hash from a hypothetical cheaper past.
		{"lower cost than today", 8 * 1024, 1, 1, 32},
		// And from a more expensive future, since a rollback runs the old binary
		// against rows the new one wrote.
		{"higher cost than today", 128 * 1024, 4, 8, 32},
		// The tag length is read from the stored key, not assumed to be 32.
		{"64-byte tag", 8 * 1024, 1, 1, 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			salt := []byte("sixteen-byte-slt")
			key := argon2.IDKey([]byte(testPassword), salt, tc.times, tc.memory, tc.threads, tc.keyLen)
			encoded := phc(tc.memory, tc.times, tc.threads, salt, key)

			ok, err := VerifyPassword(encoded, testPassword)
			if err != nil {
				t.Fatalf("VerifyPassword returned %v", err)
			}
			if !ok {
				t.Error("VerifyPassword rejected a correct password stored under different parameters")
			}
			// The parameters must not become a way in: a wrong password stays wrong
			// however cheap the row says it was hashed.
			if ok, err := VerifyPassword(encoded, "not the password"); err != nil || ok {
				t.Errorf("VerifyPassword with a wrong password = %v, %v; want false, nil", ok, err)
			}
		})
	}
}

// phc assembles a stored hash by hand, the way a row written by an older or newer
// binary would look.
func phc(memory, times uint32, threads uint8, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, times, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// TestVerifyPasswordRejectsMalformedHashes covers the corrupt-row case. It must come
// back as an error rather than as a plain false: the HTTP layer turns false into
// "wrong password, try again", which sends the owner of a damaged row round the
// password-reset loop forever, resetting to a password that then also fails to work.
//
// The other half of the contract is that nothing here authenticates. A hash the
// parser does not understand is not a hash that matches.
func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	salt := []byte("sixteen-byte-slt")
	saltB64 := base64.RawStdEncoding.EncodeToString(salt)
	keyB64 := base64.RawStdEncoding.EncodeToString(argon2.IDKey([]byte(testPassword), salt, 3, 64*1024, 4, 32))
	body := "$" + saltB64 + "$" + keyB64

	cases := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"not a hash at all", "hunter2"},
		{"the plaintext password", testPassword},
		{"no dollars", "argon2id v=19 m=65536,t=3,p=4 " + saltB64 + " " + keyB64},
		{"bcrypt", "$2b$12$C6UzMDM.H6dfI/f/IKcEe.7BlbLhVsnFOMBLUrOjmCLGGgqZgLbLu"},
		{"argon2i is not argon2id", "$argon2i$v=19$m=65536,t=3,p=4" + body},
		{"argon2d is not argon2id", "$argon2d$v=19$m=65536,t=3,p=4" + body},
		{"scrypt", "$scrypt$ln=15,r=8,p=1" + body},
		{"a field short", "$argon2id$v=19$m=65536,t=3,p=4$" + saltB64},
		{"a field too many", "$argon2id$v=19$m=65536,t=3,p=4" + body + "$extra"},
		// argon2 v1.0 salts the first pass differently, so accepting the label and
		// hashing with v1.3 would silently reject everybody's password.
		{"argon2 version 1.0", "$argon2id$v=16$m=65536,t=3,p=4" + body},
		{"unreadable version", "$argon2id$v=nineteen$m=65536,t=3,p=4" + body},
		{"no version field", "$argon2id$$m=65536,t=3,p=4" + body},
		{"unreadable memory", "$argon2id$v=19$m=lots,t=3,p=4" + body},
		{"negative time cost", "$argon2id$v=19$m=65536,t=-1,p=4" + body},
		{"parameters out of order", "$argon2id$v=19$t=3,m=65536,p=4" + body},
		{"missing parallelism", "$argon2id$v=19$m=65536,t=3" + body},
		// These three used to panic inside argon2.IDKey rather than come back as
		// errors — no passes, no lanes, and a tag of no length, none of which the
		// derivation can run. The recover in the HTTP middleware turned each into a
		// 500 on every login attempt for that account, which reads as "the site is
		// broken" rather than "this row is corrupt".
		{"zero time cost", "$argon2id$v=19$m=65536,t=0,p=4" + body},
		{"zero parallelism", "$argon2id$v=19$m=65536,t=3,p=0" + body},
		{"empty tag", "$argon2id$v=19$m=65536,t=3,p=4$" + saltB64 + "$"},
		{"salt is not base64", "$argon2id$v=19$m=65536,t=3,p=4$!!!!$" + keyB64},
		// The salt and key are standard base64, not the URL alphabet: '-' and '_'
		// are not the same bytes, and quietly accepting them would verify against
		// the wrong salt.
		{"salt in the url alphabet", "$argon2id$v=19$m=65536,t=3,p=4$a-b_cdef$" + keyB64},
		{"key is not base64", "$argon2id$v=19$m=65536,t=3,p=4$" + saltB64 + "$!!!!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.encoded, testPassword)
			if err == nil {
				t.Errorf("VerifyPassword(%q) returned no error, want one", tc.encoded)
			}
			if ok {
				t.Errorf("VerifyPassword(%q) accepted the password", tc.encoded)
			}
		})
	}
}

// TestVerifyPasswordRejectsNearMisses is the behavioural half of "compare the whole
// tag, in constant time". Each row is a stored hash that differs from the real one by
// as little as a single bit, in a position a short-circuiting or truncating comparison
// would never reach. None of them may authenticate.
//
// Whether a length-changing corruption comes back as false or as an error is left
// open on purpose — only "did not authenticate" is the contract being pinned.
func TestVerifyPasswordRejectsNearMisses(t *testing.T) {
	encoded := mustHash(t, testPassword)
	parts := strings.Split(encoded, "$")
	salt, key := mustDecode(t, parts[4]), mustDecode(t, parts[5])

	withKey := func(k []byte) string {
		return strings.Join(append(append([]string{}, parts[:5]...), base64.RawStdEncoding.EncodeToString(k)), "$")
	}
	flip := func(i int) []byte {
		mutated := bytes.Clone(key)
		mutated[i] ^= 1
		return mutated
	}

	cases := []struct {
		name    string
		encoded string
	}{
		{"one bit of the first byte of the tag", withKey(flip(0))},
		{"one bit in the middle of the tag", withKey(flip(len(key) / 2))},
		{"one bit of the last byte of the tag", withKey(flip(len(key) - 1))},
		{"the tag truncated by one byte", withKey(key[:len(key)-1])},
		{"the tag with a byte appended", withKey(append(bytes.Clone(key), 0))},
		// Eight bytes rather than one. Verification re-derives at the *stored* tag
		// length, and argon2's output is not a prefix of a longer output, so a short
		// tag is compared against an independently derived one of the same size: at
		// one byte the two collide once in 256 runs and this test failed in CI on a
		// hash that was behaving correctly. Eight bytes keeps the case — a stored tag
		// far shorter than the profile must still not wave a password through — while
		// putting the collision at one in 2^64.
		//
		// Whether a tag that short should be refused outright is a policy question
		// rather than a comparison one, and belongs with the parameter lower bounds in
		// issue #31.
		{"a tag much shorter than the profile", withKey(key[:8])},
		{"a tag of the right length from another password", withKey(argon2.IDKey([]byte("something else"), salt, 3, 64*1024, 4, 32))},
		{"the right tag under a different salt", phc(64*1024, 3, 4, []byte("different-salt!!"), key)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ok, _ := VerifyPassword(tc.encoded, testPassword); ok {
				t.Error("VerifyPassword authenticated a password against a hash that is not its own")
			}
		})
	}
}

// TestHashPasswordEnforcesTheMinimumLength keeps the floor in the one place every
// caller goes through. The HTTP layer checks it too, but `almanack bootstrap` and the
// seeder call HashPassword directly and would otherwise be able to create an account
// the signup form would have refused.
func TestHashPasswordEnforcesTheMinimumLength(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"empty", "", true},
		{"one character", "a", true},
		{"one below the minimum", "1234567", true},
		{"exactly the minimum", "12345678", false},
		{"comfortably long", testPassword, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := HashPassword(tc.password)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("HashPassword(%q) succeeded, want an error", tc.password)
				}
				// A rejected password must not also come back with a usable hash;
				// a caller that ignores the error must not get an account.
				if encoded != "" {
					t.Errorf("HashPassword(%q) returned both an error and the hash %q", tc.password, encoded)
				}
				return
			}
			if err != nil {
				t.Fatalf("HashPassword(%q) returned %v", tc.password, err)
			}
			if ok, err := VerifyPassword(encoded, tc.password); err != nil || !ok {
				t.Errorf("VerifyPassword after HashPassword(%q) = %v, %v; want true, nil", tc.password, ok, err)
			}
		})
	}

	// The number is user-visible: both locales promise "8 characters minimum" under
	// the signup field, so moving it here alone makes the form either lie or reject
	// what it just asked for.
	if MinPasswordLength != 8 {
		t.Errorf("MinPasswordLength = %d, but i18n auth.passwordTooShort still says 8", MinPasswordLength)
	}
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// TestNewTokenStoresOnlyItsHash guards the invariant the whole token design rests on:
// what goes in the database must be useless to whoever reads the database. Returning
// the token as its own "hash" is a two-word regression that breaks no test which only
// checks that a session works, because a session would keep working perfectly — right
// up until a backup, a log line or a stray SELECT hands someone every live session.
func TestNewTokenStoresOnlyItsHash(t *testing.T) {
	token, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken returned %v", err)
	}
	if token == "" || hash == "" {
		t.Fatalf("NewToken returned token %q and hash %q", token, hash)
	}
	if hash == token {
		t.Fatal("NewToken returned the token as its own stored form: the database would hold live credentials")
	}
	if strings.Contains(hash, token) || strings.Contains(token, hash) {
		t.Fatalf("token %q and stored hash %q contain one another", token, hash)
	}

	// Computed here rather than by calling HashToken, so that a change to both
	// functions at once still has to answer for itself.
	sum := sha256.Sum256([]byte(token))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); hash != want {
		t.Errorf("stored hash = %q, want the base64url SHA-256 of the token %q", hash, want)
	}
	// And the lookup path has to agree, or every session cookie is a stranger.
	if got := HashToken(token); got != hash {
		t.Errorf("HashToken(token) = %q, but NewToken stored %q", got, hash)
	}
}

// TestNewTokenIsFullEntropy pins the 256 bits. These values are the entire defence for
// a session cookie and an invite link — there is no second factor and no per-token
// rate limit on the lookup — so a shortened or repeating token is a way in.
func TestNewTokenIsFullEntropy(t *testing.T) {
	const mints = 256

	seenTokens := make(map[string]bool, mints)
	seenHashes := make(map[string]bool, mints)
	for i := 0; i < mints; i++ {
		token, hash, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken returned %v", err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatalf("token %q is not base64url: %v", token, err)
		}
		if len(raw) != 32 {
			t.Fatalf("token decodes to %d bytes, want 32 (256 bits)", len(raw))
		}
		// A rand.Read whose error was swallowed, or a buffer that was never
		// filled, produces perfectly plausible-looking all-zero tokens.
		if bytes.Equal(raw, make([]byte, 32)) {
			t.Fatalf("token %q is all zero bytes", token)
		}
		if seenTokens[token] {
			t.Fatalf("NewToken returned %q twice in %d mints", token, mints)
		}
		if seenHashes[hash] {
			t.Fatalf("NewToken returned the stored hash %q twice in %d mints", hash, mints)
		}
		seenTokens[token] = true
		seenHashes[hash] = true
	}
}

// TestTokensSurviveURLsAndCookies pins the alphabet. Invite and reset tokens are
// handed to people as links and arrive back as a path segment — `GET
// /api/v1/invites/{token}` — where a '/' from standard base64 would simply fail to
// match the route, and a '+' in a query string decodes as a space. Session tokens go
// into a cookie value, which does not admit '=' or ',' either. RawURLEncoding avoids
// all of it; switching to StdEncoding for tidiness would break invites for roughly
// one link in ten, which is exactly the rate that reads as "flaky" rather than "bug".
func TestTokensSurviveURLsAndCookies(t *testing.T) {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
	}
	for i := 0; i < 64; i++ {
		token, hash, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken returned %v", err)
		}
		for _, value := range []string{token, hash} {
			for _, r := range value {
				if !safe(r) {
					t.Fatalf("%q contains %q, which is not safe in a URL path segment or a cookie value", value, r)
				}
			}
		}
	}
}

// TestHashToken covers the lookup side on its own. It has to be a pure function of the
// token: the session middleware hashes the presented cookie on every single request and
// compares it with what was stored at login, so anything stateful or non-deterministic
// here logs the whole family out.
func TestHashToken(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"a real token", "9Q7g3xW1rN4vK8sT0bYtVcXhF2mLpQ6uZ1jA5nE7dCg"},
		{"one character", "a"},
		{"non-ascii", "clé de session"},
		{"long", strings.Repeat("token", 400)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HashToken(tc.token)
			if got != HashToken(tc.token) {
				t.Fatal("HashToken is not deterministic")
			}
			sum := sha256.Sum256([]byte(tc.token))
			if want := base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
				t.Errorf("HashToken(%q) = %q, want %q", tc.token, got, want)
			}
			// 32 bytes in base64url, unpadded.
			if len(got) != 43 {
				t.Errorf("HashToken(%q) is %d characters, want 43", tc.token, len(got))
			}
			if tc.token != "" && got == tc.token {
				t.Errorf("HashToken(%q) returned its input", tc.token)
			}
		})
	}

	// Distinct tokens must not collide into one row, and near-identical ones least
	// of all: a hash that only depended on, say, the first bytes would make a token
	// guessable one character at a time.
	distinct := []string{"", "a", "b", "ab", "ba", "token", "token ", "Token"}
	seen := make(map[string]string, len(distinct))
	for _, token := range distinct {
		hash := HashToken(token)
		if other, clash := seen[hash]; clash {
			t.Errorf("HashToken(%q) and HashToken(%q) both give %q", token, other, hash)
		}
		seen[hash] = token
	}
}
