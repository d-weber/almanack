// Package httpapi is the HTTP layer: routing, middleware, the JSON API described by
// docs/api.md (normative), and the embedded PWA.
//
// The web assets arrive as an fs.FS rather than being embedded here, because go:embed
// cannot reach outside its own package directory: main.go embeds web/ and passes it in.
//
// Two rules run through the whole package and are enforced centrally rather than per
// handler, because a handler that forgets is a security hole:
//
//   - every non-GET/HEAD/OPTIONS request must carry "X-Requested-With: almanack"
//     (with SameSite=Lax this is the entire CSRF defense — there is no token);
//   - mutations are never GET. The route table records which routes mutate, and a test
//     walks it.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"almanack/internal/clock"
	"almanack/internal/config"
	"almanack/internal/events"
	"almanack/internal/i18n"
	"almanack/internal/mailer"
	"almanack/internal/store"
)

// Notifier is the part of internal/notify this package needs: one pass of the planner
// and the dispatcher. It is declared here, as an interface, so that the HTTP layer
// depends on a behaviour rather than on a concrete type it does not own.
type Notifier interface {
	// Tick runs one planning + delivery pass. POST /dev/tick calls it.
	Tick(ctx context.Context) error
}

// TestSender is optionally implemented by a Notifier. When it is, POST /api/v1/push/test
// delegates to it; when it is not, the endpoint queues a test notification instead and
// reports how many devices it was queued for.
type TestSender interface {
	SendTest(ctx context.Context, userID int64) (int, error)
}

// SchedulerHealth is optionally implemented by a Notifier. When it is, /healthz asks the
// scheduler itself when it last completed a pass, instead of reading the heartbeat the
// store may or may not hold. It is declared structurally — no import of internal/notify —
// so that neither package has to know about the other to work.
type SchedulerHealth interface {
	// Heartbeat is when the scheduler last completed a pass; zero means never.
	Heartbeat() time.Time
	// TickInterval is how often it means to run, which is what makes a heartbeat age
	// judgeable.
	TickInterval() time.Duration
}

// Keys in the store's meta table that /healthz reads. They are exported so that the
// backup subcommand writes the keys this package looks for, rather than the two sides
// guessing at strings.
const (
	// MetaSchedulerHeartbeat is an RFC 3339 instant. It is only consulted when the wired
	// notifier does not report its own heartbeat (see SchedulerHealth), which is how a
	// scheduler living in another process would still be visible here.
	MetaSchedulerHeartbeat = "scheduler_heartbeat"
	// MetaLastBackupAt is an RFC 3339 instant, written when a backup finishes.
	MetaLastBackupAt = "last_backup_at"
	// MetaLastBackupResult is "ok" or a short failure reason.
	MetaLastBackupResult = "last_backup_result"
)

// Deps is everything the server needs. main.go builds it; nothing here reaches for a
// global.
type Deps struct {
	Store    *store.Store
	Events   *events.Service
	Notifier Notifier      // optional: /dev/tick and push tests degrade without it
	Mailer   mailer.Mailer // optional: password-reset mail is skipped (and logged) without it
	Catalog  *i18n.Catalog
	Clock    clock.Clock
	Config   config.Config
	Web      fs.FS // the embedded web/ tree
}

// Server is the whole HTTP application.
type Server struct {
	store    *store.Store
	events   *events.Service
	notifier Notifier
	mailer   mailer.Mailer
	catalog  *i18n.Catalog
	clock    clock.Clock
	cfg      config.Config

	assets  map[string]asset
	version string

	limiter   *limiterSet
	proxies   []trustedPeer
	startedAt time.Time

	routes  []route
	handler http.Handler
}

// route is one registered endpoint. mutates is recorded rather than inferred so that a
// test can assert the invariant "mutations are never GET" against the actual table.
type route struct {
	method  string
	path    string
	auth    bool
	mutates bool
	h       http.HandlerFunc
}

func (r route) pattern() string { return r.method + " " + r.path }

// New builds the server: it validates dependencies, loads the web assets into memory
// and computes the application version from them.
func New(deps Deps) (*Server, error) {
	switch {
	case deps.Store == nil:
		return nil, errors.New("httpapi: Deps.Store is required")
	case deps.Events == nil:
		return nil, errors.New("httpapi: Deps.Events is required")
	case deps.Catalog == nil:
		return nil, errors.New("httpapi: Deps.Catalog is required")
	case deps.Clock == nil:
		return nil, errors.New("httpapi: Deps.Clock is required")
	case deps.Config.FamilyTZ == nil:
		return nil, errors.New("httpapi: Deps.Config.FamilyTZ is required")
	}

	assets, version, err := loadAssets(deps.Web)
	if err != nil {
		return nil, fmt.Errorf("httpapi: load web assets: %w", err)
	}

	s := &Server{
		store:     deps.Store,
		events:    deps.Events,
		notifier:  deps.Notifier,
		mailer:    deps.Mailer,
		catalog:   deps.Catalog,
		clock:     deps.Clock,
		cfg:       deps.Config,
		assets:    assets,
		version:   version,
		limiter:   newLimiterSet(deps.Clock),
		proxies:   parseTrustedPeers(deps.Config.TrustedProxies),
		startedAt: deps.Clock.Now(),
	}
	s.build()
	return s, nil
}

// Handler returns the root handler, middleware and all.
func (s *Server) Handler() http.Handler { return s.handler }

// AppVersion is the short hex hash of the embedded web assets. It rides on every
// response as X-App-Version; the client hard-reloads when it changes.
func (s *Server) AppVersion() string { return s.version }

// build registers every route and wraps the mux in the middleware chain.
func (s *Server) build() {
	s.routes = []route{
		// -- public ---------------------------------------------------------
		{method: "GET", path: "/api/v1/config", h: s.handleConfig},
		{method: "GET", path: "/api/v1/invites/{token}", h: s.handleInvitePreview},
		{method: "POST", path: "/api/v1/auth/signup", mutates: true, h: s.handleSignup},
		{method: "POST", path: "/api/v1/auth/login", mutates: true, h: s.handleLogin},
		{method: "POST", path: "/api/v1/auth/password-reset/request", mutates: true, h: s.handleResetRequest},
		{method: "POST", path: "/api/v1/auth/password-reset/confirm", mutates: true, h: s.handleResetConfirm},

		// -- session --------------------------------------------------------
		{method: "POST", path: "/api/v1/auth/logout", auth: true, mutates: true, h: s.handleLogout},
		{method: "GET", path: "/api/v1/me", auth: true, h: s.handleMe},
		{method: "PATCH", path: "/api/v1/me", auth: true, mutates: true, h: s.handlePatchMe},
		{method: "PUT", path: "/api/v1/me/avatar", auth: true, mutates: true, h: s.handlePutAvatar},
		{method: "DELETE", path: "/api/v1/me/avatar", auth: true, mutates: true, h: s.handleDeleteAvatar},
		{method: "GET", path: "/api/v1/users/{id}/avatar", auth: true, h: s.handleUserAvatar},

		// -- calendars ------------------------------------------------------
		{method: "POST", path: "/api/v1/calendars", auth: true, mutates: true, h: s.handleCreateCalendar},
		{method: "PATCH", path: "/api/v1/calendars/{id}", auth: true, mutates: true, h: s.handlePatchCalendar},
		{method: "DELETE", path: "/api/v1/calendars/{id}", auth: true, mutates: true, h: s.handleDeleteCalendar},
		{method: "POST", path: "/api/v1/calendars/{id}/leave", auth: true, mutates: true, h: s.handleLeaveCalendar},
		{method: "PATCH", path: "/api/v1/calendars/{id}/membership", auth: true, mutates: true, h: s.handlePatchMembership},
		{method: "DELETE", path: "/api/v1/calendars/{id}/members/{user_id}", auth: true, mutates: true, h: s.handleRemoveMember},
		{method: "PATCH", path: "/api/v1/calendars/{id}/labels/{label_id}", auth: true, mutates: true, h: s.handlePatchLabel},
		{method: "POST", path: "/api/v1/calendars/{id}/invites", auth: true, mutates: true, h: s.handleCreateInvite},
		{method: "GET", path: "/api/v1/calendars/{id}/invites", auth: true, h: s.handleListInvites},
		{method: "PUT", path: "/api/v1/calendars/{id}/image", auth: true, mutates: true, h: s.handlePutCalendarImage},
		{method: "DELETE", path: "/api/v1/calendars/{id}/image", auth: true, mutates: true, h: s.handleDeleteCalendarImage},
		{method: "GET", path: "/api/v1/calendars/{id}/image", auth: true, h: s.handleCalendarImage},
		{method: "POST", path: "/api/v1/invites/{id}/revoke", auth: true, mutates: true, h: s.handleRevokeInvite},

		// -- events ---------------------------------------------------------
		{method: "GET", path: "/api/v1/events", auth: true, h: s.handleListEvents},
		{method: "POST", path: "/api/v1/events", auth: true, mutates: true, h: s.handleCreateEvent},
		{method: "GET", path: "/api/v1/events/{id}", auth: true, h: s.handleGetEvent},
		{method: "PATCH", path: "/api/v1/events/{id}", auth: true, mutates: true, h: s.handleUpdateEvent},
		{method: "DELETE", path: "/api/v1/events/{id}", auth: true, mutates: true, h: s.handleDeleteEvent},
		{method: "PUT", path: "/api/v1/events/{id}/reminders", auth: true, mutates: true, h: s.handlePutReminders},
		{method: "GET", path: "/api/v1/search", auth: true, h: s.handleSearch},

		// -- notifications --------------------------------------------------
		{method: "GET", path: "/api/v1/prefs", auth: true, h: s.handleGetPrefs},
		{method: "PATCH", path: "/api/v1/prefs", auth: true, mutates: true, h: s.handlePatchPrefs},
		{method: "POST", path: "/api/v1/push/subscription", auth: true, mutates: true, h: s.handlePushSubscribe},
		{method: "DELETE", path: "/api/v1/push/subscription", auth: true, mutates: true, h: s.handlePushUnsubscribe},
		{method: "POST", path: "/api/v1/push/confirm", auth: true, mutates: true, h: s.handlePushConfirm},
		{method: "POST", path: "/api/v1/push/test", auth: true, mutates: true, h: s.handlePushTest},
		{method: "GET", path: "/api/v1/activity", auth: true, h: s.handleActivity},

		// -- operational ----------------------------------------------------
		{method: "GET", path: "/healthz", h: s.handleHealthz},
		{method: "GET", path: "/locales/{name}", h: s.handleLocale},
	}

	// The /dev endpoints exist only in dev mode. They are not registered otherwise —
	// not registered-and-guarded, which would be one refactor away from being exposed.
	if s.cfg.Dev {
		s.routes = append(s.routes, s.devRoutes()...)
	}

	mux := http.NewServeMux()
	for _, r := range s.routes {
		h := r.h
		if r.auth {
			h = s.requireSession(h)
		}
		mux.Handle(r.pattern(), h)
	}
	// Anything under /api/ that matched no route is a JSON 404, never the SPA shell.
	mux.HandleFunc("/api/", s.handleAPINotFound)
	mux.HandleFunc("/", s.serveStatic)

	s.handler = recoverer(requestLogger(s.securityHeaders(bodyLimit(s.csrf(mux)))))
}

// handleAPINotFound answers unknown API paths in the error envelope. Without it they
// would fall through to the SPA fallback and a fetch() would parse an HTML page.
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, codeNotFound, "no such endpoint")
}
