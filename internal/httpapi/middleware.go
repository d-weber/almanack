package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// maxRequestBytes bounds every request body. An avatar is the only large thing the API
// accepts and the client resizes it to well under 1 MB first; 2 MB leaves room for an
// old client without letting an unauthenticated request allocate anything interesting.
const maxRequestBytes = 2 << 20

// csrfHeader is the header every mutation must carry. A cross-origin form or image tag
// cannot set it, and SameSite=Lax already keeps the cookie off cross-site navigations,
// which is why this design needs no token. See CONVENTIONS.md §5.
const (
	csrfHeader = "X-Requested-With"
	csrfValue  = "almanack"
)

type ctxKey int

const (
	ctxKeyUser ctxKey = iota
	ctxKeyInfo
)

// reqInfo is the per-request scratch space the logger reads after the handler has run.
// The user id lands here when a session is resolved, so one log line can carry who did
// what without every handler having to log for itself.
type reqInfo struct {
	userID int64
}

func infoOf(ctx context.Context) *reqInfo {
	if v, ok := ctx.Value(ctxKeyInfo).(*reqInfo); ok {
		return v
	}
	return &reqInfo{}
}

// statusWriter records the status and size of a response for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer (flushing, deadlines).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recoverer turns a panic into a 500 instead of a dropped connection. Library code in
// this project never panics on purpose (CONVENTIONS.md §3); this exists so that the one
// that slips through takes down a request rather than the family's calendar server.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v) // the stdlib's own "stop, quietly" signal
				}
				slog.Error("panic serving request",
					"method", r.Method, "path", redactPath(r.URL.Path),
					"panic", v, "stack", string(debug.Stack()))
				writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestLogger writes one structured line per request.
//
// It logs the path, never the query string and never the raw path of a token-bearing
// route: an invite or password-reset token in a log file is a credential in a log file.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &reqInfo{}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyInfo, info))

		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		took := time.Since(start)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		level := slog.LevelInfo
		switch {
		case sw.status >= 500:
			level = slog.LevelError
		case sw.status >= 400:
			level = slog.LevelWarn
		}
		slog.LogAttrs(r.Context(), level, "http",
			slog.String("method", r.Method),
			slog.String("path", redactPath(r.URL.Path)),
			slog.Int("status", sw.status),
			slog.Int64("duration_ms", took.Milliseconds()),
			slog.Int64("user", info.userID),
		)
	})
}

// redactPath removes secrets that live in path segments. The invite preview and the two
// client-side token routes are the only ones, and they are exactly the ones worth
// keeping out of journald forever.
//
// Numeric ids are kept: /api/v1/invites/12/revoke is not a credential, and a log of
// paths with the ids stripped out is a log nobody can follow.
func redactPath(p string) string {
	for _, prefix := range []string{"/api/v1/invites/", "/join/", "/reset/"} {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		segment, tail, _ := strings.Cut(rest, "/")
		if segment == "" || isDigits(segment) {
			return p
		}
		if tail != "" {
			return prefix + "…/" + tail
		}
		return prefix + "…"
	}
	return p
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// securityHeaders sets the headers that apply to every response, API and asset alike.
//
// The CSP ships without 'unsafe-inline' and the frontend is written for that: event
// handlers are attached with addEventListener, never as inline attributes. Weakening it
// here would quietly permit a class of bug the frontend has been designed to make
// impossible (CONVENTIONS.md §6).
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; img-src 'self' data:; base-uri 'none'; " +
		"form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-App-Version", s.version)
		next.ServeHTTP(w, r)
	})
}

// bodyLimit caps every request body.
func bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// csrf rejects any mutation that did not come from our own JavaScript.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get(csrfHeader) != csrfValue {
				writeError(w, r, http.StatusForbidden, codeForbidden,
					"missing "+csrfHeader+": "+csrfValue+" header")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
