package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"almanack/internal/auth"
	"almanack/internal/clock"
	"almanack/internal/config"
	"almanack/internal/domain"
	"almanack/internal/events"
	"almanack/internal/i18n"
	"almanack/internal/mailer"
	"almanack/internal/store"
)

// The tests run against a real temp SQLite store and a real http.Server, because the
// things worth pinning here — cookies, headers, status codes, CSRF — only exist at that
// level. Nothing touches the network and nothing sleeps: the clock is fake.

const testPassword = "motdepasse"

// Fixture accounts are hashed by internal/auth, exactly as the seeder does. That is
// deliberate: if the HTTP layer ever grew a password format of its own, the accounts
// `almanack seed` creates would stop being able to log in, and this is where that shows up.
//
// The hash is computed once for the whole binary, since RFC 9106 parameters cost ~100 ms
// a time by design.
var fixtureHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(testPassword)
	if err != nil {
		panic(err)
	}
	return h
})

type env struct {
	t      *testing.T
	cfg    config.Config
	clk    *clock.Fake
	store  *store.Store
	events *events.Service
	mail   *mailer.FileMailer
	srv    *Server
	ts     *httptest.Server
	client *http.Client
}

func testWebFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":  &fstest.MapFile{Data: []byte("<!doctype html><title>Almanack</title><div id=app></div>")},
		"sw.js":       &fstest.MapFile{Data: []byte("const CACHE = 'almanack-__APP_VERSION__';\n")},
		"js/app.js":   &fstest.MapFile{Data: []byte("export const boot = () => {};\n")},
		"css/app.css": &fstest.MapFile{Data: []byte(":root{--c:#3b7ddd}\n")},
	}
}

func newEnv(t *testing.T, tweaks ...func(*config.Config)) *env {
	t.Helper()

	dir := t.TempDir()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("load Europe/Paris: %v", err)
	}
	cfg := config.Config{
		Dev:            true,
		ListenAddr:     "127.0.0.1:0",
		BaseURL:        "http://localhost:8080",
		DataPath:       filepath.Join(dir, "almanack.db"),
		BackupDir:      filepath.Join(dir, "backups"),
		MailDir:        filepath.Join(dir, "mail"),
		TZName:         "Europe/Paris",
		FamilyTZ:       loc,
		TrustedProxies: []string{"127.0.0.1", "::1"},
		VAPIDPublic:    "BEl-test-public-key",
		SchedulerTick:  30 * time.Second,
		PlanHorizon:    48 * time.Hour,
	}
	for _, tweak := range tweaks {
		tweak(&cfg)
	}

	clk := clock.NewFake(time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC))
	st, err := store.Open(cfg.DataPath, loc, clk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	fileMailer, err := mailer.NewFile(cfg.MailDir)
	if err != nil {
		t.Fatalf("mail sink: %v", err)
	}

	srv, err := New(Deps{
		Store:   st,
		Events:  events.New(st, loc, clk),
		Mailer:  fileMailer,
		Catalog: i18n.MustLoad(),
		Clock:   clk,
		Config:  cfg,
		Web:     testWebFS(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	e := &env{t: t, cfg: cfg, clk: clk, store: st, events: srv.events, mail: fileMailer, srv: srv, ts: ts}
	e.client = e.newClient()
	return e
}

// newClient is a fresh browser: its own cookie jar, no session.
func (e *env) newClient() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type resp struct {
	t      *testing.T
	status int
	header http.Header
	body   []byte
}

func (r *resp) decode(v any) {
	r.t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		r.t.Fatalf("decode %q: %v", truncate(string(r.body), 300), err)
	}
}

func (r *resp) errorCode() string {
	r.t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(r.body, &env); err != nil {
		r.t.Fatalf("decode error body %q: %v", truncate(string(r.body), 300), err)
	}
	return env.Error.Code
}

func (r *resp) expect(status int) *resp {
	r.t.Helper()
	if r.status != status {
		r.t.Fatalf("status = %d, want %d (body: %s)", r.status, status, truncate(string(r.body), 400))
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// request is the workhorse. Non-GET requests carry the CSRF header unless a test
// deliberately drops it.
func (e *env) request(client *http.Client, method, path string, body any, tweaks ...func(*http.Request)) *resp {
	e.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, reader)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(csrfHeader, csrfValue)
	}
	for _, tweak := range tweaks {
		tweak(req)
	}

	res, err := client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return &resp{t: e.t, status: res.StatusCode, header: res.Header, body: data}
}

func (e *env) do(method, path string, body any, tweaks ...func(*http.Request)) *resp {
	e.t.Helper()
	return e.request(e.client, method, path, body, tweaks...)
}

func (e *env) get(path string) *resp { e.t.Helper(); return e.do(http.MethodGet, path, nil) }
func (e *env) post(path string, body any) *resp {
	e.t.Helper()
	return e.do(http.MethodPost, path, body)
}

// ---------------------------------------------------------------------------
// Fixtures
//
// The first account is created through the store, exactly as the CLI's first-run path
// does: the HTTP layer has no open registration, and an invite has to come from
// somewhere.
// ---------------------------------------------------------------------------

func (e *env) createUser(email, name string) domain.User {
	e.t.Helper()
	count, err := e.store.CountUsers(e.t.Context())
	if err != nil {
		e.t.Fatalf("count users: %v", err)
	}
	u, err := e.store.CreateUser(e.t.Context(), domain.User{
		Email:       email,
		DisplayName: name,
		Color:       "#c0392b",
		Lang:        domain.LangFR,
		WeekStart:   time.Monday,
		TimeFormat:  "24h",
		IsAdmin:     count == 0,
	}, fixtureHash())
	if err != nil {
		e.t.Fatalf("create user %s: %v", email, err)
	}
	if err := e.store.UpdatePrefs(e.t.Context(), defaultPrefs(u.ID)); err != nil {
		e.t.Fatalf("seed prefs: %v", err)
	}
	return u
}

func (e *env) createCalendar(owner domain.User, name string) domain.Calendar {
	e.t.Helper()
	c, err := e.store.CreateCalendar(e.t.Context(), domain.Calendar{
		Name: name, Color: "#3b7ddd", CreatorID: owner.ID,
	})
	if err != nil {
		e.t.Fatalf("create calendar: %v", err)
	}
	return c
}

func (e *env) labels(calendarID int64) []domain.Label {
	e.t.Helper()
	labels, err := e.store.ListLabels(e.t.Context(), calendarID)
	if err != nil {
		e.t.Fatalf("list labels: %v", err)
	}
	return labels
}

// login signs a client in and returns it.
func (e *env) login(client *http.Client, email string) *http.Client {
	e.t.Helper()
	e.request(client, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": email, "password": testPassword}).expect(http.StatusOK)
	return client
}

// family creates a user, a calendar and a signed-in session in one step, since almost
// every test needs exactly that.
func (e *env) family() (domain.User, domain.Calendar) {
	e.t.Helper()
	user := e.createUser("maman@example.org", "Maman")
	cal := e.createCalendar(user, "Famille")
	e.login(e.client, user.Email)
	return user, cal
}

// ---------------------------------------------------------------------------
// Routing invariants
// ---------------------------------------------------------------------------

// TestNoStateChangingGET walks the registered routes. One state-changing GET reopens
// CSRF, and this is the check that says there is not one.
func TestNoStateChangingGET(t *testing.T) {
	e := newEnv(t)
	seen := map[string]bool{}
	for _, r := range e.srv.routes {
		if seen[r.pattern()] {
			t.Errorf("route %s is registered twice", r.pattern())
		}
		seen[r.pattern()] = true

		safe := r.method == http.MethodGet || r.method == http.MethodHead || r.method == http.MethodOptions
		// /dev/login/{userID} is the one deliberate exception, documented where it is
		// declared: dev routes are only registered when cfg.Dev is set, they bind to
		// localhost, and being navigable is their whole purpose. The rule holds
		// everywhere that is reachable in production.
		if safe && r.mutates && !strings.HasPrefix(r.path, "/dev/") {
			t.Errorf("%s is a %s route that mutates state", r.pattern(), r.method)
		}
		if !safe && !r.mutates {
			t.Errorf("%s uses %s but is not marked as mutating; either it mutates (mark it) "+
				"or it should be a GET", r.pattern(), r.method)
		}
	}
	if len(e.srv.routes) < 30 {
		t.Fatalf("only %d routes registered; the table looks truncated", len(e.srv.routes))
	}
}

// TestMutationsRejectedWithoutCSRFHeader is the other half: the middleware enforces the
// header centrally, so no handler can forget it.
func TestCSRFHeaderRequired(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()

	drop := func(r *http.Request) { r.Header.Del(csrfHeader) }
	body := map[string]string{"name": "Maison"}

	res := e.do(http.MethodPatch, fmt.Sprintf("/api/v1/calendars/%d", cal.ID), body, drop)
	res.expect(http.StatusForbidden)
	if got := res.errorCode(); got != codeForbidden {
		t.Fatalf("error code = %q, want %q", got, codeForbidden)
	}

	// The same request with the header is accepted, and really did change something.
	var updated calendarView
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/calendars/%d", cal.ID), body).
		expect(http.StatusOK).decode(&updated)
	if updated.Name != "Maison" {
		t.Fatalf("calendar name = %q after the accepted PATCH", updated.Name)
	}
}

// TestGETIsNeverBlockedByCSRF guards the inverse mistake: reads must not require the
// header, or the service worker's cache-warming fetches would all fail.
func TestGETIsNeverBlockedByCSRF(t *testing.T) {
	e := newEnv(t)
	e.family()
	e.do(http.MethodGet, "/api/v1/me", nil, func(r *http.Request) {
		r.Header.Del(csrfHeader)
	}).expect(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Static serving and the PWA
// ---------------------------------------------------------------------------

func TestAppVersionHeaderOnEveryResponse(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/", "/index.html", "/api/v1/config", "/healthz", "/api/v1/me", "/nope.js"} {
		res := e.get(path)
		if res.header.Get("X-App-Version") != e.srv.AppVersion() {
			t.Errorf("%s: X-App-Version = %q, want %q", path, res.header.Get("X-App-Version"), e.srv.AppVersion())
		}
	}
	if len(e.srv.AppVersion()) != 8 {
		t.Errorf("app version %q is not a short hex hash", e.srv.AppVersion())
	}
}

func TestServiceWorkerVersionSubstitution(t *testing.T) {
	e := newEnv(t)
	res := e.get("/sw.js").expect(http.StatusOK)

	body := string(res.body)
	if strings.Contains(body, appVersionPlaceholder) {
		t.Errorf("sw.js still contains %s: %q", appVersionPlaceholder, body)
	}
	if !strings.Contains(body, e.srv.AppVersion()) {
		t.Errorf("sw.js does not carry the app version %q: %q", e.srv.AppVersion(), body)
	}
	if got := res.header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("sw.js Cache-Control = %q, want no-cache", got)
	}
	if got := res.header.Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("sw.js Content-Type = %q", got)
	}

	index := e.get("/").expect(http.StatusOK)
	if got := index.header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index.html Cache-Control = %q, want no-cache", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	res := e.get("/")
	csp := res.header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "img-src 'self' data:", "base-uri 'none'",
		"form-action 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP allows unsafe-inline: %q", csp)
	}
	if got := res.header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := res.header.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}

// TestSPAFallback: client-side routes get the shell, API and locale paths never do.
func TestSPAFallback(t *testing.T) {
	e := newEnv(t)

	shell := e.get("/join/some-token").expect(http.StatusOK)
	if !strings.Contains(string(shell.body), "<div id=app>") {
		t.Errorf("/join/… did not serve the shell: %q", truncate(string(shell.body), 120))
	}
	e.get("/reset/some-token").expect(http.StatusOK)

	api := e.get("/api/v1/nonsense").expect(http.StatusNotFound)
	if got := api.header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("unknown API path answered %q, not JSON", got)
	}
	if code := api.errorCode(); code != codeNotFound {
		t.Errorf("unknown API path error code = %q", code)
	}
	e.get("/locales/de.json").expect(http.StatusNotFound)

	// A missing asset with an extension is a 404, not a page of HTML.
	e.get("/js/missing.js").expect(http.StatusNotFound)
}

func TestLocalesAreServedFromTheSharedCatalogs(t *testing.T) {
	e := newEnv(t)
	for _, lang := range []string{"fr", "en"} {
		res := e.get("/locales/" + lang + ".json").expect(http.StatusOK)
		var table map[string]string
		res.decode(&table)
		if table["error.not_found"] == "" {
			t.Errorf("%s.json is missing error.not_found", lang)
		}
		if got := res.header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("%s.json Content-Type = %q", lang, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Dev endpoints
// ---------------------------------------------------------------------------

func TestDevRoutesAreNotMountedOutsideDevMode(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Dev = false })

	for _, r := range e.srv.routes {
		if strings.HasPrefix(r.path, "/dev") {
			t.Fatalf("route %s is registered with Dev=false", r.pattern())
		}
	}
	for _, path := range []string{"/dev/", "/dev/mail", "/dev/notifications", "/dev/state"} {
		e.get(path).expect(http.StatusNotFound)
	}
	e.post("/dev/tick", nil).expect(http.StatusNotFound)
	e.post("/dev/clock", map[string]string{"advance": "1h"}).expect(http.StatusNotFound)
	e.post("/dev/seed", nil).expect(http.StatusNotFound)
}

func TestDevDashboardAndTimeTravel(t *testing.T) {
	e := newEnv(t)

	dash := e.get("/dev/").expect(http.StatusOK)
	if !strings.Contains(string(dash.body), "Time travel") {
		t.Errorf("the dev dashboard does not mention time travel")
	}
	if strings.Contains(string(dash.body), "<script>") {
		t.Errorf("the dev dashboard has an inline script, which the CSP forbids")
	}
	e.get("/dev/dev.css").expect(http.StatusOK)
	e.get("/dev/dev.js").expect(http.StatusOK)

	before := e.clk.Now()
	e.post("/dev/clock", map[string]string{"advance": "26h"}).expect(http.StatusOK)
	if got := e.clk.Now().Sub(before); got != 26*time.Hour {
		t.Errorf("clock advanced by %v, want 26h", got)
	}
	e.post("/dev/clock", map[string]string{"set": "2026-08-04T06:00:00Z"}).expect(http.StatusOK)
	if got := e.clk.Now(); !got.Equal(time.Date(2026, 8, 4, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("clock = %v after set", got)
	}
	e.post("/dev/clock", map[string]string{"advance": "not-a-duration"}).expect(http.StatusBadRequest)

	// Time travel is a mutation like any other: no header, no travel.
	e.do(http.MethodPost, "/dev/clock", map[string]string{"advance": "1h"}, func(r *http.Request) {
		r.Header.Del(csrfHeader)
	}).expect(http.StatusForbidden)
}

func TestDevSeedAndInboxes(t *testing.T) {
	e := newEnv(t)

	res := e.post("/dev/seed", nil).expect(http.StatusOK)
	var seeded struct {
		Seeded      bool   `json:"seeded"`
		Users       int    `json:"users"`
		EventsAdded int    `json:"events_added"`
		Password    string `json:"password"`
	}
	res.decode(&seeded)
	if !seeded.Seeded || seeded.Users != 3 || seeded.EventsAdded != 3 {
		t.Fatalf("seed result = %+v", seeded)
	}
	// The seeded family can actually sign in — a seeder that produces unusable
	// accounts is worse than none. Use the password the endpoint reports rather than
	// the suite's own: they were equal by coincidence once, and the day they stopped
	// being equal this test failed for a reason that had nothing to do with seeding.
	e.request(e.newClient(), http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "mum@example.org", "password": seeded.Password}).
		expect(http.StatusOK)

	e.get("/dev/notifications").expect(http.StatusOK)
	e.get("/dev/mail").expect(http.StatusOK)
	e.get("/dev/state").expect(http.StatusOK)

	// A second seed is additive, not destructive.
	e.post("/dev/seed", nil).expect(http.StatusOK)
	if n, err := e.store.CountUsers(t.Context()); err != nil || n != 3 {
		t.Fatalf("users after a second seed = %d (%v), want 3", n, err)
	}
}

func TestDevTickWithoutANotifier(t *testing.T) {
	e := newEnv(t)
	// No notifier is wired in these tests; the endpoint must say so rather than panic.
	e.post("/dev/tick", nil).expect(http.StatusConflict)
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	e := newEnv(t)

	res := e.get("/healthz").expect(http.StatusOK)
	var health healthResponse
	res.decode(&health)
	if health.Status != "ok" {
		t.Fatalf("status = %q, checks = %+v", health.Status, health.Checks)
	}
	for _, key := range []string{"database", "scheduler", "backup", "mail", "disk", "push"} {
		if _, ok := health.Checks[key]; !ok {
			t.Errorf("/healthz has no %q check", key)
		}
	}
	// No secrets, ever: this endpoint is unauthenticated.
	if strings.Contains(strings.ToLower(string(res.body)), "vapid") {
		t.Errorf("/healthz leaks VAPID configuration: %s", res.body)
	}

	// A scheduler that stopped ticking is a degraded server even though every request
	// still works, because reminders have quietly stopped.
	if err := e.store.SetMeta(t.Context(), MetaSchedulerHeartbeat, e.clk.Now().Format(time.RFC3339)); err != nil {
		t.Fatalf("set heartbeat: %v", err)
	}
	e.get("/healthz").expect(http.StatusOK)

	e.clk.Advance(2 * time.Hour)
	degraded := e.get("/healthz").expect(http.StatusServiceUnavailable)
	var after healthResponse
	degraded.decode(&after)
	if after.Status != "degraded" {
		t.Errorf("status = %q with a two-hour-old heartbeat", after.Status)
	}
}

func TestHealthzNeedsNoSession(t *testing.T) {
	e := newEnv(t)
	e.request(e.newClient(), http.MethodGet, "/healthz", nil).expect(http.StatusOK)
}

// A database that is no longer on disk — a volume that failed to mount after a reboot,
// a file deleted by hand — used to be invisible here. The connection pool holds the file
// open, so Ping keeps answering for as long as the process lives, and the one field that
// looked like it was about to say otherwise, database_exists, only reported that the
// *setting* was not the empty string. So /healthz stayed green on a server whose calendar
// had gone, which is the longest possible way to find out.
func TestHealthzDegradesWhenTheDatabaseFileHasGone(t *testing.T) {
	e := newEnv(t)
	e.get("/healthz").expect(http.StatusOK)

	if err := os.Remove(e.cfg.DataPath); err != nil {
		t.Fatalf("remove the database file: %v", err)
	}

	res := e.get("/healthz").expect(http.StatusServiceUnavailable)
	var health healthResponse
	res.decode(&health)
	disk, ok := health.Checks["disk"].(map[string]any)
	if !ok {
		t.Fatalf("disk check = %#v", health.Checks["disk"])
	}
	if disk["database_exists"] != false {
		t.Errorf("database_exists = %v with no database on disk", disk["database_exists"])
	}
	if disk["ok"] != false {
		t.Errorf("disk check reports ok = %v, so nothing in the response says which check degraded the server", disk["ok"])
	}
}

// Because it needs no session, /healthz must not answer questions about a
// particular subscription. It used to report failures keyed by push service host,
// which is a member-supplied value, and so told anyone who asked whether a delivery
// to a host of their choosing had succeeded. The count that monitoring actually
// wants — is push working? — carries no such key, and the per-service breakdown an
// operator wants goes out in the daily heartbeat mail instead.
func TestHealthzDoesNotNamePushServices(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.PushHosts = []string{"push.example.org"} })
	user, _ := e.family()

	const endpoint = "https://push.example.org/subscription/abc123"
	if err := e.store.UpsertPushSubscription(t.Context(), domain.PushSubscription{
		UserID: user.ID, Endpoint: endpoint, P256DH: "BEl-key", Auth: "auth-secret", UALabel: "iPhone",
	}); err != nil {
		t.Fatalf("upsert subscription: %v", err)
	}
	subs, err := e.store.ListPushSubscriptions(t.Context(), user.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("list subscriptions: %+v (%v)", subs, err)
	}
	if err := e.store.MarkPushFailure(t.Context(), subs[0].ID); err != nil {
		t.Fatalf("mark push failure: %v", err)
	}

	res := e.request(e.newClient(), http.MethodGet, "/healthz", nil).expect(http.StatusOK)
	if strings.Contains(string(res.body), "push.example.org") {
		t.Errorf("/healthz names a push service host to an anonymous caller:\n%s", res.body)
	}

	var health healthResponse
	res.decode(&health)
	push, ok := health.Checks["push"].(map[string]any)
	if !ok {
		t.Fatalf("push check = %#v", health.Checks["push"])
	}
	if got, want := push["failing"], 1.0; got != want {
		t.Errorf("push.failing = %v, want %v: monitoring still has to be able to see that push is broken", got, want)
	}
}

// ---------------------------------------------------------------------------
// Client address handling
// ---------------------------------------------------------------------------

func TestClientIPTrustsForwardedForOnlyFromATrustedPeer(t *testing.T) {
	e := newEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.7")
	if got := e.srv.clientIP(req); got != "198.51.100.7" {
		t.Errorf("behind a trusted proxy, clientIP = %q, want the rightmost entry", got)
	}

	// The same header from an untrusted peer is a lie and is ignored, or one attacker
	// could spend everybody else's rate-limit budget.
	req.RemoteAddr = "203.0.113.200:5555"
	if got := e.srv.clientIP(req); got != "203.0.113.200" {
		t.Errorf("from an untrusted peer, clientIP = %q, want the socket address", got)
	}
}

// ---------------------------------------------------------------------------
// Misc plumbing
// ---------------------------------------------------------------------------

func TestRequestBodyLimit(t *testing.T) {
	e := newEnv(t)
	e.family()

	huge := strings.Repeat("x", maxRequestBytes+1024)
	res := e.post("/api/v1/calendars", map[string]string{"name": huge, "color": "#ffffff"})
	if res.status != http.StatusBadRequest && res.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status = %d", res.status)
	}
}

func TestRedactPathKeepsTokensOutOfLogs(t *testing.T) {
	cases := map[string]string{
		"/api/v1/invites/SECRETTOKEN": "/api/v1/invites/…",
		"/api/v1/invites/12/revoke":   "/api/v1/invites/12/revoke",
		"/join/SECRETTOKEN":           "/join/…",
		"/reset/SECRETTOKEN":          "/reset/…",
		"/api/v1/events/12":           "/api/v1/events/12",
	}
	for in, want := range cases {
		if got := redactPath(in); got != want {
			t.Errorf("redactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New with no dependencies should fail")
	}
}
