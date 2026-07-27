package domain

import (
	"net/url"
	"strings"
)

// DefaultPushHosts are the push services a browser hands out a subscription
// endpoint for. Between them they cover every browser this application supports.
//
// The list is short because the set of push services is short: a subscription
// endpoint is minted by the user agent, not chosen by the user, and there are
// four vendors running them. A name that is not on it is either a browser nobody
// here has seen yet or an endpoint somebody wrote by hand.
var DefaultPushHosts = []string{
	"fcm.googleapis.com",                // Chrome, Edge, Opera, and everything else on Blink
	"android.googleapis.com",            // older Chrome builds on Android
	"updates.push.services.mozilla.com", // Firefox
	"web.push.apple.com",                // Safari, on macOS and on iOS
	"*.notify.windows.com",              // WNS, whose endpoint host is per-datacentre
}

// PushEndpointAllowed reports whether endpoint is one this server may dial.
//
// A subscription endpoint is the one URL in this application that a member
// supplies and the server then dereferences, which makes it the one place a
// request can be aimed somewhere it was never meant to go — the database on the
// same host, the router, a cloud metadata service. An allowlist is the cheap
// answer and, unusually, also the thorough one: the alternative is a denylist of
// private address ranges, which has to be right about IPv4-mapped addresses,
// unique local addresses, carrier-grade NAT and 0.0.0.0, and which still has to
// resolve the name and pin the result to survive a second lookup. This function
// asks a question with a known answer instead.
//
// allowed is the operator's list (ALMANACK_PUSH_HOSTS). An empty one means
// DefaultPushHosts, so a caller that has not been wired up yet gets the safe
// answer rather than an open door; a single "*" turns the check off, which is
// what a self-hosted push service needs.
//
// Entries are matched against the endpoint's hostname, case-insensitively and
// with any root dot removed. "*.example.org" matches a subdomain and not
// example.org itself. The port is not part of the match: an endpoint is still
// only reached by resolving the allowed name and validating a certificate for it,
// and no port changes that.
func PushEndpointAllowed(endpoint string, allowed []string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	// https is not decoration here. It is what makes a certificate for the
	// allowed name a precondition of the request, which is the half of this that
	// a hijacked resolver cannot get around.
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return false
	}
	if len(allowed) == 0 {
		allowed = DefaultPushHosts
	}
	for _, pattern := range allowed {
		p := strings.ToLower(strings.TrimSpace(pattern))
		switch {
		case p == "*":
			return true
		case strings.HasPrefix(p, "*."):
			// The suffix keeps its dot, so *.notify.windows.com is
			// wns2-by3p.notify.windows.com but never evilnotify.windows.com; the
			// length test is what stops it also matching the bare domain.
			if suffix := p[1:]; len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
				return true
			}
		case p != "" && p == host:
			return true
		}
	}
	return false
}
