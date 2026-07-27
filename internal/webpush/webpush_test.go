package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"
)

// ---------------------------------------------------------------------------
// RFC 8291 Appendix A, byte for byte
// ---------------------------------------------------------------------------

// The published worked example from RFC 8291 (section 5 and Appendix A), with the
// presentation whitespace removed. These are the only numbers in this package that
// come from outside it; everything else is derived.
const (
	vecPlaintext  = "When I grow up, I want to be a watermelon"
	vecASPublic   = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	vecASPrivate  = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	vecUAPublic   = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	vecUAPrivate  = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	vecSalt       = "DGv6ra1nlYgDCS1FRnbzlw"
	vecAuthSecret = "BTBZMqHH6r4Tts7J_aSIgg"

	// Intermediate values (Appendix A).
	vecECDHSecret = "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs"
	vecPRKKey     = "Snr3JMxaHVDXHWJn5wdC52WjpCtd2EIEGBykDcZW32k"
	vecKeyInfo    = "V2ViUHVzaDogaW5mbwAEJXGyvs3942BVGq8e0PTNNmwRzr5VX4m8t7GGpTM5FzFo7OLr4BhZe9MEebhuPI-OztV3ylkYfpJGmQ22ggCLDgT-M_SrDepxkU21WCP3O1SUj0EwbZIHMtu5pZpTKGSCIA5Zent7wmC6HCJ5mFgJkuk5cwAvMBKiiujwa7t45ewP"
	vecIKM        = "S4lYMb_L0FxCeq0WhDx813KgSYqU26kOyzWUdsXYyrg"
	vecPRK        = "09_eUZGrsvxChDCGRCdkLiDXrReGOEVeSCdCcPBSJSc"
	vecCEK        = "oIhVW04MRdy2XN9CiKLxTg"
	vecNonce      = "4h_95klXJ5E_qnoN"

	// Outputs (Appendix A) and the complete body from section 5.
	vecHeader     = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	vecCiphertext = "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ"
	vecBody       = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml" +
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT" +
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
)

func mustDecode(t *testing.T, b64 string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode %q: %v", b64, err)
	}
	return b
}

// TestRFC8291AppendixA reproduces the published example end to end. Every
// intermediate is asserted separately so that a future interoperability failure
// points at the exact step of the key schedule that drifted.
func TestRFC8291AppendixA(t *testing.T) {
	asPriv, err := ecdh.P256().NewPrivateKey(mustDecode(t, vecASPrivate))
	if err != nil {
		t.Fatalf("parse application server private key: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(asPriv.PublicKey().Bytes()); got != vecASPublic {
		t.Fatalf("as_public = %s, want %s", got, vecASPublic)
	}

	uaPub, err := ecdh.P256().NewPublicKey(mustDecode(t, vecUAPublic))
	if err != nil {
		t.Fatalf("parse user agent public key: %v", err)
	}

	// The ECDH step, checked on its own: this is where a wrong point encoding
	// would show up.
	secret, err := asPriv.ECDH(uaPub)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(secret); got != vecECDHSecret {
		t.Errorf("ecdh_secret = %s, want %s", got, vecECDHSecret)
	}

	d, err := derive(secret, mustDecode(t, vecAuthSecret), mustDecode(t, vecSalt), uaPub.Bytes(), asPriv.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  []byte
		want string
	}{
		{"PRK_key", d.prkKey, vecPRKKey},
		{"key_info", d.keyInfo, vecKeyInfo},
		{"IKM", d.ikm, vecIKM},
		{"PRK", d.prk, vecPRK},
		{"CEK", d.cek, vecCEK},
		{"NONCE", d.nonce, vecNonce},
	} {
		if got := base64.RawURLEncoding.EncodeToString(tc.got); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, got, tc.want)
		}
	}

	sub := domain.PushSubscription{P256DH: vecUAPublic, Auth: vecAuthSecret}
	body, err := EncryptWith(sub, []byte(vecPlaintext), mustDecode(t, vecSalt), asPriv)
	if err != nil {
		t.Fatalf("EncryptWith: %v", err)
	}

	if len(body) != headerLen+len(vecPlaintext)+1+gcmTagLen {
		t.Errorf("body is %d bytes, want %d", len(body), headerLen+len(vecPlaintext)+1+gcmTagLen)
	}
	if got, want := base64.RawURLEncoding.EncodeToString(body[:headerLen]), vecHeader; got != want {
		t.Errorf("header = %s, want %s", got, want)
	}
	if got, want := base64.RawURLEncoding.EncodeToString(body[headerLen:]), vecCiphertext; got != want {
		t.Errorf("ciphertext = %s, want %s", got, want)
	}
	if want := mustDecode(t, vecBody); !bytes.Equal(body, want) {
		t.Errorf("body mismatch\n got %x\nwant %x", body, want)
	}
}

// TestRFC8291AppendixAReceiver decrypts the RFC's own ciphertext with the
// receiver key it publishes, which proves the framing is readable by a conforming
// user agent and not merely self-consistent.
func TestRFC8291AppendixAReceiver(t *testing.T) {
	uaPriv, err := ecdh.P256().NewPrivateKey(mustDecode(t, vecUAPrivate))
	if err != nil {
		t.Fatalf("parse user agent private key: %v", err)
	}
	got := decryptBody(t, mustDecode(t, vecBody), uaPriv, mustDecode(t, vecAuthSecret))
	if string(got) != vecPlaintext {
		t.Errorf("plaintext = %q, want %q", got, vecPlaintext)
	}
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

// receiver is a fake user agent: the half of the protocol a browser implements.
type receiver struct {
	priv *ecdh.PrivateKey
	auth []byte
	sub  domain.PushSubscription
}

func newReceiver(t *testing.T, endpoint string) *receiver {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate receiver key: %v", err)
	}
	auth := make([]byte, authSecretLen)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	return &receiver{
		priv: priv,
		auth: auth,
		sub: domain.PushSubscription{
			ID:       1,
			UserID:   7,
			Endpoint: endpoint,
			P256DH:   base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
			Auth:     base64.RawURLEncoding.EncodeToString(auth),
		},
	}
}

func (r *receiver) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	return decryptBody(t, body, r.priv, r.auth)
}

// decryptBody parses an aes128gcm body and decrypts its single record. The key
// schedule here is written out with raw HMAC (RFC 8188 notes that HKDF collapses
// to one HMAC at these lengths) rather than calling the package's derive: an
// independent implementation is what makes the round trip a test rather than a
// tautology.
func decryptBody(t *testing.T, body []byte, uaPriv *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	if len(body) < headerLen {
		t.Fatalf("body is %d bytes, shorter than the %d-octet header", len(body), headerLen)
	}
	salt := body[:saltLen]
	rs := binary.BigEndian.Uint32(body[saltLen : saltLen+4])
	idlen := int(body[saltLen+4])
	if idlen != pointLen {
		t.Fatalf("idlen = %d, want %d", idlen, pointLen)
	}
	keyid := body[saltLen+5 : saltLen+5+idlen]
	record := body[saltLen+5+idlen:]
	if uint32(len(record)) > rs {
		t.Fatalf("record is %d bytes, larger than the advertised rs %d", len(record), rs)
	}

	asPub, err := ecdh.P256().NewPublicKey(keyid)
	if err != nil {
		t.Fatalf("keyid is not a valid P-256 point: %v", err)
	}
	secret, err := uaPriv.ECDH(asPub)
	if err != nil {
		t.Fatalf("receiver ecdh: %v", err)
	}

	mac := func(key, data []byte) []byte {
		m := hmac.New(sha256.New, key)
		m.Write(data)
		return m.Sum(nil)
	}
	prkKey := mac(auth, secret)
	keyInfo := append([]byte("WebPush: info\x00"), uaPriv.PublicKey().Bytes()...)
	keyInfo = append(keyInfo, keyid...)
	ikm := mac(prkKey, append(keyInfo, 0x01))
	prk := mac(salt, ikm)
	cek := mac(prk, []byte("Content-Encoding: aes128gcm\x00\x01"))[:16]
	nonce := mac(prk, []byte("Content-Encoding: nonce\x00\x01"))[:12]

	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	plaintext, err := aead.Open(nil, nonce, record, nil)
	if err != nil {
		t.Fatalf("open record: %v", err)
	}
	// Strip the padding: zero or more 0x00 octets follow the 0x02 delimiter.
	end := len(plaintext)
	for end > 0 && plaintext[end-1] == 0x00 {
		end--
	}
	if end == 0 || plaintext[end-1] != padDelimiter {
		t.Fatalf("record does not end with the 0x%02x padding delimiter: % x", padDelimiter, plaintext)
	}
	return plaintext[:end-1]
}

func TestEncryptRoundTrip(t *testing.T) {
	rcv := newReceiver(t, "https://push.example.net/push/abc")
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"empty", []byte{}},
		{"short", []byte(`{"title":"Dentiste","body":"16:30"}`)},
		{"utf8", []byte("Rappel : rendez-vous à 16 h 30 — château d'Écouen 🗓")},
		{"maximum", bytes.Repeat([]byte("x"), MaxPayloadBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := Encrypt(rcv.sub, tc.payload)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if len(body) > maxBodyBytes {
				t.Errorf("body is %d bytes, over the %d-byte service limit", len(body), maxBodyBytes)
			}
			if got := rcv.decrypt(t, body); !bytes.Equal(got, tc.payload) {
				t.Errorf("round trip = %q, want %q", got, tc.payload)
			}
		})
	}
}

// TestEncryptFreshRandomness guards the one property that cannot be checked
// against a vector: two encryptions of the same payload must share neither salt
// nor ephemeral key.
func TestEncryptFreshRandomness(t *testing.T) {
	rcv := newReceiver(t, "https://push.example.net/push/abc")
	a, err := Encrypt(rcv.sub, []byte("same payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := Encrypt(rcv.sub, []byte("same payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(a[:saltLen], b[:saltLen]) {
		t.Error("salt reused across messages")
	}
	if bytes.Equal(a[saltLen+5:headerLen], b[saltLen+5:headerLen]) {
		t.Error("ephemeral public key reused across messages")
	}
}

func TestEncryptMaxPayloadBytes(t *testing.T) {
	if MaxPayloadBytes != 3993 {
		t.Errorf("MaxPayloadBytes = %d, want 3993", MaxPayloadBytes)
	}
	rcv := newReceiver(t, "https://push.example.net/push/abc")
	body, err := Encrypt(rcv.sub, bytes.Repeat([]byte("x"), MaxPayloadBytes))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(body) != maxBodyBytes {
		t.Errorf("a maximum payload produced a %d-byte body, want exactly %d", len(body), maxBodyBytes)
	}
	if _, err := Encrypt(rcv.sub, bytes.Repeat([]byte("x"), MaxPayloadBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Encrypt(MaxPayloadBytes+1) error = %v, want ErrTooLarge", err)
	}
}

func TestEncryptRejectsBadSubscription(t *testing.T) {
	good := newReceiver(t, "https://push.example.net/push/abc").sub
	tests := []struct {
		name string
		sub  domain.PushSubscription
	}{
		{"empty p256dh", domain.PushSubscription{P256DH: "", Auth: good.Auth}},
		{"p256dh not base64", domain.PushSubscription{P256DH: "!!!!", Auth: good.Auth}},
		{"p256dh off curve", domain.PushSubscription{
			P256DH: base64.RawURLEncoding.EncodeToString(append([]byte{0x04}, bytes.Repeat([]byte{0x01}, 64)...)),
			Auth:   good.Auth,
		}},
		{"auth wrong length", domain.PushSubscription{P256DH: good.P256DH, Auth: base64.RawURLEncoding.EncodeToString([]byte("short"))}},
		{"auth empty", domain.PushSubscription{P256DH: good.P256DH, Auth: ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Encrypt(tc.sub, []byte("hi")); err == nil {
				t.Fatal("Encrypt accepted an invalid subscription")
			}
		})
	}
}

// TestDecodeKeyMaterialTolerance documents that keys arriving padded or in the
// standard alphabet still work — clients and hand-edited config produce both.
func TestDecodeKeyMaterialTolerance(t *testing.T) {
	raw := []byte{0xfb, 0xff, 0x00, 0x3e, 0x3f}
	for _, enc := range []string{
		base64.RawURLEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		base64.StdEncoding.EncodeToString(raw),
		base64.RawStdEncoding.EncodeToString(raw),
	} {
		got, err := decodeKeyMaterial(enc)
		if err != nil {
			t.Fatalf("decodeKeyMaterial(%q): %v", enc, err)
		}
		if !bytes.Equal(got, raw) {
			t.Errorf("decodeKeyMaterial(%q) = % x, want % x", enc, got, raw)
		}
	}
	if _, err := decodeKeyMaterial("  "); err == nil {
		t.Error("decodeKeyMaterial accepted a blank key")
	}
}

// ---------------------------------------------------------------------------
// VAPID (RFC 8292)
// ---------------------------------------------------------------------------

func newTestSender(t *testing.T, hc *http.Client) (*Sender, Key) {
	t.Helper()
	pub, priv, err := GenerateKeys()
	if err != nil {
		t.Fatalf("GenerateKeys: %v", err)
	}
	s, err := NewSender(pub, priv, "mailto:almanack@example.org", hc)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s, Key{Public: pub, Private: priv}
}

func TestGenerateKeys(t *testing.T) {
	pub, priv, err := GenerateKeys()
	if err != nil {
		t.Fatalf("GenerateKeys: %v", err)
	}
	pubBytes := mustDecode(t, pub)
	if len(pubBytes) != pointLen {
		t.Errorf("public key is %d bytes, want %d (uncompressed point)", len(pubBytes), pointLen)
	}
	if pubBytes[0] != 0x04 {
		t.Errorf("public key starts with 0x%02x, want 0x04 (uncompressed form)", pubBytes[0])
	}
	if got := len(mustDecode(t, priv)); got != 32 {
		t.Errorf("private key is %d bytes, want 32", got)
	}
	if strings.ContainsAny(pub+priv, "+/=") {
		t.Error("keys must be base64url without padding")
	}
	if _, err := parseKey(pub, priv); err != nil {
		t.Errorf("parseKey on a freshly generated pair: %v", err)
	}
}

func TestNewSenderValidation(t *testing.T) {
	pub, priv, err := GenerateKeys()
	if err != nil {
		t.Fatalf("GenerateKeys: %v", err)
	}
	otherPub, _, err := GenerateKeys()
	if err != nil {
		t.Fatalf("GenerateKeys: %v", err)
	}

	tests := []struct {
		name             string
		pub, priv, subj  string
		wantErrSubstring string
	}{
		{"valid mailto", pub, priv, "mailto:a@example.org", ""},
		{"valid https", pub, priv, "https://example.org/contact", ""},
		{"mismatched pair", otherPub, priv, "mailto:a@example.org", "does not match"},
		{"garbage public", "!!!", priv, "mailto:a@example.org", "public key"},
		{"garbage private", pub, "!!!", "mailto:a@example.org", "private key"},
		{"bare email subject", pub, priv, "a@example.org", "mailto:"},
		{"empty subject", pub, priv, "", "mailto:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSender(tc.pub, tc.priv, tc.subj, nil)
			switch {
			case tc.wantErrSubstring == "" && err != nil:
				t.Fatalf("NewSender: %v", err)
			case tc.wantErrSubstring != "" && err == nil:
				t.Fatal("NewSender accepted invalid input")
			case tc.wantErrSubstring != "" && !strings.Contains(err.Error(), tc.wantErrSubstring):
				t.Fatalf("NewSender error = %v, want it to mention %q", err, tc.wantErrSubstring)
			}
		})
	}
}

// TestVAPIDAuthorization checks the token a push service actually validates: the
// header field shape, the claims, and above all that the signature is the raw
// 64-octet R||S concatenation rather than an ASN.1 SEQUENCE.
func TestVAPIDAuthorization(t *testing.T) {
	s, key := newTestSender(t, nil)
	now := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	s.Clock = clock.NewFake(now)

	header, err := s.authorization("https://updates.push.services.mozilla.com/wpush/v2/gAAAAA", s.Clock.Now())
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}

	scheme, params, ok := strings.Cut(header, " ")
	if !ok || scheme != "vapid" {
		t.Fatalf("Authorization = %q, want it to start with %q", header, "vapid ")
	}
	tokenParam, keyParam, ok := strings.Cut(params, ",")
	if !ok {
		t.Fatalf("Authorization params = %q, want t=...,k=...", params)
	}
	token, ok := strings.CutPrefix(tokenParam, "t=")
	if !ok {
		t.Fatalf("first parameter = %q, want t=<jwt>", tokenParam)
	}
	pubB64, ok := strings.CutPrefix(keyParam, "k=")
	if !ok {
		t.Fatalf("second parameter = %q, want k=<public key>", keyParam)
	}
	if pubB64 != key.Public {
		t.Errorf("k = %s, want the configured public key %s", pubB64, key.Public)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments, want 3", len(parts))
	}

	if got := string(mustDecode(t, parts[0])); got != `{"typ":"JWT","alg":"ES256"}` {
		t.Errorf("JOSE header = %s, want {\"typ\":\"JWT\",\"alg\":\"ES256\"}", got)
	}

	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(mustDecode(t, parts[1]), &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if want := "https://updates.push.services.mozilla.com"; claims.Aud != want {
		t.Errorf("aud = %q, want %q (the origin, not the endpoint)", claims.Aud, want)
	}
	if claims.Sub != "mailto:almanack@example.org" {
		t.Errorf("sub = %q, want mailto:almanack@example.org", claims.Sub)
	}
	if want := now.Add(TokenTTL).Unix(); claims.Exp != want {
		t.Errorf("exp = %d, want %d", claims.Exp, want)
	}
	if d := time.Unix(claims.Exp, 0).Sub(now); d > 24*time.Hour {
		t.Errorf("exp is %s away; RFC 8292 caps it at 24h", d)
	}

	// The classic bug: ecdsa.SignASN1 would put a DER SEQUENCE here, which is
	// 70-72 bytes and rejected by Mozilla and Apple. JWS requires fixed-width
	// R||S.
	sig := mustDecode(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want exactly 64 (raw R||S, never ASN.1/DER)", len(sig))
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), mustDecode(t, key.Public))
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	sInt := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, sInt) {
		t.Error("signature does not verify against the advertised public key")
	}

	// A tampered claim must not verify — proof the signature covers the body.
	badDigest := sha256.Sum256([]byte(parts[0] + "." + parts[1] + "x"))
	if ecdsa.Verify(pub, badDigest[:], r, sInt) {
		t.Error("signature verifies over modified input")
	}
}

// TestVAPIDSignatureFixedWidth pins the padding of small R or S values. A naive
// big.Int.Bytes() concatenation produces a 63-byte signature roughly once in 256
// tokens, which is exactly the kind of bug that looks like a flaky push service.
func TestVAPIDSignatureFixedWidth(t *testing.T) {
	s, _ := newTestSender(t, nil)
	for i := range 200 {
		sig, err := signES256(s.priv, []byte{byte(i)})
		if err != nil {
			t.Fatalf("signES256: %v", err)
		}
		if len(sig) != 64 {
			t.Fatalf("signature %d is %d bytes, want 64", i, len(sig))
		}
	}
}

func TestOrigin(t *testing.T) {
	tests := []struct {
		endpoint string
		want     string
		wantErr  bool
	}{
		{"https://fcm.googleapis.com/fcm/send/dQw4w9Wg", "https://fcm.googleapis.com", false},
		{"https://updates.push.services.mozilla.com/wpush/v2/gAAA", "https://updates.push.services.mozilla.com", false},
		{"https://web.push.apple.com/QDQ0MjE", "https://web.push.apple.com", false},
		{"https://example.com:8443/push", "https://example.com:8443", false},
		{"https://example.com:443/push", "https://example.com", false},
		{"http://127.0.0.1:8080/push", "http://127.0.0.1:8080", false},
		{"http://example.com:80/push", "http://example.com", false},
		{"HTTPS://Example.com/push", "https://Example.com", false},
		{"ftp://example.com/push", "", true},
		{"/relative/push", "", true},
		{"", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.endpoint, func(t *testing.T) {
			got, err := origin(tc.endpoint)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("origin(%q) = %q, want an error", tc.endpoint, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("origin(%q): %v", tc.endpoint, err)
			}
			if got != tc.want {
				t.Errorf("origin(%q) = %q, want %q", tc.endpoint, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Delivery (RFC 8030)
// ---------------------------------------------------------------------------

// capture is what the fake push service saw.
type capture struct {
	requests int
	method   string
	header   http.Header
	body     []byte
}

// newPushService starts a fake push service. status is consulted per request so a
// test can choose what to return.
func newPushService(t *testing.T, cap *capture, status func() int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		cap.requests++
		cap.method = r.Method
		cap.header = r.Header.Clone()
		cap.body = body
		code := http.StatusCreated
		if status != nil {
			code = status()
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

func TestSendHeaderMatrix(t *testing.T) {
	tests := []struct {
		name        string
		opt         Options
		wantTTL     string
		wantUrgency string
		wantTopic   string // "" means the header must be absent
	}{
		{"zero options default to normal urgency and DefaultTTL", Options{}, "21600", "normal", ""},
		{"reminder", Options{TTL: 90 * time.Minute, Urgency: UrgencyHigh}, "5400", "high", ""},
		{"digest collapses on a topic", Options{TTL: 6 * time.Hour, Urgency: UrgencyNormal, Topic: "digest-2026-07-26"}, "21600", "normal", "digest-2026-07-26"},
		{"activity", Options{TTL: 24 * time.Hour, Urgency: UrgencyLow}, "86400", "low", ""},
		{"very low", Options{TTL: time.Second, Urgency: UrgencyVeryLow}, "1", "very-low", ""},
		{"sub-second TTL rounds up", Options{TTL: 1500 * time.Millisecond}, "2", "normal", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cap capture
			srv := newPushService(t, &cap, nil)
			s, key := newTestSender(t, srv.Client())
			rcv := newReceiver(t, srv.URL+"/push/abc")
			payload := []byte(`{"title":"Dentiste"}`)

			if err := s.Send(context.Background(), rcv.sub, payload, tc.opt); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if cap.requests != 1 {
				t.Fatalf("push service saw %d requests, want 1", cap.requests)
			}
			if cap.method != http.MethodPost {
				t.Errorf("method = %s, want POST", cap.method)
			}
			if got := cap.header.Get("TTL"); got != tc.wantTTL {
				t.Errorf("TTL = %q, want %q", got, tc.wantTTL)
			}
			if got := cap.header.Get("Urgency"); got != tc.wantUrgency {
				t.Errorf("Urgency = %q, want %q", got, tc.wantUrgency)
			}
			if got := cap.header.Get("Topic"); got != tc.wantTopic {
				t.Errorf("Topic = %q, want %q", got, tc.wantTopic)
			}
			if _, ok := cap.header["Topic"]; !ok && tc.wantTopic != "" {
				t.Error("Topic header missing")
			}
			if tc.wantTopic == "" {
				if _, ok := cap.header["Topic"]; ok {
					t.Error("Topic header present although no topic was requested")
				}
			}
			if got := cap.header.Get("Content-Encoding"); got != "aes128gcm" {
				t.Errorf("Content-Encoding = %q, want aes128gcm", got)
			}
			if got := cap.header.Get("Authorization"); !strings.HasPrefix(got, "vapid t=") || !strings.Contains(got, ",k="+key.Public) {
				t.Errorf("Authorization = %q, want vapid t=<jwt>,k=%s", got, key.Public)
			}
			if got := rcv.decrypt(t, cap.body); !bytes.Equal(got, payload) {
				t.Errorf("delivered payload = %q, want %q", got, payload)
			}
		})
	}
}

// TestSendTTLAlwaysPresent is separate because its absence is a 400 from Mozilla
// and a silent non-delivery elsewhere: the header is required, never conditional.
func TestSendTTLAlwaysPresent(t *testing.T) {
	var cap capture
	srv := newPushService(t, &cap, nil)
	s, _ := newTestSender(t, srv.Client())
	rcv := newReceiver(t, srv.URL+"/push/abc")

	if err := s.Send(context.Background(), rcv.sub, nil, Options{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, ok := cap.header["Ttl"]; !ok { // http.Header canonicalises TTL to Ttl
		t.Fatalf("TTL header absent; headers were %v", cap.header)
	}
	if _, ok := cap.header["Urgency"]; !ok {
		t.Fatalf("Urgency header absent; headers were %v", cap.header)
	}
}

func TestSendStatusMapping(t *testing.T) {
	tests := []struct {
		status     int
		wantErr    error // sentinel expected via errors.Is, nil for success
		wantStatus bool  // expect a *StatusError
	}{
		{http.StatusOK, nil, false},
		{http.StatusCreated, nil, false},
		{http.StatusAccepted, nil, false},
		{http.StatusNotFound, ErrGone, true},
		{http.StatusGone, ErrGone, true},
		{http.StatusTooManyRequests, ErrRateLimited, true},
		{http.StatusRequestEntityTooLarge, ErrTooLarge, true},
		{http.StatusBadRequest, nil, true},
		{http.StatusForbidden, nil, true},
		{http.StatusInternalServerError, nil, true},
		{http.StatusServiceUnavailable, nil, true},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var cap capture
			srv := newPushService(t, &cap, func() int { return tc.status })
			s, _ := newTestSender(t, srv.Client())
			rcv := newReceiver(t, srv.URL+"/push/abc")

			err := s.Send(context.Background(), rcv.sub, []byte("hi"), Options{TTL: time.Hour})
			if tc.wantErr == nil && !tc.wantStatus {
				if err != nil {
					t.Fatalf("Send: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Send returned nil for a rejected message")
			}
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("error %v is not a *StatusError", err)
			}
			if se.StatusCode != tc.status {
				t.Errorf("StatusError.StatusCode = %d, want %d", se.StatusCode, tc.status)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false; got %v", tc.wantErr, err)
			}
			for _, other := range []error{ErrGone, ErrRateLimited, ErrTooLarge} {
				if other != tc.wantErr && errors.Is(err, other) {
					t.Errorf("errors.Is(err, %v) is true but should not be", other)
				}
			}
		})
	}
}

// TestSendGoneCarriesBody keeps the diagnostic push services put in the body:
// with Apple, the body is the only place that says why.
func TestSendGoneCarriesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte("BadDeviceToken\n"))
	}))
	t.Cleanup(srv.Close)

	s, _ := newTestSender(t, srv.Client())
	rcv := newReceiver(t, srv.URL+"/push/abc")
	err := s.Send(context.Background(), rcv.sub, []byte("hi"), Options{})
	if !errors.Is(err, ErrGone) {
		t.Fatalf("Send error = %v, want ErrGone", err)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a *StatusError", err)
	}
	if se.Body != "BadDeviceToken" {
		t.Errorf("StatusError.Body = %q, want %q", se.Body, "BadDeviceToken")
	}
	if !strings.Contains(err.Error(), "BadDeviceToken") {
		t.Errorf("Error() = %q, want it to include the service's explanation", err.Error())
	}
}

func TestSendRateLimitedRetryAfter(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"120", 2 * time.Minute},
		{"", 0},
		{"0", 0},
		{"nonsense", 0},
	}
	for _, tc := range tests {
		t.Run("Retry-After: "+tc.header, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)

			s, _ := newTestSender(t, srv.Client())
			rcv := newReceiver(t, srv.URL+"/push/abc")
			err := s.Send(context.Background(), rcv.sub, []byte("hi"), Options{})
			if !errors.Is(err, ErrRateLimited) {
				t.Fatalf("Send error = %v, want ErrRateLimited", err)
			}
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("error %v is not a *StatusError", err)
			}
			if se.RetryAfter != tc.want {
				t.Errorf("RetryAfter = %s, want %s", se.RetryAfter, tc.want)
			}
		})
	}
}

// TestSendRejectsOversizeBeforeNetwork is the point of MaxPayloadBytes: the
// caller finds out deterministically, not from one push service's 413.
func TestSendRejectsOversizeBeforeNetwork(t *testing.T) {
	var cap capture
	srv := newPushService(t, &cap, nil)
	s, _ := newTestSender(t, srv.Client())
	rcv := newReceiver(t, srv.URL+"/push/abc")

	err := s.Send(context.Background(), rcv.sub, bytes.Repeat([]byte("x"), MaxPayloadBytes+1), Options{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Send error = %v, want ErrTooLarge", err)
	}
	if cap.requests != 0 {
		t.Errorf("push service saw %d requests; the payload must be rejected before any network call", cap.requests)
	}

	// One byte less must go through, so the limit is exact rather than merely safe.
	if err := s.Send(context.Background(), rcv.sub, bytes.Repeat([]byte("x"), MaxPayloadBytes), Options{}); err != nil {
		t.Fatalf("Send of exactly MaxPayloadBytes: %v", err)
	}
	if cap.requests != 1 {
		t.Errorf("push service saw %d requests, want 1", cap.requests)
	}
	if len(cap.body) != maxBodyBytes {
		t.Errorf("body was %d bytes, want %d", len(cap.body), maxBodyBytes)
	}
}

func TestSendRejectsBadOptionsBeforeNetwork(t *testing.T) {
	tests := []struct {
		name string
		sub  func(domain.PushSubscription) domain.PushSubscription
		opt  Options
	}{
		{"negative TTL", nil, Options{TTL: -time.Second}},
		{"unknown urgency", nil, Options{Urgency: Urgency("urgent")}},
		{"topic too long", nil, Options{Topic: strings.Repeat("a", 33)}},
		{"topic outside the alphabet", nil, Options{Topic: "digest 2026-07-26"}},
		{"no endpoint", func(s domain.PushSubscription) domain.PushSubscription {
			s.Endpoint = ""
			return s
		}, Options{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cap capture
			srv := newPushService(t, &cap, nil)
			s, _ := newTestSender(t, srv.Client())
			sub := newReceiver(t, srv.URL+"/push/abc").sub
			if tc.sub != nil {
				sub = tc.sub(sub)
			}
			if err := s.Send(context.Background(), sub, []byte("hi"), tc.opt); err == nil {
				t.Fatal("Send accepted invalid options")
			}
			if cap.requests != 0 {
				t.Errorf("push service saw %d requests, want 0", cap.requests)
			}
		})
	}
}

func TestSendContextCancelled(t *testing.T) {
	var cap capture
	srv := newPushService(t, &cap, nil)
	s, _ := newTestSender(t, srv.Client())
	rcv := newReceiver(t, srv.URL+"/push/abc")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Send(ctx, rcv.sub, []byte("hi"), Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send error = %v, want context.Canceled", err)
	}
}

// TestSendDoesNotLogEndpointPath guards the capability in the endpoint URL: the
// path is a bearer token for that device and must not reach an error string.
func TestSendDoesNotLogEndpointPath(t *testing.T) {
	s, _ := newTestSender(t, &http.Client{Timeout: time.Second})
	sub := domain.PushSubscription{
		Endpoint: "https://127.0.0.1:1/push/SUPERSECRETCAPABILITY",
		P256DH:   vecUAPublic,
		Auth:     vecAuthSecret,
	}
	err := s.Send(context.Background(), sub, []byte("hi"), Options{})
	if err == nil {
		t.Fatal("Send to a dead port returned nil")
	}
	if strings.Contains(err.Error(), "SUPERSECRETCAPABILITY") {
		t.Errorf("error leaks the endpoint path: %v", err)
	}
}

func TestSenderPublicKey(t *testing.T) {
	s, key := newTestSender(t, nil)
	if s.PublicKey() != key.Public {
		t.Errorf("PublicKey() = %s, want %s", s.PublicKey(), key.Public)
	}
}
