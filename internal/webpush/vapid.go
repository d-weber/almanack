package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TokenTTL is how long a VAPID token stays valid. RFC 8292, section 2 caps the
// "exp" claim at 24 hours from the time of the request; half of that leaves room
// for a slow request and for a push service whose clock is minutes ahead of ours.
const TokenTTL = 12 * time.Hour

// jwtHeader is the fixed JOSE header, written as a literal rather than marshalled
// so that its bytes — and therefore the signing input — can never depend on map
// ordering. RFC 8292 permits only ES256.
const jwtHeader = `{"typ":"JWT","alg":"ES256"}`

// Key is a VAPID application server key pair: ECDSA on P-256, used with ES256.
// The encodings are the ones the Web Push ecosystem uses everywhere — the public
// key is the uncompressed point that the browser passes as applicationServerKey,
// the private key is the bare 32-octet scalar — both base64url without padding.
type Key struct {
	// Public is the 65-octet uncompressed point, base64url unpadded.
	Public string
	// Private is the 32-octet private scalar, base64url unpadded.
	Private string
}

// GenerateKey returns a fresh VAPID key pair.
func GenerateKey() (Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Key{}, fmt.Errorf("webpush: generate vapid key: %w", err)
	}
	pubBytes, err := priv.PublicKey.Bytes()
	if err != nil {
		return Key{}, fmt.Errorf("webpush: encode vapid public key: %w", err)
	}
	privBytes, err := priv.Bytes()
	if err != nil {
		return Key{}, fmt.Errorf("webpush: encode vapid private key: %w", err)
	}
	return Key{
		Public:  base64.RawURLEncoding.EncodeToString(pubBytes),
		Private: base64.RawURLEncoding.EncodeToString(privBytes),
	}, nil
}

// GenerateKeys returns a fresh VAPID key pair as two base64url strings, in the
// shape the `almanack gen-vapid` subcommand prints for the operator to paste into
// the environment file. A deployment generates these once and never rotates them
// casually: changing them invalidates every existing subscription.
func GenerateKeys() (publicB64, privateB64 string, err error) {
	k, err := GenerateKey()
	if err != nil {
		return "", "", err
	}
	return k.Public, k.Private, nil
}

// parseKey turns the configured base64url pair into a usable signing key, and
// checks that the two halves actually belong together. That check is worth its
// three lines: a public/private mismatch in the environment file otherwise shows
// up only as every push service returning 403.
func parseKey(publicB64, privateB64 string) (*ecdsa.PrivateKey, error) {
	pubBytes, err := decodeKeyMaterial(publicB64)
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if _, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubBytes); err != nil {
		return nil, fmt.Errorf("invalid public key: %w", err)
	}
	privBytes, err := decodeKeyMaterial(privateB64)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), privBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	derivedPub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode derived public key: %w", err)
	}
	if string(derivedPub) != string(pubBytes) {
		return nil, fmt.Errorf("public key does not match private key")
	}
	return priv, nil
}

// vapidClaims is the JWT body. The field order is the marshalled order, which
// only matters in that it must be stable between signing and transmission.
type vapidClaims struct {
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Sub string `json:"sub"`
}

// authorization builds the RFC 8292 section 3 Authorization header field value
// for one delivery: `vapid t=<jwt>,k=<public key>`.
func (s *Sender) authorization(endpoint string, now time.Time) (string, error) {
	aud, err := origin(endpoint)
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(vapidClaims{
		Aud: aud,
		Exp: now.Add(TokenTTL).Unix(),
		Sub: s.subject,
	})
	if err != nil {
		return "", fmt.Errorf("marshal vapid claims: %w", err)
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString([]byte(jwtHeader)) + "." + enc.EncodeToString(claims)
	sig, err := signES256(s.priv, []byte(signingInput))
	if err != nil {
		return "", err
	}
	jwt := signingInput + "." + enc.EncodeToString(sig)
	return "vapid t=" + jwt + ",k=" + s.publicB64, nil
}

// signES256 produces a JWS ES256 signature: the raw 64-octet R||S concatenation,
// each half left-padded to the 32-octet coordinate size.
//
// This is the bug that eats a week. ecdsa.SignASN1 (and every "just sign it"
// helper) emits a DER SEQUENCE, which is a valid ECDSA signature and completely
// invalid as JWS: Mozilla's autopush and Apple's push service reject the token
// outright, and some services accept it just often enough to look intermittent.
// JWS (RFC 7515, section 3.4 and RFC 7518, section 3.4) mandates fixed-width
// R||S, so we build it by hand and never touch the ASN.1 encoder.
func signES256(priv *ecdsa.PrivateKey, signingInput []byte) ([]byte, error) {
	digest := sha256.Sum256(signingInput)
	r, sInt, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign vapid token: %w", err)
	}
	const coordLen = 32 // P-256 field size
	sig := make([]byte, 2*coordLen)
	r.FillBytes(sig[:coordLen])
	sInt.FillBytes(sig[coordLen:])
	return sig, nil
}

// origin returns the Unicode serialization of the push resource's origin
// (RFC 6454, section 6.1), which is what the "aud" claim must contain: scheme,
// host, and port only when it is not the scheme's default. A token is reusable
// for every endpoint sharing an origin, which is why the claim is not the full
// endpoint URL.
func origin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse endpoint: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("endpoint scheme %q is not http(s)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("endpoint has no host")
	}
	host := u.Host
	if port := u.Port(); (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		host = u.Hostname()
		if strings.Contains(host, ":") { // IPv6 literal, put the brackets back
			host = "[" + host + "]"
		}
	}
	return scheme + "://" + host, nil
}
