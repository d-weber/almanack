package main

import (
	"strings"
	"testing"
)

// The bug this guards against was silent: `almanack serve --config /etc/almanack.conf`
// started the server on a different configuration than the one named on the command
// line, because Go's flag package stops at the first non-flag argument and nothing
// looked at what was left over.
func TestTakeConfigFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		path string
		rest []string
	}{
		{"absent", []string{"--force"}, "", []string{"--force"}},
		{"separate value", []string{"--config", "/etc/a.conf"}, "/etc/a.conf", []string{}},
		{"equals value", []string{"--config=/etc/a.conf"}, "/etc/a.conf", []string{}},
		{"single dash", []string{"-config", "/etc/a.conf"}, "/etc/a.conf", []string{}},
		{"single dash equals", []string{"-config=/etc/a.conf"}, "/etc/a.conf", []string{}},
		// The command's own arguments must survive untouched, and in order: `backup`
		// reads a positional directory and `seed` reads --force out of what is left.
		{"keeps the rest", []string{"/srv/snapshots", "--config", "/etc/a.conf", "--prune"},
			"/etc/a.conf", []string{"/srv/snapshots", "--prune"}},
		{"empty value is still a value", []string{"--config="}, "", []string{}},
		{"a path may look like a flag value", []string{"--config", "--force"}, "--force", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, rest, err := takeConfigFlag(tc.args)
			if err != nil {
				t.Fatalf("takeConfigFlag(%q) returned %v", tc.args, err)
			}
			if path != tc.path {
				t.Errorf("path = %q, want %q", path, tc.path)
			}
			if strings.Join(rest, "\x00") != strings.Join(tc.rest, "\x00") {
				t.Errorf("rest = %q, want %q", rest, tc.rest)
			}
		})
	}
}

// A --config with nothing after it must fail loudly. Treating it as absent is how the
// original bug behaved, and it is the one outcome that must not come back.
func TestTakeConfigFlagRejectsMissingPath(t *testing.T) {
	if _, _, err := takeConfigFlag([]string{"seed", "--config"}); err == nil {
		t.Fatal("takeConfigFlag accepted a --config with no path")
	}
}
