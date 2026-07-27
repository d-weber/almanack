package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"almanack/internal/auth"
	"almanack/internal/domain"
	"almanack/internal/mailer"
)

const sessionCookie = "almanack_session"

// decoyHash is a real argon2id hash of a value nobody knows. The login handler verifies
// against it when an address is unknown, so that "no such account" costs the same
// wall-clock time as "wrong password" — otherwise the endpoint is an enumeration oracle
// with a stopwatch instead of a status code.
const decoyHash = "$argon2id$v=19$m=65536,t=3,p=4$B+q94iEo/wlOLEbp4eHS5g$zapE3VOXLPA8KpzRVKTQbsGi28j9Bzo1U8H0TIxzVhM"

// sessionTouchInterval is how stale the "last seen" stamp is allowed to get before the
// sliding window is renewed. Renewing on literally every request would mean a database
// write per API call for no benefit: a session that is used at all in a 90-day window
// never expires either way.
const sessionTouchInterval = time.Hour

// requireSession resolves the session cookie and rejects the request without one.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "authentication required")
			return
		}
		sess, user, err := s.store.SessionByToken(r.Context(), auth.HashToken(cookie.Value))
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				s.clearSessionCookie(w)
				writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "authentication required")
				return
			}
			fail(w, r, err)
			return
		}

		now := s.clock.Now()
		if now.Sub(sess.LastSeenAt) > sessionTouchInterval {
			expires := now.Add(domain.SessionTTL)
			if err := s.store.TouchSession(r.Context(), sess.ID, now, expires); err != nil {
				// A failed slide is not worth failing the request the family made.
				slog.Error("slide session", "session", sess.ID, "error", err)
			} else {
				s.setSessionCookie(w, cookie.Value, expires)
			}
		}

		infoOf(r.Context()).userID = user.ID
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyUser, user)))
	}
}

// userOf returns the authenticated user. It is only called from handlers behind
// requireSession, where the value is always present.
func userOf(ctx context.Context) domain.User {
	u, _ := ctx.Value(ctxKeyUser).(domain.User)
	return u
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   !s.cfg.Dev,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   !s.cfg.Dev,
		SameSite: http.SameSiteLaxMode,
	})
}

// startSession creates a session row and sets the cookie.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, userID int64) error {
	token, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	expires := s.clock.Now().Add(domain.SessionTTL)
	if _, err := s.store.CreateSession(ctx, userID, hash, expires); err != nil {
		return err
	}
	s.setSessionCookie(w, token, expires)
	return nil
}

// ---------------------------------------------------------------------------
// Public endpoints
// ---------------------------------------------------------------------------

type configResponse struct {
	FamilyTZ       string   `json:"family_tz"`
	AppVersion     string   `json:"app_version"`
	VAPIDPublicKey string   `json:"vapid_public_key"`
	Languages      []string `json:"languages"`
	DevMode        bool     `json:"dev_mode"`
	// SourceURL lets the About screen offer this build's source, which is how an
	// AGPL-3.0 network service satisfies section 13. Empty when unconfigured.
	SourceURL string `json:"source_url,omitempty"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, configResponse{
		FamilyTZ:       s.cfg.TZName,
		AppVersion:     s.version,
		VAPIDPublicKey: s.cfg.VAPIDPublic,
		Languages:      []string{string(domain.LangFR), string(domain.LangEN)},
		DevMode:        s.cfg.Dev,
		SourceURL:      s.cfg.SourceURL,
	})
}

type invitePreview struct {
	Valid         bool   `json:"valid"`
	CalendarName  string `json:"calendar_name,omitempty"`
	CalendarColor string `json:"calendar_color,omitempty"`
}

// handleInvitePreview answers 200 whatever the token is. An unknown, expired and
// revoked link are all simply "not valid": the holder of a stale link learns nothing
// about whether the calendar exists.
func (s *Server) handleInvitePreview(w http.ResponseWriter, r *http.Request) {
	_, cal, err := s.store.InviteByToken(r.Context(), auth.HashToken(r.PathValue("token")), s.clock.Now())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeJSON(w, r, http.StatusOK, invitePreview{Valid: false})
			return
		}
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, invitePreview{
		Valid:         true,
		CalendarName:  cal.Name,
		CalendarColor: cal.Color,
	})
}

type signupRequest struct {
	InviteToken string `json:"invite_token"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Color       string `json:"color"`
	Lang        string `json:"lang"`
}

// handleSignup creates an account. There is no open registration: a valid invite is the
// only route in, and the first account ever created becomes the admin.
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimit(w, r, "signup") {
		return
	}
	var req signupRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	email, err := normalizeEmail(req.Email)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := validatePassword(req.Password); err != nil {
		fail(w, r, err)
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		fail(w, r, invalidf("a display name is required"))
		return
	}
	color, err := normalizeColor(req.Color)
	if err != nil {
		fail(w, r, err)
		return
	}
	lang := domain.Language(req.Lang)
	if req.Lang == "" {
		lang = defaultLang
	}
	if !lang.Valid() {
		fail(w, r, invalidf("unknown language %q", req.Lang))
		return
	}

	ctx := r.Context()
	now := s.clock.Now()
	invite, _, err := s.store.InviteByToken(ctx, auth.HashToken(req.InviteToken), now)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			fail(w, r, invalidf("this invitation is no longer valid"))
			return
		}
		fail(w, r, err)
		return
	}

	count, err := s.store.CountUsers(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		fail(w, r, err)
		return
	}
	user, err := s.store.CreateUser(ctx, domain.User{
		Email:       email,
		DisplayName: name,
		Color:       color,
		Lang:        lang,
		WeekStart:   time.Monday,
		TimeFormat:  "24h",
		IsAdmin:     count == 0,
	}, hash)
	if err != nil {
		fail(w, r, err)
		return
	}

	if err := s.store.AddMember(ctx, invite.CalendarID, user.ID); err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.IncrementInviteUse(ctx, invite.ID); err != nil {
		slog.Error("count invite use", "invite", invite.ID, "error", err)
	}
	// Seed the notification preferences so the planner sees a real row rather than
	// relying on every reader to substitute defaults.
	if err := s.store.UpdatePrefs(ctx, defaultPrefs(user.ID)); err != nil {
		slog.Error("seed notification prefs", "user", user.ID, "error", err)
	}
	if err := s.store.LogActivity(ctx, domain.Activity{
		CalendarID: invite.CalendarID,
		UserID:     user.ID,
		Action:     domain.ActionMemberJoined,
		Title:      user.DisplayName,
	}); err != nil {
		slog.Error("record activity", "action", domain.ActionMemberJoined, "error", err)
	}

	if err := s.startSession(ctx, w, user.ID); err != nil {
		fail(w, r, err)
		return
	}
	infoOf(ctx).userID = user.ID
	writeJSON(w, r, http.StatusCreated, map[string]any{"user": user})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin answers identically for an unknown address and a wrong password, in both
// body and elapsed time: the decoy hash below costs what a real verification costs.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimit(w, r, "login") {
		return
	}
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	email := strings.ToLower(strings.TrimSpace(req.Email))
	user, err := s.store.UserByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			fail(w, r, err)
			return
		}
		// Spend the same time a real verification would, against a hash of nothing.
		_, _ = auth.VerifyPassword(decoyHash, req.Password)
		s.rejectLogin(w, r, email, "unknown address")
		return
	}
	hash, err := s.store.UserPasswordHash(ctx, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	ok, err := auth.VerifyPassword(hash, req.Password)
	if err != nil {
		// A hash this binary cannot read means a corrupt row, not a wrong password:
		// answering 401 would send somebody round the reset loop forever.
		fail(w, r, fmt.Errorf("stored password hash for user %d: %w", user.ID, err))
		return
	}
	if !ok {
		s.rejectLogin(w, r, email, "wrong password")
		return
	}

	if err := s.startSession(ctx, w, user.ID); err != nil {
		fail(w, r, err)
		return
	}
	infoOf(ctx).userID = user.ID
	writeJSON(w, r, http.StatusOK, map[string]any{"user": user})
}

func (s *Server) rejectLogin(w http.ResponseWriter, r *http.Request, email, reason string) {
	slog.Warn("login refused", "reason", reason, "email", redactEmail(email), "ip", s.clientIP(r))
	writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "invalid email or password")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := s.store.DeleteSession(r.Context(), auth.HashToken(cookie.Value)); err != nil {
			fail(w, r, err)
			return
		}
	}
	s.clearSessionCookie(w)
	writeNoContent(w)
}

type resetRequest struct {
	Email string `json:"email"`
}

// handleResetRequest always answers 204. Whether the address exists is exactly the fact
// this endpoint must not reveal.
func (s *Server) handleResetRequest(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimit(w, r, "reset") {
		return
	}
	var req resetRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	ctx := r.Context()
	user, err := s.store.UserByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			slog.Error("password reset lookup", "error", err)
		}
		writeNoContent(w)
		return
	}

	token, hash, err := auth.NewToken()
	if err != nil {
		slog.Error("password reset token", "error", err)
		writeNoContent(w)
		return
	}
	expires := s.clock.Now().Add(domain.PasswordResetTTL)
	if err := s.store.CreatePasswordReset(ctx, user.ID, hash, expires); err != nil {
		slog.Error("password reset record", "user", user.ID, "error", err)
		writeNoContent(w)
		return
	}
	s.sendResetMail(ctx, user, token)
	writeNoContent(w)
}

// sendResetMail composes the reset email from the shared catalogs, in the recipient's
// language. A failure is logged and swallowed: the HTTP answer must not differ.
func (s *Server) sendResetMail(ctx context.Context, user domain.User, token string) {
	if s.mailer == nil {
		slog.Warn("no mailer configured; password reset link not sent", "user", user.ID)
		return
	}
	link := strings.TrimRight(s.cfg.BaseURL, "/") + "/reset/" + token
	t := func(key string, params map[string]string) string { return s.catalog.T(user.Lang, key, params) }

	body := strings.Join([]string{
		t("mail.greeting", map[string]string{"name": user.DisplayName}),
		"",
		t("mail.reset.body", nil),
		link,
		"",
		t("mail.reset.ignore", nil),
		"",
		"-- ",
		t("mail.footer", nil),
	}, "\n")

	if err := s.mailer.Send(ctx, mailer.Message{
		To:      user.Email,
		Subject: t("mail.subject.reset", nil),
		Text:    body,
	}); err != nil {
		slog.Error("send password reset mail", "user", user.ID, "error", err)
	}
}

type resetConfirm struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// handleResetConfirm burns the token, sets the password and logs the user out
// everywhere: a reset exists to lock somebody out, and leaving their sessions alive
// would mean it did not.
func (s *Server) handleResetConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimit(w, r, "reset") {
		return
	}
	var req resetConfirm
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	if err := validatePassword(req.Password); err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	userID, err := s.store.ConsumePasswordReset(ctx, auth.HashToken(req.Token), s.clock.Now())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			fail(w, r, invalidf("this reset link is no longer valid"))
			return
		}
		fail(w, r, err)
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.SetPassword(ctx, userID, hash); err != nil {
		fail(w, r, err)
		return
	}
	if err := s.store.DeleteUserSessions(ctx, userID); err != nil {
		fail(w, r, err)
		return
	}
	s.clearSessionCookie(w)
	writeNoContent(w)
}

// redactEmail keeps a login failure recognisable in the log without recording who it
// was aimed at.
func redactEmail(addr string) string {
	at := strings.IndexByte(addr, '@')
	if at <= 1 {
		return "***"
	}
	return addr[:1] + "***" + addr[at:]
}

func defaultPrefs(userID int64) domain.NotificationPrefs {
	return domain.NotificationPrefs{
		UserID:         userID,
		DigestEnabled:  true,
		DigestTime:     "07:30",
		SummaryTime:    "20:00",
		EmailReminders: true,
		ActivityPush:   true,
	}
}
