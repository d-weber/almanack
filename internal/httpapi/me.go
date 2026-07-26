package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"agenda/internal/auth"
	"agenda/internal/domain"
	"agenda/internal/imgproc"
)

type meResponse struct {
	User       domain.User    `json:"user"`
	Prefs      prefsView      `json:"prefs"`
	Calendars  []calendarView `json:"calendars"`
	FamilyTZ   string         `json:"family_tz"`
	AppVersion string         `json:"app_version"`
	ServerTime time.Time      `json:"server_time"`
}

// handleMe is the single bootstrap call the app makes on load: everything the shell
// needs to render before it fetches a date range.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userOf(ctx)

	prefs, err := s.prefsView(ctx, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	cals, err := s.calendarViews(ctx, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, meResponse{
		User:       user,
		Prefs:      prefs,
		Calendars:  cals,
		FamilyTZ:   s.cfg.TZName,
		AppVersion: s.version,
		ServerTime: s.clock.Now(),
	})
}

type patchMeRequest struct {
	DisplayName *string `json:"display_name"`
	Color       *string `json:"color"`
	Lang        *string `json:"lang"`
	WeekStart   *int    `json:"week_start"`
	TimeFormat  *string `json:"time_format"`

	CurrentPassword *string `json:"current_password"`
	NewPassword     *string `json:"new_password"`
}

// handlePatchMe edits the caller's profile, and doubles as the password change.
//
// Changing a password logs every *other* browser out: that is the point of changing it.
// This browser is issued a fresh session in the same response, so the person who just
// typed their new password is not immediately thrown back to the login screen.
func (s *Server) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	var req patchMeRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)

	if req.NewPassword != nil || req.CurrentPassword != nil {
		if req.CurrentPassword == nil || req.NewPassword == nil {
			fail(w, r, invalidf("a password change needs both current_password and new_password"))
			return
		}
		if err := s.changePassword(w, r, user, *req.CurrentPassword, *req.NewPassword); err != nil {
			fail(w, r, err)
			return
		}
	}

	updated := user
	if req.DisplayName != nil {
		name, err := cleanText(*req.DisplayName, maxNameLen, "the display name")
		if err != nil {
			fail(w, r, err)
			return
		}
		if name == "" {
			fail(w, r, invalidf("a display name is required"))
			return
		}
		updated.DisplayName = name
	}
	if req.Color != nil {
		color, err := normalizeColor(*req.Color)
		if err != nil {
			fail(w, r, err)
			return
		}
		updated.Color = color
	}
	if req.Lang != nil {
		lang, err := validLanguage(*req.Lang)
		if err != nil {
			fail(w, r, err)
			return
		}
		updated.Lang = lang
	}
	if req.WeekStart != nil {
		if *req.WeekStart < 0 || *req.WeekStart > 6 {
			fail(w, r, invalidf("week_start must be 0 (Sunday) to 6 (Saturday)"))
			return
		}
		// Display only. Recurrence math is Monday-anchored, always, and this value must
		// never reach internal/recur (CONVENTIONS.md §4).
		updated.WeekStart = time.Weekday(*req.WeekStart)
	}
	if req.TimeFormat != nil {
		if *req.TimeFormat != "24h" && *req.TimeFormat != "12h" {
			fail(w, r, invalidf(`time_format must be "24h" or "12h"`))
			return
		}
		updated.TimeFormat = *req.TimeFormat
	}

	if updated != user {
		if err := s.store.UpdateUser(ctx, updated); err != nil {
			fail(w, r, err)
			return
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"user": updated})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, user domain.User, current, next string) error {
	ctx := r.Context()
	hash, err := s.store.UserPasswordHash(ctx, user.ID)
	if err != nil {
		return err
	}
	ok, err := auth.VerifyPassword(hash, current)
	if err != nil {
		return fmt.Errorf("stored password hash for user %d: %w", user.ID, err)
	}
	if !ok {
		return domain.ErrUnauthorized
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	newHash, err := auth.HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.store.SetPassword(ctx, user.ID, newHash); err != nil {
		return err
	}
	// Every session goes, including this one; a fresh cookie keeps the caller signed in.
	if err := s.store.DeleteUserSessions(ctx, user.ID); err != nil {
		return err
	}
	return s.startSession(ctx, w, user.ID)
}

// handlePutAvatar takes raw image bytes and stores only what imgproc produces: a 128 px
// square JPEG with no EXIF, no colour profile and nothing the original file smuggled in.
func (s *Server) handlePutAvatar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userOf(ctx)

	// One byte over the limit is enough to tell that it is over the limit.
	body := http.MaxBytesReader(w, r.Body, imgproc.MaxUploadBytes+1)
	data, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			fail(w, r, invalidf("an avatar must be at most %d bytes", imgproc.MaxUploadBytes))
			return
		}
		fail(w, r, invalidf("could not read the uploaded image"))
		return
	}
	if len(data) > imgproc.MaxUploadBytes {
		fail(w, r, invalidf("an avatar must be at most %d bytes", imgproc.MaxUploadBytes))
		return
	}

	// imgproc's errors wrap domain.ErrInvalid, so a bad image maps to 400 for free.
	processed, err := imgproc.Process(data)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.SetAvatar(ctx, user.ID, processed); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"has_avatar": true})
}

func (s *Server) handleDeleteAvatar(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAvatar(r.Context(), userOf(r.Context()).ID); err != nil {
		fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// handleUserAvatar serves a stored avatar. The bytes are ours — imgproc re-encoded
// them — but nosniff is set anyway, because the one place a stored blob is served back
// is the one place worth being paranoid about.
func (s *Server) handleUserAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	// Every other id-scoped route is gated on membership; this one was not, so any
	// account could walk the id space and collect the avatars of people it shares no
	// calendar with. Answer 404 rather than 403: whether that id exists is itself
	// something the caller has no business learning.
	if id != userOf(r.Context()).ID {
		shared, err := s.sharesACalendar(r.Context(), userOf(r.Context()).ID, id)
		if err != nil {
			fail(w, r, err)
			return
		}
		if !shared {
			fail(w, r, fmt.Errorf("avatar of user %d: %w", id, domain.ErrNotFound))
			return
		}
	}
	data, err := s.store.Avatar(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	h := w.Header()
	h.Set("Content-Type", imgproc.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", etag)
	h.Set("Cache-Control", "private, max-age=300")
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if _, err := w.Write(data); err != nil {
		slog.Debug("write avatar", "user", id, "error", err)
	}
}

// sharesACalendar reports whether two people can see each other at all — the
// membership relation the rest of the API gates on, asked from the other direction.
func (s *Server) sharesACalendar(ctx context.Context, a, b int64) (bool, error) {
	cals, err := s.store.ListCalendarsForUser(ctx, a)
	if err != nil {
		return false, err
	}
	for _, c := range cals {
		member, err := s.store.IsMember(ctx, c.ID, b)
		if err != nil {
			return false, err
		}
		if member {
			return true, nil
		}
	}
	return false, nil
}
