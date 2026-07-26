// Package webpush delivers Web Push messages, implemented from the specifications
// with nothing but the standard library.
//
// Three RFCs meet here:
//
//   - RFC 8030 is the delivery protocol: POST the encrypted body to the
//     subscription endpoint with TTL, Urgency and optionally Topic.
//   - RFC 8291 is the payload encryption: ECDH on P-256 between a per-message
//     ephemeral key and the subscription's p256dh key, mixed with the
//     subscription's auth secret, feeding the aes128gcm content coding of
//     RFC 8188. See encrypt.go.
//   - RFC 8292 is VAPID: an ES256 JWT identifying this server to the push
//     service. See vapid.go.
//
// Nothing here reads the clock directly; a Sender takes a clock.Clock so the
// token's expiry claim is testable and so dev-mode time travel does not produce
// tokens from the future.
//
// Field notes that cost real debugging time, and that a future maintainer should
// not have to rediscover:
//
//   - The VAPID signature must be raw R||S, never ASN.1/DER (see signES256).
//   - The TTL header is mandatory. Mozilla's autopush answers 400 without it.
//   - Urgency is optional in RFC 8030 but is always sent here: Apple's push
//     service has been observed to reject messages that omit it.
//   - A 404 or 410 means the subscription is dead and must be deleted. A 410
//     will otherwise repeat forever, and the failure counter never catches it
//     because the endpoint answers promptly every time.
//   - Success is not proof of delivery. iOS in particular keeps returning 201
//     for subscriptions that will never display anything again, which is why the
//     app also tracks last_confirmed_at from the client side.
package webpush

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"agenda/internal/clock"
	"agenda/internal/domain"
)

// Sentinel errors. Callers use errors.Is on these; the concrete error returned by
// Send for an HTTP failure is always a *StatusError, which wraps the matching
// sentinel where one applies.
var (
	// ErrGone means the push service has forgotten this subscription (404 or
	// 410). The caller must delete the row: it will never work again.
	ErrGone = errors.New("webpush: subscription gone")

	// ErrTooLarge means the payload exceeds MaxPayloadBytes. Send reports this
	// before making a request; a push service reporting 413 also maps to it.
	ErrTooLarge = errors.New("webpush: payload too large")

	// ErrRateLimited means the push service returned 429. StatusError.RetryAfter
	// carries the service's hint when it sent one.
	ErrRateLimited = errors.New("webpush: rate limited")
)

// StatusError is a delivery rejected by the push service. The body is kept
// because push services put the only useful diagnostic there (Apple's
// "BadDeviceToken", FCM's JSON error object) while the status code alone is
// ambiguous.
type StatusError struct {
	StatusCode int
	Body       string
	// RetryAfter is the parsed Retry-After header, or zero if absent.
	RetryAfter time.Duration
	// Err is the sentinel this status maps to, or nil for statuses with no
	// specific meaning.
	Err error
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("webpush: push service returned %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Unwrap exposes the sentinel so errors.Is(err, ErrGone) works on the returned
// *StatusError.
func (e *StatusError) Unwrap() error { return e.Err }

// Urgency is the RFC 8030 section 5.3 urgency of a message. The push service uses
// it to decide whether waking a battery-saving device is worth it.
type Urgency string

const (
	// UrgencyVeryLow is for messages that may wait for the device to be plugged
	// in and on wifi.
	UrgencyVeryLow Urgency = "very-low"
	// UrgencyLow is for messages that may wait for the device to be plugged in
	// or on wifi.
	UrgencyLow Urgency = "low"
	// UrgencyNormal is the default: deliver whenever the device is on.
	UrgencyNormal Urgency = "normal"
	// UrgencyHigh is for time-critical messages, such as a reminder for an event
	// that is about to start.
	UrgencyHigh Urgency = "high"
)

func (u Urgency) valid() bool {
	switch u {
	case UrgencyVeryLow, UrgencyLow, UrgencyNormal, UrgencyHigh:
		return true
	}
	return false
}

// DefaultTTL is used when Options.TTL is zero. It is deliberately not zero
// seconds: TTL: 0 tells the push service to drop the message unless the device
// is connected at that instant, which is never what a calendar reminder wants.
const DefaultTTL = 6 * time.Hour

// maxTopicLen is the RFC 8030 section 5.4 limit on a Topic.
const maxTopicLen = 32

// Options are the per-message delivery knobs.
type Options struct {
	// TTL is how long the push service should hold the message for a device
	// that is offline. Zero means DefaultTTL; negative is an error.
	TTL time.Duration
	// Urgency defaults to UrgencyNormal when empty.
	Urgency Urgency
	// Topic, when set, replaces any undelivered message with the same topic for
	// the same subscription. Use it so a superseded daily digest collapses
	// instead of stacking. At most 32 characters from the URL-safe base64
	// alphabet.
	Topic string
}

// Sender delivers push messages for one VAPID identity. It is safe for
// concurrent use and is meant to be created once at startup.
type Sender struct {
	// Clock is the source of "now" for the VAPID expiry claim. NewSender sets
	// it to clock.Real; replace it before first use, not during.
	Clock clock.Clock

	priv      *ecdsa.PrivateKey
	publicB64 string
	subject   string
	hc        *http.Client
}

// NewSender builds a Sender from the configured VAPID key pair (base64url, as
// produced by GenerateKeys) and a contact subject — a mailto: or https: URI that
// a push service operator can use to reach whoever runs this server. Passing a
// nil client installs one with a timeout, because a push service that never
// answers must not wedge the scheduler goroutine.
func NewSender(vapidPublicB64, vapidPrivateB64, subject string, hc *http.Client) (*Sender, error) {
	priv, err := parseKey(vapidPublicB64, vapidPrivateB64)
	if err != nil {
		return nil, fmt.Errorf("webpush: vapid keys: %w", err)
	}
	subject = strings.TrimSpace(subject)
	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https://") {
		return nil, fmt.Errorf("webpush: vapid subject %q must be a mailto: or https: URI", subject)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Sender{
		Clock:     clock.Real{},
		priv:      priv,
		publicB64: strings.TrimRight(strings.TrimSpace(vapidPublicB64), "="),
		subject:   subject,
		hc:        hc,
	}, nil
}

// PublicKey returns the base64url VAPID public key this Sender signs with. The
// client needs it as applicationServerKey when subscribing.
func (s *Sender) PublicKey() string { return s.publicB64 }

// Send encrypts payload for sub and delivers it. It returns nil only when the
// push service accepted the message; acceptance is not delivery, and delivery is
// not display (see the package comment).
//
// Oversized payloads are rejected before any network call, so a caller that
// builds too large a digest learns about it deterministically rather than as an
// occasional 413 from one particular push service.
func (s *Sender) Send(ctx context.Context, sub domain.PushSubscription, payload []byte, opt Options) error {
	if len(payload) > MaxPayloadBytes {
		return fmt.Errorf("webpush: payload is %d bytes, limit %d: %w", len(payload), MaxPayloadBytes, ErrTooLarge)
	}
	if sub.Endpoint == "" {
		return fmt.Errorf("webpush: subscription %d has no endpoint", sub.ID)
	}
	if opt.TTL < 0 {
		return fmt.Errorf("webpush: negative TTL %s", opt.TTL)
	}
	urgency := opt.Urgency
	if urgency == "" {
		urgency = UrgencyNormal
	}
	if !urgency.valid() {
		return fmt.Errorf("webpush: unknown urgency %q", opt.Urgency)
	}
	if err := validTopic(opt.Topic); err != nil {
		return err
	}

	// Sign first: it also parses the endpoint, so a malformed one fails before
	// the ECDH and the AES work.
	auth, err := s.authorization(sub.Endpoint, s.Clock.Now())
	if err != nil {
		return fmt.Errorf("webpush: %w", err)
	}
	body, err := Encrypt(sub, payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webpush: build request: %w", err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.FormatInt(ttlSeconds(opt.TTL), 10))
	req.Header.Set("Urgency", string(urgency))
	if opt.Topic != "" {
		req.Header.Set("Topic", opt.Topic)
	}
	req.Header.Set("Authorization", auth)

	resp, err := s.hc.Do(req)
	if err != nil {
		// *url.Error stringifies the whole endpoint, and the endpoint's path is
		// a bearer capability for that device. Keep the cause, drop the URL.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("webpush: post %s: %w", endpointForLog(sub.Endpoint), err)
	}
	defer resp.Body.Close()
	// Read (a bounded amount of) the body so the connection can be reused, and
	// because the body is where push services explain themselves.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	e := &StatusError{
		StatusCode: resp.StatusCode,
		Body:       strings.TrimSpace(string(respBody)),
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), s.Clock.Now()),
	}
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		// 404: the endpoint never existed or was garbage collected.
		// 410: the user agent unsubscribed. Both are terminal.
		e.Err = ErrGone
	case http.StatusRequestEntityTooLarge:
		e.Err = ErrTooLarge
	case http.StatusTooManyRequests:
		e.Err = ErrRateLimited
	}
	return e
}

// ttlSeconds converts a duration to the integer seconds the TTL header carries,
// rounding up so that a sub-second TTL does not become "deliver now or discard".
func ttlSeconds(d time.Duration) int64 {
	if d <= 0 {
		d = DefaultTTL
	}
	return int64((d + time.Second - 1) / time.Second)
}

// validTopic enforces the RFC 8030 section 5.4 shape. Push services answer 400
// for a bad topic, which is a confusing way to learn that a title got used as a
// collapse key.
func validTopic(topic string) error {
	if topic == "" {
		return nil
	}
	if len(topic) > maxTopicLen {
		return fmt.Errorf("webpush: topic %q is %d characters, limit %d", topic, len(topic), maxTopicLen)
	}
	for _, r := range topic {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("webpush: topic %q contains %q, which is not in the URL-safe base64 alphabet", topic, r)
		}
	}
	return nil
}

// retryAfter parses the two legal forms of the header: delta-seconds, or an
// HTTP-date. An unparseable or past value yields zero, meaning "no hint".
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// endpointForLog trims the endpoint to its origin. The path segment is a bearer
// capability for that device: anyone holding it can push to it, so it must not
// reach a log line.
func endpointForLog(endpoint string) string {
	if o, err := origin(endpoint); err == nil {
		return o
	}
	return "push endpoint"
}
