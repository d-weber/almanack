package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"almanack/internal/auth"
	"almanack/internal/config"
	"almanack/internal/domain"
)

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

func TestLoginSuccess(t *testing.T) {
	e := newEnv(t)
	user := e.createUser("maman@example.org", "Maman")

	res := e.post("/api/v1/auth/login", map[string]string{
		"email": "maman@example.org", "password": testPassword,
	}).expect(http.StatusOK)

	var body struct {
		User domain.User `json:"user"`
	}
	res.decode(&body)
	if body.User.ID != user.ID || body.User.Email != user.Email {
		t.Fatalf("login returned %+v", body.User)
	}
	if !body.User.IsAdmin {
		t.Errorf("the first account should be the admin")
	}

	cookie := findCookie(res.header, sessionCookie)
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Errorf("session cookie = %+v; want HttpOnly, SameSite=Lax, Path=/", cookie)
	}
	if cookie.Secure {
		t.Errorf("session cookie is Secure in dev mode, so http://localhost would never see it")
	}
	if len(cookie.Value) < 40 {
		t.Errorf("session token %q looks too short for 256 bits of base64url", cookie.Value)
	}
	// The cookie carries the token; the database keeps only its hash.
	if _, _, err := e.store.SessionByToken(t.Context(), cookie.Value); err == nil {
		t.Errorf("the raw cookie value resolves a session, so it was stored unhashed")
	}

	e.get("/api/v1/me").expect(http.StatusOK)
}

func TestLoginCookieIsSecureOutsideDevMode(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Dev = false })
	e.createUser("maman@example.org", "Maman")

	res := e.post("/api/v1/auth/login", map[string]string{
		"email": "maman@example.org", "password": testPassword,
	}).expect(http.StatusOK)
	cookie := findCookie(res.header, sessionCookie)
	if cookie == nil || !cookie.Secure {
		t.Fatalf("session cookie outside dev mode = %+v; want Secure", cookie)
	}
}

// TestLoginFailuresAreIndistinguishable is the anti-enumeration property: an unknown
// address and a wrong password must produce byte-identical answers.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	e := newEnv(t)
	e.createUser("maman@example.org", "Maman")

	unknown := e.post("/api/v1/auth/login", map[string]string{
		"email": "nobody@example.org", "password": testPassword,
	}).expect(http.StatusUnauthorized)

	wrong := e.post("/api/v1/auth/login", map[string]string{
		"email": "maman@example.org", "password": "not-the-password",
	}).expect(http.StatusUnauthorized)

	if string(unknown.body) != string(wrong.body) {
		t.Fatalf("responses differ:\n unknown address: %s\n wrong password:  %s", unknown.body, wrong.body)
	}
	if unknown.errorCode() != codeUnauthorized {
		t.Errorf("error code = %q", unknown.errorCode())
	}
	if findCookie(unknown.header, sessionCookie) != nil || findCookie(wrong.header, sessionCookie) != nil {
		t.Errorf("a failed login set a session cookie")
	}
	e.get("/api/v1/me").expect(http.StatusUnauthorized)
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	e.createUser("maman@example.org", "Maman")

	limit := rateLimits["login"]
	var limited *resp
	for i := 0; i < limit.burst+3; i++ {
		res := e.post("/api/v1/auth/login", map[string]string{
			"email": "maman@example.org", "password": "wrong",
		})
		if res.status == http.StatusTooManyRequests {
			limited = res
			break
		}
		if res.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d", i+1, res.status)
		}
	}
	if limited == nil {
		t.Fatalf("no 429 after %d failed logins", limit.burst+3)
	}
	if code := limited.errorCode(); code != codeRateLimited {
		t.Errorf("error code = %q, want %q", code, codeRateLimited)
	}

	// Even the correct password is refused while the bucket is empty…
	e.post("/api/v1/auth/login", map[string]string{
		"email": "maman@example.org", "password": testPassword,
	}).expect(http.StatusTooManyRequests)

	// …and the bucket refills with time, not with a restart.
	e.clk.Advance(2 * limit.refill)
	e.post("/api/v1/auth/login", map[string]string{
		"email": "maman@example.org", "password": testPassword,
	}).expect(http.StatusOK)
}

func TestLogoutClearsTheSession(t *testing.T) {
	e := newEnv(t)
	e.family()

	e.post("/api/v1/auth/logout", nil).expect(http.StatusNoContent)
	e.get("/api/v1/me").expect(http.StatusUnauthorized)
}

// ---------------------------------------------------------------------------
// Invites and signup
// ---------------------------------------------------------------------------

func TestInviteSignupFlow(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()

	var invite inviteResponse
	e.post(fmt.Sprintf("/api/v1/calendars/%d/invites", cal.ID), nil).
		expect(http.StatusCreated).decode(&invite)
	if invite.Token == "" || !strings.HasSuffix(invite.URL, "/join/"+invite.Token) {
		t.Fatalf("invite = %+v", invite)
	}
	if !invite.ExpiresAt.After(e.clk.Now()) {
		t.Fatalf("invite expires at %v, which is not in the future", invite.ExpiresAt)
	}

	// The preview is public and says nothing but "valid".
	guest := e.newClient()
	var preview invitePreview
	e.request(guest, http.MethodGet, "/api/v1/invites/"+invite.Token, nil).
		expect(http.StatusOK).decode(&preview)
	if !preview.Valid || preview.CalendarName != "Famille" {
		t.Fatalf("preview = %+v", preview)
	}

	signup := func(client *http.Client, email, name string) *resp {
		return e.request(client, http.MethodPost, "/api/v1/auth/signup", map[string]string{
			"invite_token": invite.Token, "email": email, "password": "un-bon-mot-de-passe",
			"display_name": name, "color": "#2980b9", "lang": "fr",
		})
	}

	var created struct {
		User domain.User `json:"user"`
	}
	signup(guest, "papa@example.org", "Papa").expect(http.StatusCreated).decode(&created)
	if created.User.IsAdmin {
		t.Errorf("the second account must not be an admin")
	}
	// Signup signs the new account in straight away.
	e.request(guest, http.MethodGet, "/api/v1/me", nil).expect(http.StatusOK)

	// Signing up joined the calendar…
	member, err := e.store.IsMember(t.Context(), cal.ID, created.User.ID)
	if err != nil || !member {
		t.Fatalf("the new account did not join the calendar (member=%v, err=%v)", member, err)
	}
	// …and got notification preferences of its own, so the planner can see it.
	if _, err := e.store.Prefs(t.Context(), created.User.ID); err != nil {
		t.Fatalf("no notification prefs were seeded: %v", err)
	}

	// The link is multi-use inside its window: one link goes to the whole household.
	second := e.newClient()
	signup(second, "leo@example.org", "Léo").expect(http.StatusCreated)

	// The same address twice is a conflict, not a silent second account.
	signup(e.newClient(), "leo@example.org", "Léo again").expect(http.StatusConflict)

	// A bad token is refused without saying why.
	signup(e.newClient(), "mamie@example.org", "Mamie").expect(http.StatusCreated)
	bad := e.request(e.newClient(), http.MethodPost, "/api/v1/auth/signup", map[string]string{
		"invite_token": "not-a-real-token", "email": "papi@example.org",
		"password": "un-bon-mot-de-passe", "display_name": "Papi", "color": "#2980b9", "lang": "fr",
	})
	bad.expect(http.StatusBadRequest)
}

func TestInviteExpiryAndRevocation(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()

	newInvite := func() inviteResponse {
		var inv inviteResponse
		e.post(fmt.Sprintf("/api/v1/calendars/%d/invites", cal.ID), nil).
			expect(http.StatusCreated).decode(&inv)
		return inv
	}
	preview := func(token string) invitePreview {
		var p invitePreview
		e.request(e.newClient(), http.MethodGet, "/api/v1/invites/"+token, nil).
			expect(http.StatusOK).decode(&p)
		return p
	}

	// Expired.
	expired := newInvite()
	e.clk.Advance(domain.InviteTTL + time.Hour)
	if preview(expired.Token).Valid {
		t.Errorf("an expired invite previews as valid")
	}
	e.request(e.newClient(), http.MethodPost, "/api/v1/auth/signup", map[string]string{
		"invite_token": expired.Token, "email": "papa@example.org", "password": "un-bon-mot-de-passe",
		"display_name": "Papa", "color": "#2980b9", "lang": "fr",
	}).expect(http.StatusBadRequest)

	// Revoked.
	revoked := newInvite()
	e.post(fmt.Sprintf("/api/v1/invites/%d/revoke", revoked.ID), nil).expect(http.StatusNoContent)
	if preview(revoked.Token).Valid {
		t.Errorf("a revoked invite previews as valid")
	}
	e.request(e.newClient(), http.MethodPost, "/api/v1/auth/signup", map[string]string{
		"invite_token": revoked.Token, "email": "papa@example.org", "password": "un-bon-mot-de-passe",
		"display_name": "Papa", "color": "#2980b9", "lang": "fr",
	}).expect(http.StatusBadRequest)

	// Only live invites are listed, and never with their token.
	live := newInvite()
	var listed struct {
		Invites []domain.Invite `json:"invites"`
	}
	res := e.get(fmt.Sprintf("/api/v1/calendars/%d/invites", cal.ID)).expect(http.StatusOK)
	res.decode(&listed)
	if len(listed.Invites) != 1 || listed.Invites[0].ID != live.ID {
		t.Fatalf("active invites = %+v", listed.Invites)
	}
	if strings.Contains(string(res.body), live.Token) {
		t.Errorf("the invite list leaks a token")
	}
}

func TestInviteRevokeIsScopedToTheCallersCalendars(t *testing.T) {
	e := newEnv(t)
	_, ours := e.family()

	stranger := e.createUser("voisin@example.org", "Voisin")
	theirs := e.createCalendar(stranger, "Voisins")
	invite, err := e.store.CreateInvite(t.Context(), domain.Invite{
		CalendarID: theirs.ID, CreatedBy: stranger.ID, ExpiresAt: e.clk.Now().Add(domain.InviteTTL),
	}, auth.HashToken("some-token"))
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}

	e.post(fmt.Sprintf("/api/v1/invites/%d/revoke", invite.ID), nil).expect(http.StatusNotFound)

	// Ours still works, so the check is scope, not a blanket refusal.
	var mine inviteResponse
	e.post(fmt.Sprintf("/api/v1/calendars/%d/invites", ours.ID), nil).
		expect(http.StatusCreated).decode(&mine)
	e.post(fmt.Sprintf("/api/v1/invites/%d/revoke", mine.ID), nil).expect(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Password reset, end to end through the file mailer
// ---------------------------------------------------------------------------

func TestPasswordResetEndToEnd(t *testing.T) {
	e := newEnv(t)
	user, _ := e.family()

	// A second browser, so that "all sessions are invalidated" is observable.
	other := e.login(e.newClient(), user.Email)
	e.request(other, http.MethodGet, "/api/v1/me", nil).expect(http.StatusOK)

	// An unknown address answers exactly like a known one.
	unknown := e.post("/api/v1/auth/password-reset/request", map[string]string{"email": "nobody@example.org"})
	unknown.expect(http.StatusNoContent)
	known := e.post("/api/v1/auth/password-reset/request", map[string]string{"email": user.Email})
	known.expect(http.StatusNoContent)
	if len(known.body) != 0 || len(unknown.body) != 0 {
		t.Fatalf("password reset answered with a body")
	}

	token := resetTokenFromMail(t, e.cfg.MailDir, e.cfg.BaseURL)

	// The token is single use and short lived; here it is used once, successfully.
	const newPassword = "un-nouveau-mot-de-passe"
	e.post("/api/v1/auth/password-reset/confirm", map[string]string{
		"token": token, "password": newPassword,
	}).expect(http.StatusNoContent)

	// Every session of that user is gone — including the one in the other browser.
	e.get("/api/v1/me").expect(http.StatusUnauthorized)
	e.request(other, http.MethodGet, "/api/v1/me", nil).expect(http.StatusUnauthorized)

	// The old password no longer works, the new one does.
	fresh := e.newClient()
	e.request(fresh, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": testPassword,
	}).expect(http.StatusUnauthorized)
	e.request(fresh, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": user.Email, "password": newPassword,
	}).expect(http.StatusOK)

	// Replaying the token fails: it was burned.
	e.request(fresh, http.MethodPost, "/api/v1/auth/password-reset/confirm", map[string]string{
		"token": token, "password": "encore-un-autre-mot-de-passe",
	}).expect(http.StatusBadRequest)
}

func TestPasswordResetTokenExpires(t *testing.T) {
	e := newEnv(t)
	user, _ := e.family()

	e.post("/api/v1/auth/password-reset/request", map[string]string{"email": user.Email}).
		expect(http.StatusNoContent)
	token := resetTokenFromMail(t, e.cfg.MailDir, e.cfg.BaseURL)

	e.clk.Advance(domain.PasswordResetTTL + time.Minute)
	e.post("/api/v1/auth/password-reset/confirm", map[string]string{
		"token": token, "password": "un-nouveau-mot-de-passe",
	}).expect(http.StatusBadRequest)
}

// resetTokenFromMail reads the newest .eml the dev sink wrote and extracts the reset
// link's token — the same path a person clicking the link takes.
func resetTokenFromMail(t *testing.T, dir, baseURL string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read mail dir: %v", err)
	}
	var newest string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".eml") && entry.Name() > newest {
			newest = entry.Name()
		}
	}
	if newest == "" {
		t.Fatalf("no mail was written to %s", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, newest))
	if err != nil {
		t.Fatalf("read %s: %v", newest, err)
	}
	prefix := strings.TrimRight(baseURL, "/") + "/reset/"
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("no reset link in the mail:\n%s", data)
	return ""
}

// ---------------------------------------------------------------------------
// Profile and password change
// ---------------------------------------------------------------------------

func TestPatchMe(t *testing.T) {
	e := newEnv(t)
	e.family()

	var body struct {
		User domain.User `json:"user"`
	}
	e.do(http.MethodPatch, "/api/v1/me", map[string]any{
		"display_name": "Maman ",
		"color":        "#AABBCC",
		"lang":         "en",
		"week_start":   0,
		"time_format":  "12h",
	}).expect(http.StatusOK).decode(&body)

	if body.User.DisplayName != "Maman" || body.User.Color != "#aabbcc" ||
		body.User.Lang != domain.LangEN || body.User.WeekStart != time.Sunday || body.User.TimeFormat != "12h" {
		t.Fatalf("patched user = %+v", body.User)
	}

	for _, bad := range []map[string]any{
		{"color": "red"},
		{"lang": "de"},
		{"week_start": 9},
		{"time_format": "36h"},
		{"display_name": ""},
	} {
		e.do(http.MethodPatch, "/api/v1/me", bad).expect(http.StatusBadRequest)
	}
}

func TestPasswordChangeInvalidatesOtherSessions(t *testing.T) {
	e := newEnv(t)
	user, _ := e.family()
	other := e.login(e.newClient(), user.Email)

	e.do(http.MethodPatch, "/api/v1/me", map[string]any{
		"current_password": "wrong", "new_password": "un-nouveau-mot-de-passe",
	}).expect(http.StatusUnauthorized)

	e.do(http.MethodPatch, "/api/v1/me", map[string]any{
		"current_password": testPassword, "new_password": "un-nouveau-mot-de-passe",
	}).expect(http.StatusOK)

	// The browser that changed the password stays signed in…
	e.get("/api/v1/me").expect(http.StatusOK)
	// …every other one does not.
	e.request(other, http.MethodGet, "/api/v1/me", nil).expect(http.StatusUnauthorized)
}

func TestMeBootstrapPayload(t *testing.T) {
	e := newEnv(t)
	user, cal := e.family()

	var me meResponse
	e.get("/api/v1/me").expect(http.StatusOK).decode(&me)

	if me.User.ID != user.ID {
		t.Fatalf("me.user = %+v", me.User)
	}
	if me.FamilyTZ != "Europe/Paris" || me.AppVersion != e.srv.AppVersion() {
		t.Errorf("me = %+v", me)
	}
	if !me.ServerTime.Equal(e.clk.Now()) {
		t.Errorf("server_time = %v, want the clock's %v", me.ServerTime, e.clk.Now())
	}
	if len(me.Calendars) != 1 {
		t.Fatalf("calendars = %+v", me.Calendars)
	}
	view := me.Calendars[0]
	if view.ID != cal.ID || view.CreatorID != user.ID {
		t.Errorf("calendar view = %+v", view)
	}
	if len(view.Labels) != domain.LabelsPerCalendar {
		t.Errorf("calendar has %d labels, want %d", len(view.Labels), domain.LabelsPerCalendar)
	}
	if len(view.Members) != 1 || view.Members[0].UserID != user.ID {
		t.Errorf("members = %+v", view.Members)
	}
	if me.Prefs.DigestTime == "" {
		t.Errorf("prefs are empty: %+v", me.Prefs)
	}
	// No devices have confirmed, so the client is told to offer the repair flow.
	if !me.Prefs.PushHealth.Stale {
		t.Errorf("push_health.stale = false with no confirmed device")
	}
}

func TestSessionRequiredEverywhere(t *testing.T) {
	e := newEnv(t)
	e.family()
	guest := e.newClient()

	for _, r := range e.srv.routes {
		if !r.auth || strings.Contains(r.path, "{") {
			continue
		}
		res := e.request(guest, r.method, r.path, map[string]string{})
		if res.status != http.StatusUnauthorized {
			t.Errorf("%s without a session: status = %d, want 401", r.pattern(), res.status)
		}
	}
}

func findCookie(header http.Header, name string) *http.Cookie {
	if header == nil {
		return nil
	}
	res := http.Response{Header: header}
	for _, c := range res.Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	return nil
}
