package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"almanack/internal/clock"
)

// Rate limits for the three unauthenticated endpoints that are worth guessing at. The
// buckets are in memory and therefore reset when the process restarts: at family scale,
// with 256-bit session tokens and argon2id, that is an accepted trade rather than a
// reason to add a table and a cleanup job.
var rateLimits = map[string]struct {
	burst  int
	refill time.Duration // time to earn one token back
}{
	"login":  {burst: 8, refill: 20 * time.Second},
	"signup": {burst: 5, refill: 2 * time.Minute},
	"reset":  {burst: 5, refill: 2 * time.Minute},
}

// bucketIdleTTL is how long an untouched bucket is kept before the next sweep drops it.
const bucketIdleTTL = time.Hour

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// limiterSet is a token bucket per (endpoint, client). It reads the injected clock, so
// dev-mode time travel and tests move the buckets forward without sleeping.
type limiterSet struct {
	clk clock.Clock

	mu       sync.Mutex
	buckets  map[string]*bucket
	lastGC   time.Time
	gcPeriod time.Duration
}

func newLimiterSet(clk clock.Clock) *limiterSet {
	return &limiterSet{clk: clk, buckets: map[string]*bucket{}, gcPeriod: 10 * time.Minute}
}

// allow takes one token for client on the named endpoint, reporting whether there was
// one to take.
func (l *limiterSet) allow(name, client string) bool {
	limit, ok := rateLimits[name]
	if !ok {
		return true
	}
	now := l.clk.Now()
	key := name + "|" + client

	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(limit.burst), lastSeen: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.lastSeen); elapsed > 0 {
		b.tokens += elapsed.Seconds() / limit.refill.Seconds()
		if b.tokens > float64(limit.burst) {
			b.tokens = float64(limit.burst)
		}
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// gc drops buckets nobody has touched in a while. Called under the lock.
func (l *limiterSet) gc(now time.Time) {
	if now.Sub(l.lastGC) < l.gcPeriod {
		return
	}
	l.lastGC = now
	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) > bucketIdleTTL {
			delete(l.buckets, key)
		}
	}
}

// rateLimit consumes a token for this request, answering 429 when there is none.
func (s *Server) rateLimit(w http.ResponseWriter, r *http.Request, name string) bool {
	if s.limiter.allow(name, s.clientIP(r)) {
		return true
	}
	writeError(w, r, http.StatusTooManyRequests, codeRateLimited, "too many attempts; please wait")
	return false
}

// trustedPeer is a configured proxy address: a single IP or a CIDR block.
type trustedPeer struct {
	ip  net.IP
	net *net.IPNet
}

func parseTrustedPeers(list []string) []trustedPeer {
	var out []trustedPeer
	for _, entry := range list {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, cidr, err := net.ParseCIDR(entry); err == nil {
			out = append(out, trustedPeer{net: cidr})
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			out = append(out, trustedPeer{ip: ip})
		}
	}
	return out
}

func (s *Server) trusts(ip net.IP) bool {
	for _, p := range s.proxies {
		switch {
		case p.net != nil && p.net.Contains(ip):
			return true
		case p.ip != nil && p.ip.Equal(ip):
			return true
		}
	}
	return false
}

// clientIP identifies the client for rate limiting.
//
// X-Forwarded-For is believed only when the socket peer is a configured trusted proxy,
// and then only its rightmost entry — the one that proxy appended, which is the address
// it actually accepted the connection from. Anything further left was supplied by the
// client and is a lie waiting to happen. Getting this wrong in either direction is a
// real failure: trust it always and one attacker locks out the whole family by spoofing
// their addresses; trust it never and, behind nginx, every request shares one bucket and
// the first failed login locks out everyone.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !s.trusts(peer) {
		return host
	}
	forwarded := r.Header.Get("X-Forwarded-For")
	parts := strings.Split(forwarded, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		if ip := net.ParseIP(candidate); ip != nil {
			return ip.String()
		}
	}
	return host
}
