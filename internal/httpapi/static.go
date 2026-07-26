package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"

	"agenda/internal/domain"
)

// appVersionPlaceholder is substituted in sw.js as it is served. The service worker
// needs the build hash to name its cache, and the alternative — a build step that
// rewrites the file — is exactly the toolchain this project refuses to have.
const appVersionPlaceholder = "__APP_VERSION__"

// asset is one preloaded file from the embedded web tree. The whole tree is a few
// hundred kilobytes, so holding it in memory buys per-file ETags, a single substitution
// pass over sw.js, and serving with no filesystem work at all.
type asset struct {
	data        []byte
	contentType string
	etag        string
	cache       string
}

// loadAssets reads every embedded web file and computes the application version: a
// short hash over all of them, path and content alike, so that touching any asset
// changes the version the client compares against.
func loadAssets(fsys fs.FS) (map[string]asset, string, error) {
	assets := map[string]asset{}
	if fsys == nil {
		return assets, shortHash(sha256.Sum256(nil)), nil
	}

	var names []string
	contents := map[string][]byte{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		names = append(names, p)
		contents[p] = data
		return nil
	})
	if err != nil {
		return nil, "", err
	}

	// Hash in a stable order, with lengths, so that neither directory iteration order
	// nor a rename-only change can collide with a different tree.
	sort.Strings(names)
	sum := sha256.New()
	var length [8]byte
	for _, name := range names {
		sum.Write([]byte(name))
		binary.BigEndian.PutUint64(length[:], uint64(len(contents[name])))
		sum.Write(length[:])
		sum.Write(contents[name])
	}
	version := shortHash([32]byte(sum.Sum(nil)))

	for _, name := range names {
		data := contents[name]
		cache := "public, max-age=600"
		switch name {
		case "sw.js":
			// A cached service worker that can never be replaced is unrecoverable on
			// somebody's phone, so it is always revalidated — as is the shell that
			// registers it.
			data = bytes.ReplaceAll(data, []byte(appVersionPlaceholder), []byte(version))
			cache = "no-cache"
		case "index.html":
			cache = "no-cache"
		}
		assets[name] = asset{
			data:        data,
			contentType: contentTypeFor(name),
			etag:        `"` + shortHash(sha256.Sum256(data)) + `"`,
			cache:       cache,
		}
	}
	return assets, version, nil
}

func shortHash(sum [32]byte) string { return hex.EncodeToString(sum[:4]) }

// contentTypeFor maps an extension to a media type. The table is explicit rather than
// mime.TypeByExtension because that consults /etc/mime.types, and a server whose
// JavaScript is served as text/plain because a system file changed is a bad afternoon.
func contentTypeFor(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".webmanifest":
		return "application/manifest+json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/vnd.microsoft.icon"
	case ".woff2":
		return "font/woff2"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".map":
		return "application/json; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// serveStatic serves the PWA, falling back to index.html for client-side routes.
//
// The fallback never applies to the reserved prefixes: a fetch of a mistyped API path
// must fail as an API call, not come back as a 200 full of HTML that the client then
// tries to parse as JSON.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, r, http.StatusNotFound, codeNotFound, "not found")
		return
	}
	if reservedPath(r.URL.Path) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "not found")
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	if a, ok := s.assets[name]; ok {
		s.writeAsset(w, r, a)
		return
	}
	// Client-side routes (/join/…, /reset/…, /#/…) are served the shell. A missing
	// asset with an extension is a genuine 404: answering script requests with HTML is
	// how a deploy bug turns into a blank page and a confusing console error.
	if index, ok := s.assets["index.html"]; ok && wantsHTML(r, name) {
		s.writeAsset(w, r, index)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func wantsHTML(r *http.Request, name string) bool {
	if path.Ext(name) == "" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// reservedPath reports whether a path belongs to the server rather than to the PWA.
func reservedPath(p string) bool {
	for _, prefix := range []string{"/api", "/locales", "/dev"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return p == "/healthz"
}

func (s *Server) writeAsset(w http.ResponseWriter, r *http.Request, a asset) {
	h := w.Header()
	h.Set("Content-Type", a.contentType)
	h.Set("Cache-Control", a.cache)
	h.Set("ETag", a.etag)
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(a.data)
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// handleLocale serves the shared translation catalogs at /locales/{lang}.json.
//
// The browser and the server read the same files — the catalog's FS is rooted at the
// files themselves, so the /locales/ prefix is stripped here — which is what stops a
// string from being right in a notification and stale in the UI.
func (s *Server) handleLocale(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	lang := domain.Language(strings.TrimSuffix(name, ".json"))
	if !strings.HasSuffix(name, ".json") || !lang.Valid() {
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such locale")
		return
	}
	data, err := fs.ReadFile(s.catalog.FS(), string(lang)+".json")
	if err != nil {
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such locale")
		return
	}
	s.writeAsset(w, r, asset{
		data:        data,
		contentType: "application/json; charset=utf-8",
		etag:        `"` + shortHash(sha256.Sum256(data)) + `"`,
		cache:       "no-cache",
	})
}
