package domain

import (
	"strings"
	"testing"
)

// The allowlist is the one place a member-supplied URL is checked before this
// server dereferences it, so the cases here are the attack shapes rather than a
// sample: the private address, the name that only looks like a push service, the
// userinfo trick, and the scheme downgrade.
func TestPushEndpointAllowed(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		allowed  []string
		want     bool
	}{
		// The real thing, one per browser this application supports.
		{"fcm", "https://fcm.googleapis.com/fcm/send/abc123", nil, true},
		{"android", "https://android.googleapis.com/gcm/send/abc123", nil, true},
		{"mozilla", "https://updates.push.services.mozilla.com/wpush/v2/gAAAAA", nil, true},
		{"apple", "https://web.push.apple.com/QMLDZ3xc", nil, true},
		{"wns subdomain", "https://wns2-by3p.notify.windows.com/w/?token=Ab", nil, true},

		// Hostnames are case-insensitive; a browser that hands one back in mixed
		// case must not lose its notifications for it.
		{"mixed case", "https://FCM.GoogleAPIs.Com/fcm/send/abc", nil, true},
		{"fully qualified with a root dot", "https://fcm.googleapis.com./fcm/send/abc", nil, true},
		// The port says nothing about who answers: the name still has to resolve to
		// the push service and still has to present a certificate for it.
		{"explicit port", "https://fcm.googleapis.com:443/fcm/send/abc", nil, true},

		// A wildcard covers subdomains and only subdomains.
		{"wildcard does not cover the bare domain", "https://notify.windows.com/w/", nil, false},
		{"wildcard does not cover a suffix match", "https://evilnotify.windows.com/w/", nil, false},

		// The shapes that make this check worth having.
		{"private address", "https://10.0.0.5:9200/_search", nil, false},
		{"loopback", "https://127.0.0.1/push", nil, false},
		{"loopback by name", "https://localhost/push", nil, false},
		{"link-local metadata", "https://169.254.169.254/latest/meta-data/", nil, false},
		{"somewhere else entirely", "https://attacker.example.org/push/abc", nil, false},
		// url.Parse reads everything before the @ as userinfo, so this is a request
		// to 127.0.0.1 wearing a push service's name.
		{"userinfo impersonating a push service", "https://fcm.googleapis.com@127.0.0.1/push", nil, false},
		{"suffix that only looks right", "https://fcm.googleapis.com.evil.example.org/push", nil, false},
		{"prefix that only looks right", "https://fcm.googleapis.com.internal/push", nil, false},

		// Everything that is not an https URL at all.
		{"plaintext", "http://fcm.googleapis.com/fcm/send/abc", nil, false},
		{"no scheme", "fcm.googleapis.com/fcm/send/abc", nil, false},
		{"not a URL", "://", nil, false},
		{"empty", "", nil, false},
		{"scheme only", "https://", nil, false},

		// The operator's overrides. An empty list means the defaults — a caller that
		// forgets to pass one gets the safe answer, not an open door.
		{"configured host", "https://push.example.org/abc", []string{"push.example.org"}, true},
		{"configured list does not include the defaults", "https://fcm.googleapis.com/fcm/send/abc",
			[]string{"push.example.org"}, false},
		{"configured wildcard", "https://a.push.example.org/abc", []string{"*.push.example.org"}, true},
		{"star disables the check", "https://10.0.0.5:9200/_search", []string{"*"}, true},
		{"star among others", "https://10.0.0.5:9200/_search", []string{"push.example.org", "*"}, true},
		{"star still requires https", "http://10.0.0.5:9200/_search", []string{"*"}, false},
		{"whitespace and case in the configured list", "https://PUSH.example.org/abc",
			[]string{" Push.Example.Org "}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PushEndpointAllowed(tc.endpoint, tc.allowed); got != tc.want {
				t.Errorf("PushEndpointAllowed(%q, %v) = %v, want %v", tc.endpoint, tc.allowed, got, tc.want)
			}
		})
	}
}

// The default list is what a family's phones actually register against. An entry
// that is not a hostname would silently switch push off for that browser, and the
// symptom — notifications that simply never arrive — is the hardest kind to trace.
func TestDefaultPushHostsAreHostnames(t *testing.T) {
	for _, host := range DefaultPushHosts {
		if host == "" || host == "*" {
			t.Errorf("DefaultPushHosts contains %q, which would disable the check", host)
		}
		if strings.ContainsAny(host, "/: ") {
			t.Errorf("DefaultPushHosts entry %q is not a bare hostname", host)
		}
		if !PushEndpointAllowed("https://"+strings.Replace(host, "*.", "sub.", 1)+"/push/abc", nil) {
			t.Errorf("DefaultPushHosts entry %q does not match an endpoint of its own", host)
		}
	}
}
