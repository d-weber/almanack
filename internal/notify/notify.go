// Package notify is the notification engine: the planner that materializes the
// durable outbox, the scheduler that drains it, and the boot catch-up policy that
// decides what a server which was switched off for a week still owes the family.
//
// # The shape of the thing
//
// Nothing is delivered from the code that caused it. An edit writes an
// activity_log row; a reminder is a row in `reminders`; a digest is a preference.
// The planner turns all three into rows of `notification_queue` up to a horizon
// ahead (48 h by default), and the scheduler delivers rows that have come due.
// The indirection is the point: a crash between the edit and the notification
// cannot lose the notification, because the notification was never in memory.
//
// # At-least-once, deliberately
//
// `sent_at` is written only after a provider accepted the message. A crash
// between the accept and the mark duplicates a notification. That is the correct
// trade for this application: a duplicate reminder is an annoyance, a missed one
// is the failure the whole program exists to prevent. The tempting "fix" —
// marking before sending — silently converts delivery to at-most-once and is
// forbidden here and in internal/store.
//
// # Idempotency is structural
//
// UNIQUE(user_id, kind, source_ref, due_at) plus INSERT OR IGNORE means the
// planner may recompute the same window on every 30-second tick and only genuinely
// new work lands. It also means a row that was skipped or sent is never
// resurrected by a later pass. Source references are built by internal/events, so
// the planner that writes them and the editor that prunes them cannot drift.
//
// # Time
//
// Every instant comes from a clock.Clock. All date bucketing — which day a digest
// covers, when "09:00 the day before" is — happens in the family timezone, which
// is what keeps a 16:30 appointment 30 minutes away from its reminder on both
// sides of a daylight-saving change.
package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"
	"almanack/internal/events"
	"almanack/internal/i18n"
	"almanack/internal/mailer"
	"almanack/internal/store"
	"almanack/internal/webpush"
)

// Defaults for the two knobs the operator can turn (ALMANACK_PLAN_HORIZON, ALMANACK_TICK).
const (
	// DefaultHorizon is how far ahead the planner materializes the outbox.
	DefaultHorizon = 48 * time.Hour
	// DefaultTick is how often the scheduler plans and drains.
	DefaultTick = 30 * time.Second
)

// Meta keys. They live in the store's small key/value table, which is the only
// state the notifier keeps outside the queue itself.
const (
	// MetaPlannedThrough is the instant the planner has materialized up to.
	// CatchUp reads it to size the backfill after an outage; without it a week
	// of downtime would leave reminders that were never materialized at all,
	// which no amount of "deliver overdue rows" logic can recover.
	MetaPlannedThrough = "notify.planned_through"

	// MetaActivityCursor is the highest activity_log id already turned into
	// notifications. Reading the log rather than notifying from the edit path is
	// what makes a crash between the two harmless.
	MetaActivityCursor = "notify.activity_cursor"

	// MetaActivityCursorAt, MetaActivityCursorCalendar and MetaActivityCursorUID are
	// the instant, the calendar and the name of the row MetaActivityCursor names: the
	// witness that says the id still means what it meant when it was written.
	// activity_log.id is reused, so the number on its own cannot say whether the log
	// has been rebuilt underneath it — see repairCursor. A database written before
	// these keys existed has none of them, which reads as "cannot be vouched for" and
	// costs one repair pass.
	//
	// The name is the one part of the witness a reused id cannot imitate at all, and
	// it is here because the other two can be: calendars.id is reused exactly as
	// activity_log.id is, and dev mode's clock does not move on its own, so "delete a
	// calendar, make a calendar, add an event" puts the id, the calendar and the second
	// all back at once. A witness with no name against a row that has one is a
	// disagreement, which is what a database written before this key was added looks
	// like — one repair, and the next write records it.
	//
	// They are separate keys rather than a richer value under the first so that the
	// first goes on holding a plain integer. Not so that a binary from the previous
	// release can read it — it refuses to start against a schema it does not know, and
	// meta is not versioned anyway — but so that the value stays what it says it is to
	// anyone reading the table: a row of `meta` is one of the few places in this
	// database a person looks by hand when notifications have gone quiet, and an
	// integer that has quietly become JSON is a worse thing to meet there than three
	// keys.
	MetaActivityCursorAt       = "notify.activity_cursor_at"
	MetaActivityCursorCalendar = "notify.activity_cursor_calendar"
	MetaActivityCursorUID      = "notify.activity_cursor_uid"
)

// Policy constants. Each one is a decision, not a tuning parameter; the comments
// say which decision.
const (
	// lateThreshold is how late a row may be before the staleness policy applies
	// at all. Within it, delivery is unconditional: a reminder that is thirty
	// seconds past its slot in normal operation is simply on time, and running an
	// "is the event still ahead?" test on it would skip every at-the-start
	// reminder the moment the tick landed a second after the event began.
	lateThreshold = 5 * time.Minute

	// maxDigestLateness is the notification rules in docs/architecture.md: deliver today's digest if its slot
	// passed less than four hours ago, and drop older ones rather than pushing
	// yesterday's plan at someone.
	maxDigestLateness = 4 * time.Hour

	// maxActivityLateness matches the 24 h TTL activity pushes are sent with.
	// News about an edit from last week is not news.
	maxActivityLateness = 24 * time.Hour

	// maxBackfill bounds how much history CatchUp will re-materialize. Beyond it
	// every reminder would be stale-skipped anyway, and expanding a year of
	// recurrences to prove it is pure cost.
	maxBackfill = 30 * 24 * time.Hour

	// baseRetryBackoff and maxRetryBackoff space out the retries of a row that
	// keeps failing: min(30s << attempts, 1h).
	//
	// There is no attempt cap to go with them, and adding one back would be a
	// regression. Retirement is a question about the thing being announced —
	// staleness above answers it for every kind — and a counter answers a
	// different question badly: ten attempts one tick apart is five minutes, so a
	// push service having a bad afternoon used to permanently retire a reminder
	// for an appointment the next morning. What a counter was really standing in
	// for is this backoff.
	baseRetryBackoff = 30 * time.Second
	maxRetryBackoff  = time.Hour

	// drainBatch and maxDrainPasses bound one drain. The passes exist for the
	// catch-up case, where a week of queue may be waiting.
	drainBatch     = 100
	maxDrainPasses = 200
)

// defaultMinPlausible is the floor the scheduler refuses to run below. A server
// whose CMOS battery died boots in 1970 or 2000; with such a clock the planner
// would materialize nothing and the drain would either flush the entire queue or
// bury it forever. Refusing loudly is the only safe answer, and systemd's
// After=time-sync.target is the other half of it (docs/deployment.md).
var defaultMinPlausible = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Options are the dependencies of a Notifier. Everything except Push and Mailer
// is required.
type Options struct {
	// Store is the database. The queue, the reminders and the meta cursors live there.
	Store *store.Store
	// Events expands occurrences and applies the per-user visibility rules
	// (muted calendars, participating-only), so that no notification kind
	// reimplements them.
	Events *events.Service
	// Push may be nil when VAPID is not configured. The notifier then degrades to
	// email rather than refusing to start: a family server without push keys
	// still has to send reminders.
	Push *webpush.Sender
	// PushHosts are the hostnames a subscription endpoint may point at
	// (ALMANACK_PUSH_HOSTS). Empty means domain.DefaultPushHosts — a caller that
	// forgets to pass it gets the safe answer rather than an open door. The HTTP
	// layer refuses a bad endpoint at registration; this covers rows written
	// before it did, and rows whose host has since been narrowed out of the list.
	PushHosts []string
	// Mailer may be nil, which disables the email channel.
	Mailer mailer.Mailer
	// Catalog supplies every user-visible string and the French date formatting
	// Go's stdlib cannot do.
	Catalog *i18n.Catalog
	// Clock is the sole source of now.
	Clock clock.Clock
	// Location is the family timezone: the frame in which every date bucketing
	// decision is made.
	Location *time.Location
	// BaseURL is the public origin, used to build absolute links in email. Push
	// payloads carry app-relative URLs (see docs/api.md).
	BaseURL string
	// Horizon defaults to DefaultHorizon.
	Horizon time.Duration
	// Tick defaults to DefaultTick.
	Tick time.Duration
	// MinPlausibleTime is the clock floor described above. Zero means 2026-01-01.
	MinPlausibleTime time.Time

	// OwnerEmail and HeartbeatTime configure the daily note to whoever runs the
	// server. Leave either empty to disable it — at the cost of the only signal
	// that says the machine is still doing its job.
	OwnerEmail    string
	HeartbeatTime string
}

// Notifier plans, delivers and catches up. One instance is created at startup and
// shared between the scheduler goroutine and the HTTP handlers that read Health,
// so every mutable field is behind mu.
type Notifier struct {
	st        *store.Store
	ev        *events.Service
	push      *webpush.Sender
	pushHosts []string
	mail      mailer.Mailer
	cat       *i18n.Catalog
	clk       clock.Clock
	loc       *time.Location
	baseURL   string
	horizon   time.Duration
	tick      time.Duration
	minTime   time.Time

	mu           sync.Mutex
	lastTick     time.Time
	lastTickReal time.Time // wall time of the last tick, for clock-step detection
	lastErr      string
	pushFailures map[string]int
	caughtUp     bool
	catchUp      CatchUpSummary

	// planMu admits one planning pass at a time. Two can genuinely overlap — the
	// scheduler goroutine runs one every tick, POST /dev/tick runs another on the
	// request's goroutine — and they are not merely racy on `planned` below but wrong
	// together: reconcile deletes every undelivered row the pass no longer calls for,
	// so a pass reading half of another's decisions deletes reminders that are wanted.
	// Serialising is also the cheaper answer, since the second pass has nothing left to
	// do by the time it runs.
	//
	// It is separate from mu, which is held only for the duration of a field read and
	// must never be held across the database work a pass does.
	planMu sync.Mutex
	// planned collects what the pass in progress decided should exist, so that
	// reconcile can delete undelivered rows that are no longer called for. It is
	// non-nil only for the duration of a planning pass, and is guarded by planMu.
	planned map[string]bool

	ownerEmail  string
	heartbeatAt string
}

// New builds a Notifier. It fails when a required dependency is missing, and —
// loudly, on purpose — when the clock is implausibly early: a scheduler that
// starts with a dead-battery clock does more damage than one that does not start.
func New(o Options) (*Notifier, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("notify: nil store")
	case o.Events == nil:
		return nil, errors.New("notify: nil events service")
	case o.Catalog == nil:
		return nil, errors.New("notify: nil catalog")
	case o.Clock == nil:
		return nil, errors.New("notify: nil clock")
	case o.Location == nil:
		return nil, errors.New("notify: nil family location")
	}
	n := &Notifier{
		st:           o.Store,
		ev:           o.Events,
		push:         o.Push,
		pushHosts:    o.PushHosts,
		mail:         o.Mailer,
		cat:          o.Catalog,
		clk:          o.Clock,
		loc:          o.Location,
		baseURL:      trimSlash(o.BaseURL),
		horizon:      o.Horizon,
		tick:         o.Tick,
		minTime:      o.MinPlausibleTime,
		pushFailures: map[string]int{},
		ownerEmail:   o.OwnerEmail,
		heartbeatAt:  o.HeartbeatTime,
	}
	if n.horizon <= 0 {
		n.horizon = DefaultHorizon
	}
	if n.tick <= 0 {
		n.tick = DefaultTick
	}
	if n.minTime.IsZero() {
		n.minTime = defaultMinPlausible
	}
	if err := n.checkClock(n.now()); err != nil {
		return nil, err
	}
	return n, nil
}

// Horizon is how far ahead the outbox is materialized.
func (n *Notifier) Horizon() time.Duration { return n.horizon }

// Tick interval, exposed so /healthz can judge whether a heartbeat is stale.
func (n *Notifier) TickInterval() time.Duration { return n.tick }

// now is the single point where this package reads the clock, truncated to the
// second because that is the resolution the store's TEXT instants keep. Comparing
// a due_at that has been through the database against a raw time.Now() otherwise
// produces off-by-a-fraction results that are impossible to reproduce.
func (n *Notifier) now() time.Time { return n.clk.Now().UTC().Truncate(time.Second) }

// checkClock enforces the floor described on defaultMinPlausible.
func (n *Notifier) checkClock(now time.Time) error {
	if now.Before(n.minTime) {
		return fmt.Errorf("notify: the clock reads %s, before the earliest plausible time %s: refusing to plan or deliver (check time synchronisation and the hardware clock)",
			now.Format(time.RFC3339), n.minTime.Format(time.RFC3339))
	}
	return nil
}

// Health is what /healthz reports about the notification pipeline.
type Health struct {
	// LastTick is when the scheduler last completed a pass. Its age is the
	// difference between a wedged scheduler and a working one.
	LastTick time.Time `json:"last_tick,omitzero"`
	// QueueDepth counts rows that are neither sent nor skipped and fall inside
	// the planning horizon.
	QueueDepth int `json:"queue_depth"`
	// Overdue counts queued rows whose slot has already passed. A number that
	// does not return to zero means delivery is failing.
	Overdue int `json:"overdue"`
	// PushFailures is the consecutive failure count per push service host. Push
	// services drift (docs/architecture.md — push services drift even though the RFCs do not); this is the
	// signal that says a patch is needed.
	PushFailures map[string]int `json:"push_failures,omitempty"`
	// MailFailures is the mailer's consecutive failure count.
	MailFailures int `json:"mail_failures"`
	// LastError is the last tick error, empty when the last tick was clean.
	LastError string `json:"last_error,omitempty"`
	// ClockOK is false when the system clock is implausibly early.
	ClockOK bool `json:"clock_ok"`
	// CatchUp is the summary of the boot catch-up, for the ops heartbeat.
	CatchUp CatchUpSummary `json:"catch_up"`
}

// Health reports the counters /healthz and the ops heartbeat need. It takes a
// context because queue depth is a query, not a cached number: a stale depth is
// worse than no depth.
func (n *Notifier) Health(ctx context.Context) Health {
	now := n.now()
	h := Health{ClockOK: n.checkClock(now) == nil}

	n.mu.Lock()
	h.LastTick = n.lastTick
	h.LastError = n.lastErr
	h.CatchUp = n.catchUp
	h.PushFailures = make(map[string]int, len(n.pushFailures))
	for k, v := range n.pushFailures {
		h.PushFailures[k] = v
	}
	n.mu.Unlock()

	if n.mail != nil {
		h.MailFailures = n.mail.Failures()
	}
	if pending, err := n.st.ListUnsentBefore(ctx, now.Add(n.horizon)); err == nil {
		h.QueueDepth = len(pending)
		for _, q := range pending {
			if q.DueAt.Before(now) {
				h.Overdue++
			}
		}
	} else if h.LastError == "" {
		h.LastError = err.Error()
	}
	return h
}

// Heartbeat is when the scheduler last completed a tick. The systemd watchdog and
// /healthz both read it; a zero value means the scheduler has not run yet.
func (n *Notifier) Heartbeat() time.Time {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastTick
}

// noteTick records the outcome of a scheduler pass.
func (n *Notifier) noteTick(at time.Time, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !at.IsZero() {
		n.lastTick = at
	}
	if err != nil {
		n.lastErr = err.Error()
	} else {
		n.lastErr = ""
	}
}

// notePush maintains the per-service consecutive failure counters. Keying on the
// endpoint host rather than the whole endpoint is deliberate twice over: it is
// the granularity at which push services actually break, and the endpoint path is
// a bearer capability for one device that must never reach a log or a health page.
func (n *Notifier) notePush(endpoint string, err error) {
	host := serviceOf(endpoint)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err == nil {
		delete(n.pushFailures, host)
		return
	}
	n.pushFailures[host]++
}

func serviceOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return u.Host
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// parseHM parses the "HH:MM" family-time strings the schema stores for digest
// times and all-day reminders.
func parseHM(s string) (hour, min int, ok bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	for i, c := range []byte(s) {
		if i == 2 {
			continue
		}
		if c < '0' || c > '9' {
			return 0, 0, false
		}
	}
	hour = int(s[0]-'0')*10 + int(s[1]-'0')
	min = int(s[3]-'0')*10 + int(s[4]-'0')
	if hour > 23 || min > 59 {
		return 0, 0, false
	}
	return hour, min, true
}

// daysCovering lists the family-tz dates that [from, to] touches.
func daysCovering(from, to time.Time, loc *time.Location) []domain.Date {
	first := domain.DateIn(from, loc)
	last := domain.DateIn(to, loc)
	n := first.DaysUntil(last)
	if n < 0 {
		return nil
	}
	out := make([]domain.Date, 0, n+1)
	for i := 0; i <= n; i++ {
		out = append(out, first.AddDays(i))
	}
	return out
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
