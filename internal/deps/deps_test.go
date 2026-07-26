// Package deps holds no code: it exists so that the dependency policy can be a test.
package deps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dependency policy is the load-bearing part of this project's longevity story,
// and a policy that lives only in a document is a policy that a future maintainer —
// human or agent, in a hurry, in 2033 — will break without noticing. So it is a test.
//
// See CONVENTIONS.md §1 and docs/architecture.md §2.

// directAllowlist is the complete set of third-party modules this project may import
// directly. Adding to it is a deliberate architectural decision, not a convenience.
var directAllowlist = map[string]string{
	"modernc.org/sqlite":  "pure-Go SQLite driver: keeps CGO off, which is what makes the binary a single static file",
	"golang.org/x/crypto": "argon2id password hashing only — HKDF comes from stdlib crypto/hkdf",
}

// knownIndirect is the transitive closure that modernc.org/sqlite drags in. It is
// listed explicitly so that an upgrade which pulls in something new fails loudly and
// gets looked at, rather than quietly widening the surface we promise to maintain.
//
// If you upgraded a dependency on purpose and this test fails, read the new module's
// provenance, then update this list in the same commit.
var knownIndirect = map[string]bool{
	"github.com/dustin/go-humanize":    true,
	"github.com/google/uuid":           true,
	"github.com/mattn/go-isatty":       true,
	"github.com/ncruces/go-strftime":   true,
	"github.com/remyoudompheng/bigfft": true,
	"golang.org/x/crypto":              true,
	"golang.org/x/sys":                 true,
	"modernc.org/libc":                 true,
	"modernc.org/mathutil":             true,
	"modernc.org/memory":               true,
	"modernc.org/sqlite":               true,
}

func TestDependencyPolicy(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	direct, indirect := parseRequires(string(data))

	for _, mod := range direct {
		if _, ok := directAllowlist[mod]; !ok {
			t.Errorf("go.mod requires %q directly, which is not on the allowlist.\n"+
				"This project deliberately depends on two modules only (CONVENTIONS.md §1).\n"+
				"Either write the code by hand, or make adding this dependency an explicit,\n"+
				"documented decision and extend directAllowlist in the same commit.", mod)
		}
	}

	for _, mod := range indirect {
		if !knownIndirect[mod] {
			t.Errorf("go.mod pulls in a new indirect dependency %q.\n"+
				"Nothing is wrong with that per se, but the transitive closure is part of what\n"+
				"this project promises to keep building in 2040, so it gets reviewed: check the\n"+
				"module's provenance and add it to knownIndirect in the same commit.", mod)
		}
	}
}

// TestNoFrontendBuildTooling guards the other half of the policy: the browser code
// has zero dependencies and no build step. A package.json appearing anywhere outside
// the dev-only e2e directory means someone reached for npm.
func TestNoFrontendBuildTooling(t *testing.T) {
	forbidden := []string{
		"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml",
		"web/package.json", "web/node_modules",
		"vite.config.js", "webpack.config.js", "rollup.config.js", "tsconfig.json",
	}
	for _, path := range forbidden {
		if _, err := os.Stat(filepath.Join("..", "..", path)); err == nil {
			t.Errorf("%s exists: the frontend is hand-written with no toolchain (CONVENTIONS.md §1).\n"+
				"The only build in this project is `go build`.", path)
		}
	}
}

// parseRequires splits go.mod's require entries into direct and indirect module paths.
func parseRequires(mod string) (direct, indirect []string) {
	inBlock := false
	for _, line := range strings.Split(mod, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "//"), trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "require ("):
			inBlock = true
			continue
		case inBlock && trimmed == ")":
			inBlock = false
			continue
		}

		entry := ""
		if inBlock {
			entry = trimmed
		} else if strings.HasPrefix(trimmed, "require ") {
			entry = strings.TrimPrefix(trimmed, "require ")
		}
		if entry == "" {
			continue
		}

		isIndirect := strings.Contains(entry, "// indirect")
		if i := strings.Index(entry, "//"); i >= 0 {
			entry = entry[:i]
		}
		fields := strings.Fields(entry)
		if len(fields) < 2 {
			continue
		}
		if isIndirect {
			indirect = append(indirect, fields[0])
		} else {
			direct = append(direct, fields[0])
		}
	}
	return direct, indirect
}
