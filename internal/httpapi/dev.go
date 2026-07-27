package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"almanack/internal/auth"
	"almanack/internal/clock"
	"almanack/internal/domain"
	"almanack/internal/events"
)

// The /dev endpoints are how the whole notification pipeline is exercised on a laptop
// with no push service and no mail server: time travel, a forced scheduler pass, the
// mail sink, and the outbox with its payloads. They are registered only when
// cfg.Dev is set — not registered and guarded, which is one careless refactor away from
// being exposed on the family server.
//
// They are deliberately unauthenticated: dev mode binds to localhost, and requiring a
// session would make the first thing a developer does be "log in to find out why login
// is broken".

func (s *Server) devRoutes() []route {
	return []route{
		{method: "GET", path: "/dev/", h: s.handleDevDashboard},
		{method: "GET", path: "/dev/dev.css", h: s.handleDevCSS},
		{method: "GET", path: "/dev/dev.js", h: s.handleDevJS},
		{method: "GET", path: "/dev/state", h: s.handleDevState},
		{method: "GET", path: "/dev/mail", h: s.handleDevMail},
		{method: "GET", path: "/dev/notifications", h: s.handleDevNotifications},
		{method: "POST", path: "/dev/clock", mutates: true, h: s.handleDevClock},
		{method: "POST", path: "/dev/tick", mutates: true, h: s.handleDevTick},
		{method: "POST", path: "/dev/seed", mutates: true, h: s.handleDevSeed},
		{method: "GET", path: "/dev/login/{userID}", mutates: true, h: s.handleDevLogin},
	}
}

// handleDevLogin signs in as any account by navigating to a link, then redirects to
// the app. It is what makes the UI reachable to a headless browser taking screenshots
// (which cannot fill a login form), and it saves a developer from typing a password
// every time they want to see the calendar as Léo rather than as Maman.
//
// It is knowingly the one state-changing GET in this codebase. That is a rule worth
// keeping for the API — a single mutating GET reopens CSRF — but it does not apply
// here: these routes are only ever registered when cfg.Dev is set, dev mode binds to
// localhost, and a link you can navigate to is the entire point. TestNoStateChangingGET
// enforces the rule everywhere else.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "userID")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalid, "user id must be a number")
		return
	}
	user, err := s.store.UserByID(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.startSession(r.Context(), w, user.ID); err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "could not start a session")
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---------------------------------------------------------------------------
// The dashboard
// ---------------------------------------------------------------------------

func (s *Server) handleDevDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dev/" {
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such dev page")
		return
	}
	var b strings.Builder
	devHeader(&b, "Almanack — dev")

	b.WriteString(`<section class="grid">`)

	b.WriteString(`<article class="card"><h2>Time travel</h2>`)
	b.WriteString(`<p class="big" id="now">…</p>`)
	b.WriteString(`<p class="muted" id="clockkind"></p>`)
	b.WriteString(`<div class="row">`)
	for _, step := range []string{"1h", "6h", "26h", "168h"} {
		b.WriteString(`<button class="btn" data-advance="` + step + `">+` + step + `</button>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div class="row"><input class="input" id="setclock" placeholder="2026-08-04T06:00:00Z">` +
		`<button class="btn" id="setclockbtn">Set</button></div>`)
	b.WriteString(`<p class="muted">The scheduler, reminders and digests all read this clock.</p>`)
	b.WriteString(`</article>`)

	b.WriteString(`<article class="card"><h2>Pipeline</h2>`)
	b.WriteString(`<div class="row"><button class="btn primary" id="tick">Run planner + dispatch</button>` +
		`<button class="btn" id="seed">Seed demo family</button></div>`)
	b.WriteString(`<p class="muted">Advance the clock, run a pass, then read the two inboxes below.</p>`)
	b.WriteString(`<pre id="result" class="result"></pre>`)
	b.WriteString(`</article>`)

	b.WriteString(`<article class="card"><h2>Inboxes</h2><ul class="links">`)
	b.WriteString(`<li><a href="/dev/mail">/dev/mail</a> — <span id="mailcount">…</span> message(s) written to ` +
		html.EscapeString(s.cfg.MailDir) + `</li>`)
	b.WriteString(`<li><a href="/dev/notifications">/dev/notifications</a> — <span id="queuecount">…</span> queued, ` +
		`<span id="sentcount">…</span> sent</li>`)
	b.WriteString(`<li><a href="/healthz">/healthz</a> — <span id="health">…</span></li>`)
	b.WriteString(`<li><a href="/">/</a> — the app itself</li>`)
	b.WriteString(`</ul></article>`)

	b.WriteString(`<article class="card"><h2>Accounts</h2><div id="users">…</div>`)
	b.WriteString(`<p class="muted">The seeder gives every demo account the password <code>motdepasse</code>.</p>`)
	b.WriteString(`</article>`)

	b.WriteString(`<article class="card"><h2>This build</h2><dl class="kv">`)
	devRow(&b, "app version", s.version)
	devRow(&b, "base URL", s.cfg.BaseURL)
	devRow(&b, "family timezone", s.cfg.TZName)
	devRow(&b, "database", s.cfg.DataPath)
	devRow(&b, "mail directory", s.cfg.MailDir)
	devRow(&b, "scheduler tick", s.cfg.SchedulerTick.String())
	devRow(&b, "plan horizon", s.cfg.PlanHorizon.String())
	devRow(&b, "VAPID public key", truncateMiddle(s.cfg.VAPIDPublic, 24))
	b.WriteString(`</dl></article>`)

	b.WriteString(`</section>`)
	devFooter(&b)
	writeHTML(w, b.String())
}

func devRow(b *strings.Builder, key, value string) {
	if value == "" {
		value = "(unset)"
	}
	b.WriteString(`<dt>` + html.EscapeString(key) + `</dt><dd>` + html.EscapeString(value) + `</dd>`)
}

func truncateMiddle(s string, keep int) string {
	if len(s) <= keep {
		return s
	}
	return s[:keep/2] + "…" + s[len(s)-keep/2:]
}

// devHeader writes the page shell. The stylesheet and script are separate routes
// because the CSP has no 'unsafe-inline' and this page is not an excuse to weaken it.
func devHeader(b *strings.Builder, title string) {
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(`<title>` + html.EscapeString(title) + `</title>`)
	b.WriteString(`<link rel="stylesheet" href="/dev/dev.css"><script src="/dev/dev.js" defer></script>`)
	b.WriteString(`</head><body><header class="top"><h1>Almanack <span class="tag">dev</span></h1><nav>`)
	b.WriteString(`<a href="/dev/">dashboard</a><a href="/dev/mail">mail</a>` +
		`<a href="/dev/notifications">notifications</a><a href="/">app</a>`)
	b.WriteString(`</nav></header><main>`)
}

func devFooter(b *strings.Builder) {
	b.WriteString(`</main><footer class="muted">Dev endpoints are never mounted outside ALMANACK_DEV=1.</footer>`)
	b.WriteString(`</body></html>`)
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

const devCSS = `:root{color-scheme:light dark;--bg:#f6f7f9;--fg:#1b1f24;--card:#fff;--line:#dfe3e8;--muted:#666;--accent:#3b7ddd}
@media (prefers-color-scheme:dark){:root{--bg:#15181c;--fg:#e8eaed;--card:#1e2227;--line:#2c3138;--muted:#9aa0a6}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
.top{display:flex;flex-wrap:wrap;gap:1rem;align-items:baseline;justify-content:space-between;
padding:1rem 1.5rem;border-bottom:1px solid var(--line);background:var(--card)}
.top h1{font-size:1.1rem;margin:0}
.tag{background:var(--accent);color:#fff;border-radius:4px;padding:.1rem .4rem;font-size:.7rem;vertical-align:middle}
nav a{margin-right:1rem;color:var(--accent);text-decoration:none}
nav a:hover{text-decoration:underline}
main{padding:1.5rem;max-width:1100px;margin:0 auto}
.grid{display:grid;gap:1rem;grid-template-columns:repeat(auto-fit,minmax(320px,1fr))}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:1rem 1.2rem}
.card h2{margin:0 0 .6rem;font-size:.95rem;text-transform:uppercase;letter-spacing:.05em;color:var(--muted)}
.big{font-size:1.5rem;margin:.2rem 0;font-variant-numeric:tabular-nums}
.row{display:flex;flex-wrap:wrap;gap:.5rem;margin:.6rem 0}
.btn{cursor:pointer;border:1px solid var(--line);background:var(--bg);color:var(--fg);
border-radius:6px;padding:.4rem .7rem;font:inherit}
.btn:hover{border-color:var(--accent)}
.btn.primary{background:var(--accent);border-color:var(--accent);color:#fff}
.input{flex:1;min-width:12rem;border:1px solid var(--line);border-radius:6px;padding:.4rem .6rem;
background:var(--bg);color:var(--fg);font:inherit}
.muted{color:var(--muted);font-size:.85rem}
.links{list-style:none;padding:0;margin:0}
.links li{padding:.25rem 0;border-bottom:1px solid var(--line)}
.links li:last-child{border:0}
a{color:var(--accent)}
.kv{display:grid;grid-template-columns:auto 1fr;gap:.25rem .8rem;margin:0;font-size:.9rem}
.kv dt{color:var(--muted)}
.kv dd{margin:0;word-break:break-all}
.result{white-space:pre-wrap;background:var(--bg);border:1px solid var(--line);border-radius:6px;
padding:.5rem;min-height:1.5rem;margin:0;font-size:.85rem}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--line);border-radius:10px}
th,td{text-align:left;padding:.5rem .7rem;border-bottom:1px solid var(--line);vertical-align:top;font-size:.9rem}
th{color:var(--muted);font-weight:600;text-transform:uppercase;font-size:.72rem;letter-spacing:.05em}
tr:last-child td{border-bottom:0}
td.payload{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.8rem;word-break:break-all;max-width:32rem}
.pill{border-radius:999px;padding:.1rem .5rem;font-size:.75rem;border:1px solid var(--line)}
.pill.sent{background:#1f7a3f22;border-color:#1f7a3f}
.pill.pending{background:#c8880022;border-color:#c88800}
.pill.skipped{background:#88888822}
.mail{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:1rem 1.2rem;margin-bottom:1rem}
.mail h3{margin:0 0 .3rem;font-size:1rem}
.mail pre{white-space:pre-wrap;margin:.5rem 0 0;font-size:.85rem}
footer{padding:1.5rem;text-align:center}
.empty{padding:2rem;text-align:center;color:var(--muted)}
`

func (s *Server) handleDevCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(devCSS))
}

const devJS = `// Dev dashboard. No framework, no inline handlers: the CSP here is the production one.
const post = async (path, body) => {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'X-Requested-With': 'almanack', 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  try { return { ok: res.ok, data: JSON.parse(text) }; } catch (_) { return { ok: res.ok, data: text }; }
};

const show = (result) => {
  const box = document.getElementById('result');
  if (box) box.textContent = typeof result === 'string' ? result : JSON.stringify(result, null, 2);
};

const refresh = async () => {
  let state;
  try { state = await (await fetch('/dev/state')).json(); } catch (_) { return; }
  const set = (id, value) => { const el = document.getElementById(id); if (el) el.textContent = value; };
  set('now', state.now);
  set('clockkind', state.clock === 'fake'
    ? 'controllable clock — time travel works'
    : 'real clock — set ALMANACK_DEV=1 with the fake clock to travel');
  set('mailcount', state.mail);
  set('queuecount', state.queued);
  set('sentcount', state.sent);
  set('health', state.health);
  const users = document.getElementById('users');
  if (users) {
    users.textContent = '';
    if (!state.users || !state.users.length) {
      users.textContent = 'No accounts yet — seed the demo family.';
    } else {
      const ul = document.createElement('ul');
      ul.className = 'links';
      for (const u of state.users) {
        const li = document.createElement('li');
        li.textContent = u.display_name + ' — ' + u.email + (u.is_admin ? ' (admin)' : '');
        ul.appendChild(li);
      }
      users.appendChild(ul);
    }
  }
};

document.addEventListener('click', async (e) => {
  const el = e.target.closest('[data-advance]');
  if (el) { show(await post('/dev/clock', { advance: el.dataset.advance })); refresh(); return; }
  if (e.target.id === 'setclockbtn') {
    const value = document.getElementById('setclock').value.trim();
    show(await post('/dev/clock', { set: value })); refresh(); return;
  }
  if (e.target.id === 'tick') { show('running…'); show((await post('/dev/tick')).data); refresh(); return; }
  if (e.target.id === 'seed') { show('seeding…'); show((await post('/dev/seed')).data); refresh(); return; }
});

refresh();
setInterval(refresh, 5000);
`

func (s *Server) handleDevJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(devJS))
}

// handleDevState feeds the dashboard's five-second refresh.
func (s *Server) handleDevState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	kind := "real"
	if _, ok := s.clock.(*clock.Fake); ok {
		kind = "fake"
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	rows, err := s.queueRows(ctx, 500)
	if err != nil {
		fail(w, r, err)
		return
	}
	queued, sent := 0, 0
	for _, row := range rows {
		if row.SentAt != "" {
			sent++
		} else if row.Skipped == "" {
			queued++
		}
	}
	health := "ok"
	if err := s.store.Ping(ctx); err != nil {
		health = "database unreachable"
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"now":    s.clock.Now().Format(time.RFC3339),
		"clock":  kind,
		"users":  users,
		"mail":   len(s.mailFiles()),
		"queued": queued,
		"sent":   sent,
		"health": health,
	})
}

// ---------------------------------------------------------------------------
// Time travel, a forced pass, and the seeder
// ---------------------------------------------------------------------------

type devClockRequest struct {
	Advance string `json:"advance"`
	Set     string `json:"set"`
}

// handleDevClock moves the fake clock. This is what makes tomorrow's digest testable
// today: advance 26 hours, run a pass, read the two inboxes.
func (s *Server) handleDevClock(w http.ResponseWriter, r *http.Request) {
	fake, ok := s.clock.(*clock.Fake)
	if !ok {
		fail(w, r, fmt.Errorf("%w: this server runs on the real clock; start it with the fake clock to travel in time", domain.ErrConflict))
		return
	}
	var req devClockRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	switch {
	case req.Advance != "":
		d, err := time.ParseDuration(req.Advance)
		if err != nil {
			fail(w, r, invalidf("advance must be a Go duration such as 26h"))
			return
		}
		fake.Advance(d)
	case req.Set != "":
		t, err := time.Parse(time.RFC3339, req.Set)
		if err != nil {
			fail(w, r, invalidf("set must be an RFC 3339 instant such as 2026-08-04T06:00:00Z"))
			return
		}
		fake.Set(t)
	default:
		fail(w, r, invalidf(`send {"advance":"26h"} or {"set":"2026-08-04T06:00:00Z"}`))
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"now": s.clock.Now().Format(time.RFC3339)})
}

// handleDevTick runs one planner + dispatch pass immediately, instead of waiting for
// the scheduler's next tick.
func (s *Server) handleDevTick(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		fail(w, r, fmt.Errorf("%w: no notifier is wired into this server", domain.ErrConflict))
		return
	}
	if err := s.notifier.Tick(r.Context()); err != nil {
		fail(w, r, err)
		return
	}
	rows, err := s.queueRows(r.Context(), 500)
	if err != nil {
		fail(w, r, err)
		return
	}
	queued, sent := 0, 0
	for _, row := range rows {
		if row.SentAt != "" {
			sent++
		} else if row.Skipped == "" {
			queued++
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"ok": true, "now": s.clock.Now().Format(time.RFC3339), "queued": queued, "sent": sent,
	})
}

// devPassword is the demo family's password, matching docs/development.md.
const devPassword = "motdepasse"

// handleDevSeed fills an empty database with a family that has something to look at. On
// a database that already has accounts it adds a few events instead, so pressing the
// button twice is not destructive.
func (s *Server) handleDevSeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	if count > 0 {
		added, err := s.seedEvents(ctx)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{
			"seeded": false, "events_added": added,
			"note": "the database already had accounts; only events were added",
		})
		return
	}

	people := []struct {
		email, name, color string
		admin              bool
	}{
		{"maman@example.org", "Maman", "#c0392b", true},
		{"papa@example.org", "Papa", "#2980b9", false},
		{"leo@example.org", "Léo", "#27ae60", false},
	}
	hash, err := auth.HashPassword(devPassword)
	if err != nil {
		fail(w, r, err)
		return
	}

	var created []domain.User
	for _, p := range people {
		u, err := s.store.CreateUser(ctx, domain.User{
			Email: p.email, DisplayName: p.name, Color: p.color,
			Lang: domain.LangFR, WeekStart: time.Monday, TimeFormat: "24h", IsAdmin: p.admin,
		}, hash)
		if err != nil {
			fail(w, r, err)
			return
		}
		if err := s.store.UpdatePrefs(ctx, defaultPrefs(u.ID)); err != nil {
			fail(w, r, err)
			return
		}
		created = append(created, u)
	}

	cal, err := s.store.CreateCalendar(ctx, domain.Calendar{
		Name: "Famille", Color: "#3b7ddd", CreatorID: created[0].ID,
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	for _, u := range created[1:] {
		if err := s.store.AddMember(ctx, cal.ID, u.ID); err != nil {
			fail(w, r, err)
			return
		}
	}

	added, err := s.seedEvents(ctx)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"seeded": true, "users": len(created), "calendar": cal.Name, "events_added": added,
		"password": devPassword,
	})
}

// seedEvents adds a timed appointment, a weekly series and a short holiday to the first
// calendar, positioned relative to the current (possibly travelled) clock so that the
// reminder and digest paths have something to fire on.
func (s *Server) seedEvents(ctx context.Context) (int, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil || len(users) == 0 {
		return 0, err
	}
	actor := users[0]
	cals, err := s.store.ListCalendarsForUser(ctx, actor.ID)
	if err != nil {
		return 0, err
	}
	if len(cals) == 0 {
		return 0, nil
	}
	cal := cals[0]
	labels, err := s.store.ListLabels(ctx, cal.ID)
	if err != nil || len(labels) < 5 {
		return 0, err
	}

	loc := s.cfg.FamilyTZ
	today := domain.DateIn(s.clock.Now(), loc)
	tomorrow := today.AddDays(1)
	everyone := make([]int64, 0, len(users))
	for _, u := range users {
		everyone = append(everyone, u.ID)
	}

	inputs := []events.Input{
		{
			CalendarID: cal.ID, Title: "Dentiste Léo", LabelID: labels[3].ID,
			StartsAt:     tomorrow.At(14, 30, loc).UTC(),
			EndsAt:       tomorrow.At(15, 15, loc).UTC(),
			Location:     "Cabinet du centre",
			Participants: []int64{users[len(users)-1].ID},
		},
		{
			CalendarID: cal.ID, Title: "Piscine", LabelID: labels[4].ID,
			StartsAt: today.At(17, 0, loc).UTC(), EndsAt: today.At(18, 0, loc).UTC(),
			Participants: everyone,
			Recurrence:   &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{today.Weekday()}},
		},
		{
			CalendarID: cal.ID, Title: "Vacances à la mer", LabelID: labels[7].ID,
			AllDay: true, StartDate: today.AddDays(7), EndDate: today.AddDays(13),
			Participants: everyone,
		},
	}

	added := 0
	for _, in := range inputs {
		if _, err := s.events.Create(ctx, actor.ID, in); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

// ---------------------------------------------------------------------------
// The two inboxes
// ---------------------------------------------------------------------------

func (s *Server) mailFiles() []string {
	if s.cfg.MailDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.cfg.MailDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".eml") {
			names = append(names, e.Name())
		}
	}
	// The sink names files with a sortable timestamp prefix, so newest first is a
	// reverse sort — no stat call per file.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// handleDevMail renders the .eml files the dev sink wrote. Password resets, reminders
// and digests all land here, fully rendered, on a laptop with no mail server.
func (s *Server) handleDevMail(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	devHeader(&b, "Almanack — dev mail")
	b.WriteString(`<p class="muted">` + html.EscapeString(orUnset(s.cfg.MailDir)) + `</p>`)

	names := s.mailFiles()
	if len(names) == 0 {
		b.WriteString(`<p class="empty">No mail yet. Request a password reset, or advance the clock and run a pass.</p>`)
		devFooter(&b)
		writeHTML(w, b.String())
		return
	}

	const maxShown = 50
	if len(names) > maxShown {
		names = names[:maxShown]
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(s.cfg.MailDir, name))
		if err != nil {
			continue
		}
		headers, body := splitMessage(string(data))
		b.WriteString(`<article class="mail">`)
		b.WriteString(`<h3>` + html.EscapeString(decodeHeader(headers["subject"])) + `</h3>`)
		b.WriteString(`<p class="muted">to ` + html.EscapeString(headers["to"]) +
			` · ` + html.EscapeString(headers["date"]) + ` · ` + html.EscapeString(name) + `</p>`)
		b.WriteString(`<pre>` + html.EscapeString(body) + `</pre>`)
		b.WriteString(`</article>`)
	}
	devFooter(&b)
	writeHTML(w, b.String())
}

func orUnset(s string) string {
	if s == "" {
		return "(no mail directory configured)"
	}
	return s
}

// splitMessage separates RFC 5322 headers from the body. It is deliberately minimal:
// these are files this application wrote minutes ago, not arbitrary internet mail.
func splitMessage(raw string) (map[string]string, string) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	head, body, found := strings.Cut(raw, "\n\n")
	if !found {
		head, body = raw, ""
	}
	headers := map[string]string{}
	for _, line := range strings.Split(head, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return headers, body
}

func decodeHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

// queueRow is one outbox row as the dev page shows it. The instants stay strings: this
// view exists to show exactly what is in the table.
type queueRow struct {
	ID        int64
	UserID    int64
	Kind      string
	SourceRef string
	Payload   string
	DueAt     string
	SentAt    string
	Skipped   string
	Attempts  int
}

// queueRows reads the outbox, sent rows included.
//
// This is the one place in the application outside internal/store that touches SQL, and
// it is deliberately dev-only and read-only: the store has no "list everything in the
// queue" method because nothing in production needs one, and a dev inbox that hid
// everything the moment it was delivered would be useless for exactly the thing it is
// for. If internal/store ever grows that method, this should call it instead.
func (s *Server) queueRows(ctx context.Context, limit int) ([]queueRow, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT id, user_id, kind, source_ref, payload, due_at, sent_at, skipped, attempts
		  FROM notification_queue
		 ORDER BY due_at DESC, id DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read notification queue: %w", err)
	}
	defer rows.Close()

	var out []queueRow
	for rows.Next() {
		var q queueRow
		var sentAt, skipped sql.NullString
		if err := rows.Scan(&q.ID, &q.UserID, &q.Kind, &q.SourceRef, &q.Payload,
			&q.DueAt, &sentAt, &skipped, &q.Attempts); err != nil {
			return nil, fmt.Errorf("read notification queue: %w", err)
		}
		q.SentAt, q.Skipped = sentAt.String, skipped.String
		out = append(out, q)
	}
	return out, rows.Err()
}

// handleDevNotifications shows the outbox with its payloads, sent and unsent alike.
// This is where you check that the right person is being notified, at the right time,
// in the right language — without a push service.
func (s *Server) handleDevNotifications(w http.ResponseWriter, r *http.Request) {
	rows, err := s.queueRows(r.Context(), 200)
	if err != nil {
		fail(w, r, err)
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	names := map[int64]string{}
	for _, u := range users {
		names[u.ID] = u.DisplayName
	}

	var b strings.Builder
	devHeader(&b, "Almanack — dev notifications")
	if len(rows) == 0 {
		b.WriteString(`<p class="empty">The outbox is empty. Seed the demo family, advance the clock, then run a pass.</p>`)
		devFooter(&b)
		writeHTML(w, b.String())
		return
	}
	b.WriteString(`<table><thead><tr><th>due</th><th>state</th><th>who</th><th>kind</th>` +
		`<th>source</th><th>payload</th></tr></thead><tbody>`)
	for _, q := range rows {
		state, pill := "queued", "pending"
		switch {
		case q.SentAt != "":
			state, pill = "sent "+q.SentAt, "sent"
		case q.Skipped != "":
			state, pill = "skipped: "+q.Skipped, "skipped"
		}
		who := names[q.UserID]
		if who == "" {
			who = fmt.Sprintf("user %d", q.UserID)
		}
		b.WriteString(`<tr>`)
		b.WriteString(`<td>` + html.EscapeString(q.DueAt) + `</td>`)
		b.WriteString(`<td><span class="pill ` + pill + `">` + html.EscapeString(state) + `</span></td>`)
		b.WriteString(`<td>` + html.EscapeString(who) + `</td>`)
		b.WriteString(`<td>` + html.EscapeString(q.Kind) + `</td>`)
		b.WriteString(`<td>` + html.EscapeString(q.SourceRef) + `</td>`)
		b.WriteString(`<td class="payload">` + html.EscapeString(q.Payload) + `</td>`)
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table>`)
	devFooter(&b)
	writeHTML(w, b.String())
}
