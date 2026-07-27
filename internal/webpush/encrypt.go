package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"

	"almanack/internal/domain"
)

// Wire shape of the aes128gcm content coding (RFC 8188, section 2.1):
//
//	+-----------+--------+-----------+---------------+-------------+
//	| salt (16) | rs (4) | idlen (1) | keyid (idlen) | record(s)   |
//	+-----------+--------+-----------+---------------+-------------+
//
// For Web Push the keyid is always the application server's ephemeral public key
// (RFC 8291, section 4), so idlen is always 65 and the header is always 86 octets.
// We emit exactly one record, so there is no sequence-number bookkeeping.
const (
	saltLen   = 16
	pointLen  = 65 // uncompressed P-256 point: 0x04 || X(32) || Y(32)
	headerLen = saltLen + 4 + 1 + pointLen
	gcmTagLen = 16

	// padDelimiter marks the end of the plaintext in the final record
	// (RFC 8188, section 2: 0x02 for the last record, 0x01 otherwise).
	padDelimiter = 0x02

	// recordSize is the value written into the rs header field. It is the
	// receiver's promise about the largest record it must buffer.
	recordSize = 4096

	// maxBodyBytes is the request-body ceiling. It is not in any RFC: it is the
	// figure every major push service (FCM, Mozilla autopush, Apple, WNS)
	// converged on, and exceeding it earns a 413.
	maxBodyBytes = 4096

	// authSecretLen is fixed at 16 octets by RFC 8291, section 3.2.
	authSecretLen = 16
)

// MaxPayloadBytes is the largest plaintext a single aes128gcm record can carry
// inside a 4096-octet body: 4096 less the 86-octet header, the one-octet padding
// delimiter and the 16-octet GCM tag. Callers must keep payloads under this;
// Send rejects anything larger before touching the network.
const MaxPayloadBytes = maxBodyBytes - headerLen - 1 - gcmTagLen // 3993

// HKDF info strings. The trailing NUL is part of the string, not a terminator
// Go adds for us — omitting it is a silent interoperability failure, because the
// receiver derives a different key and reports only "decryption failed".
const (
	webPushInfo = "WebPush: info"                   // RFC 8291, section 3.4
	cekInfo     = "Content-Encoding: aes128gcm\x00" // RFC 8188, section 2.2
	nonceInfo   = "Content-Encoding: nonce\x00"     // RFC 8188, section 2.3
)

// derived holds every intermediate value of the RFC 8291 key schedule. The
// intermediates are kept (rather than folded into one expression) because
// RFC 8291 Appendix A publishes each of them, and the vector test asserts them
// one by one: when an interop bug appears, the failing line names the step.
type derived struct {
	prkKey  []byte // PRK_key: HKDF-Extract(salt=auth_secret, ikm=ecdh_secret)
	keyInfo []byte // "WebPush: info" || 0x00 || ua_public || as_public
	ikm     []byte // input keying material for the content encoding
	prk     []byte // PRK: HKDF-Extract(salt=salt, ikm=IKM)
	cek     []byte // content encryption key, 16 octets
	nonce   []byte // nonce for record 0, 12 octets
}

// derive runs the RFC 8291 section 3.4 key combining step and then the RFC 8188
// section 2.2/2.3 content key derivation.
func derive(ecdhSecret, authSecret, salt, uaPub, asPub []byte) (derived, error) {
	var d derived
	var err error

	// The subscription's auth secret is the HKDF salt here. This is what binds
	// the ciphertext to one subscription: an attacker holding only the public
	// p256dh key cannot derive the same keys.
	if d.prkKey, err = hkdf.Extract(sha256.New, ecdhSecret, authSecret); err != nil {
		return derived{}, fmt.Errorf("extract key-combining prk: %w", err)
	}

	d.keyInfo = make([]byte, 0, len(webPushInfo)+1+len(uaPub)+len(asPub))
	d.keyInfo = append(d.keyInfo, webPushInfo...)
	d.keyInfo = append(d.keyInfo, 0x00)
	d.keyInfo = append(d.keyInfo, uaPub...)
	d.keyInfo = append(d.keyInfo, asPub...)

	if d.ikm, err = hkdf.Expand(sha256.New, d.prkKey, string(d.keyInfo), sha256.Size); err != nil {
		return derived{}, fmt.Errorf("expand ikm: %w", err)
	}
	if d.prk, err = hkdf.Extract(sha256.New, d.ikm, salt); err != nil {
		return derived{}, fmt.Errorf("extract content prk: %w", err)
	}
	if d.cek, err = hkdf.Expand(sha256.New, d.prk, cekInfo, 16); err != nil {
		return derived{}, fmt.Errorf("expand cek: %w", err)
	}
	if d.nonce, err = hkdf.Expand(sha256.New, d.prk, nonceInfo, 12); err != nil {
		return derived{}, fmt.Errorf("expand nonce: %w", err)
	}
	return d, nil
}

// Encrypt encrypts payload for sub per RFC 8291, returning a complete aes128gcm
// body (header plus one record) ready to be POSTed as the request entity. A fresh
// ephemeral key pair and salt are generated for every call, as the RFC requires:
// reusing either across messages leaks plaintext relationships.
func Encrypt(sub domain.PushSubscription, payload []byte) ([]byte, error) {
	ephemeral, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webpush: generate ephemeral key: %w", err)
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("webpush: generate salt: %w", err)
	}
	return EncryptWith(sub, payload, salt, ephemeral)
}

// EncryptWith is Encrypt with its two random inputs supplied by the caller. It
// exists so the RFC 8291 Appendix A vector can be reproduced byte for byte;
// production code calls Encrypt. Supplying a salt or ephemeral key that has been
// used before for the same subscription is a real cryptographic break, so this is
// a test seam, not an optimisation.
func EncryptWith(sub domain.PushSubscription, payload, salt []byte, ephemeral *ecdh.PrivateKey) ([]byte, error) {
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("webpush: payload is %d bytes, limit %d: %w", len(payload), MaxPayloadBytes, ErrTooLarge)
	}
	if len(salt) != saltLen {
		return nil, fmt.Errorf("webpush: salt is %d bytes, want %d", len(salt), saltLen)
	}
	if ephemeral == nil {
		return nil, fmt.Errorf("webpush: nil ephemeral key")
	}
	if ephemeral.Curve() != ecdh.P256() {
		return nil, fmt.Errorf("webpush: ephemeral key is not on P-256")
	}

	uaPubRaw, err := decodeKeyMaterial(sub.P256DH)
	if err != nil {
		return nil, fmt.Errorf("webpush: decode p256dh: %w", err)
	}
	// NewPublicKey rejects points that are not on the curve, which RFC 8291
	// section 7 requires of both peers.
	uaPub, err := ecdh.P256().NewPublicKey(uaPubRaw)
	if err != nil {
		return nil, fmt.Errorf("webpush: invalid p256dh key: %w", err)
	}
	authSecret, err := decodeKeyMaterial(sub.Auth)
	if err != nil {
		return nil, fmt.Errorf("webpush: decode auth secret: %w", err)
	}
	if len(authSecret) != authSecretLen {
		return nil, fmt.Errorf("webpush: auth secret is %d bytes, want %d", len(authSecret), authSecretLen)
	}

	ecdhSecret, err := ephemeral.ECDH(uaPub)
	if err != nil {
		return nil, fmt.Errorf("webpush: ecdh: %w", err)
	}
	asPub := ephemeral.PublicKey().Bytes()

	d, err := derive(ecdhSecret, authSecret, salt, uaPub.Bytes(), asPub)
	if err != nil {
		return nil, fmt.Errorf("webpush: %w", err)
	}

	block, err := aes.NewCipher(d.cek)
	if err != nil {
		return nil, fmt.Errorf("webpush: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webpush: gcm: %w", err)
	}

	body := make([]byte, 0, headerLen+len(payload)+1+gcmTagLen)
	body = append(body, salt...)
	body = binary.BigEndian.AppendUint32(body, recordSize)
	body = append(body, byte(len(asPub)))
	body = append(body, asPub...)

	// One record, so the sequence number is 0 and the nonce is used as derived.
	plaintext := make([]byte, 0, len(payload)+1)
	plaintext = append(plaintext, payload...)
	plaintext = append(plaintext, padDelimiter)

	return aead.Seal(body, d.nonce, plaintext, nil), nil
}

// decodeKeyMaterial decodes the base64 forms a subscription's keys arrive in.
// The spec says base64url without padding, and that is what browsers produce,
// but keys reach us through JSON written by client code and by hand-edited
// config, so padded and standard-alphabet variants are accepted rather than
// failing at delivery time with an opaque crypto error.
func decodeKeyMaterial(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty key")
	}
	s = strings.TrimRight(s, "=")
	s = strings.ReplaceAll(s, "+", "-")
	s = strings.ReplaceAll(s, "/", "_")
	return base64.RawURLEncoding.DecodeString(s)
}
