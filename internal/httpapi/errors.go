package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"almanack/internal/domain"
)

// Error codes on the wire. The client shows localized text keyed by the code, so these
// strings are part of the contract and must not change.
const (
	codeUnauthorized = "unauthorized"
	codeForbidden    = "forbidden"
	codeNotFound     = "not_found"
	codeInvalid      = "invalid"
	codeConflict     = "conflict"
	codeRateLimited  = "rate_limited"
	codeInternal     = "internal"
)

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON sends a JSON body. It marshals first so that an encoding failure cannot
// leave a half-written response behind a 200.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("encode response", "path", redactPath(r.URL.Path), "error", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not encode the response")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	body, err := json.Marshal(errorEnvelope{Error: errorDetail{Code: code, Message: message}})
	if err != nil { // unreachable: the envelope is two strings
		body = []byte(`{"error":{"code":"internal","message":"internal error"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// fail maps an error to its status code through the domain sentinels and answers.
//
// The message is only passed through for invalid and conflict, where it explains what
// the client got wrong and is worth reading in a log or a bug report. Everything else
// gets a fixed message: 404/403 texts are the ones that leak whether something exists,
// and a 500's real cause belongs in the server log, not in a family member's browser.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "not found")
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, r, http.StatusForbidden, codeForbidden, "not allowed")
	case errors.Is(err, domain.ErrUnauthorized):
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "authentication required")
	case errors.Is(err, domain.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, codeInvalid, cleanMessage(err))
	case errors.Is(err, domain.ErrConflict):
		writeError(w, r, http.StatusConflict, codeConflict, cleanMessage(err))
	default:
		slog.Error("request failed",
			"method", r.Method, "path", redactPath(r.URL.Path), "error", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

// cleanMessage strips the wrapping context off a validation error, leaving the part a
// human can act on. "create event: invalid: an event needs a title" reads better as
// "an event needs a title".
func cleanMessage(err error) string {
	msg := err.Error()
	for _, marker := range []string{"invalid: ", "conflict: "} {
		if i := strings.LastIndex(msg, marker); i >= 0 {
			return msg[i+len(marker):]
		}
	}
	return msg
}

// invalidf builds a domain.ErrInvalid with a message, for the many small request
// validations the handlers do themselves.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{domain.ErrInvalid}, args...)...)
}

// decodeJSON reads a JSON request body into v, rejecting anything that is not an object
// of known fields. Unknown fields are an error on purpose: a client sending
// "participant" when the field is "participants" should hear about it now rather than
// wonder later why nothing happened.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if err == io.EOF {
			return invalidf("a JSON body is required")
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return invalidf("the request body is too large")
		}
		return invalidf("malformed JSON: %s", err)
	}
	// A second value would mean the client sent two documents; the first would have
	// been silently used.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return invalidf("the body must contain exactly one JSON document")
	}
	return nil
}

// pathInt64 reads a {name} path segment as an integer id.
func pathInt64(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, invalidf("%s must be a positive integer, got %q", name, raw)
	}
	return id, nil
}
