package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"
	"almanack/internal/events"
	"almanack/internal/i18n"
	"almanack/internal/mailer"
	"almanack/internal/store"
	"almanack/internal/webpush"
)

// paris is the family timezone for every test here. The DST cases are the reason
// this package exists in the form it does, so they are not an add-on: Europe/Paris
// changes offset twice a year and a reminder that drifts by an hour is a missed
// appointment.
var paris = mustLoadParis()

func mustLoadParis() *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic(err)
	}
	return loc
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// pushCall is one delivery the fake push service received. The body stays
// encrypted — this asserts on the RFC 8030 envelope, which is what the notifier
// controls; the encryption has its own test vectors in internal/webpush.
type pushCall struct {
	Path    string
	TTL     string
	Urgency string
	Topic   string
}

// fakePush is an httptest push service. Its status can be changed per endpoint so
// a test can make one subscription answer 410 Gone.
type fakePush struct {
	srv *httptest.Server

	mu     sync.Mutex
	calls  []pushCall
	status map[string]int // path → status, default 201
}

func newFakePush(t *testing.T) *fakePush {
	t.Helper()
	f := &fakePush{status: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		code, ok := f.status[r.URL.Path]
		if !ok {
			code = http.StatusCreated
		}
		f.calls = append(f.calls, pushCall{
			Path:    r.URL.Path,
			TTL:     r.Header.Get("TTL"),
			Urgency: r.Header.Get("Urgency"),
			Topic:   r.Header.Get("Topic"),
		})
		f.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePush) endpoint(name string) string { return f.srv.URL + "/push/" + name }

func (f *fakePush) setStatus(name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status["/push/"+name] = code
}

func (f *fakePush) received() []pushCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushCall(nil), f.calls...)
}

func (f *fakePush) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// stubMailer records messages instead of sending them, and can be made to fail.
type stubMailer struct {
	mu       sync.Mutex
	sent     []mailer.Message
	failures int
	fail     bool
}

func (m *stubMailer) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		m.failures++
		return fmt.Errorf("stub mailer: refusing")
	}
	m.failures = 0
	m.sent = append(m.sent, msg)
	return nil
}

func (m *stubMailer) Failures() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failures
}

func (m *stubMailer) messages() []mailer.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mailer.Message(nil), m.sent...)
}

func (m *stubMailer) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = nil
}

// ---------------------------------------------------------------------------
// The environment under test
// ---------------------------------------------------------------------------

type env struct {
	t    *testing.T
	ctx  context.Context
	st   *store.Store
	ev   *events.Service
	n    *Notifier
	clk  *clock.Fake
	push *fakePush
	mail *stubMailer
}

// newEnv builds a complete pipeline against a temp SQLite file: no network, no
// sleeping, no running server (CONVENTIONS §7).
func newEnv(t *testing.T, now time.Time) *env {
	t.Helper()
	clk := clock.NewFake(now)
	st, err := store.Open(filepath.Join(t.TempDir(), "almanack.db"), paris, clk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	fp := newFakePush(t)
	pub, priv, err := webpush.GenerateKeys()
	if err != nil {
		t.Fatalf("generate vapid keys: %v", err)
	}
	sender, err := webpush.NewSender(pub, priv, "mailto:almanack@example.org", fp.srv.Client())
	if err != nil {
		t.Fatalf("new sender: %v", err)
	}
	sender.Clock = clk

	mail := &stubMailer{}
	ev := events.New(st, paris, clk)
	n, err := New(Options{
		Store:    st,
		Events:   ev,
		Push:     sender,
		Mailer:   mail,
		Catalog:  i18n.MustLoad(),
		Clock:    clk,
		Location: paris,
		BaseURL:  "https://almanack.example.org",
	})
	if err != nil {
		t.Fatalf("new notifier: %v", err)
	}
	return &env{t: t, ctx: context.Background(), st: st, ev: ev, n: n, clk: clk, push: fp, mail: mail}
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

func (e *env) user(name string) domain.User {
	e.t.Helper()
	u, err := e.st.CreateUser(e.ctx, domain.User{
		Email:       name + "@example.org",
		DisplayName: name,
		Color:       "#1e88e5",
		Lang:        domain.LangFR,
		TimeFormat:  "24h",
		// Tests never authenticate; password hashing belongs to internal/auth.
	}, "not-a-real-hash")
	if err != nil {
		e.t.Fatalf("create user %s: %v", name, err)
	}
	return u
}

func (e *env) calendar(name string, creator int64) domain.Calendar {
	e.t.Helper()
	c, err := e.st.CreateCalendar(e.ctx, domain.Calendar{Name: name, Color: "#43a047", CreatorID: creator})
	if err != nil {
		e.t.Fatalf("create calendar %s: %v", name, err)
	}
	return c
}

func (e *env) label(calendarID int64) int64 {
	e.t.Helper()
	ls, err := e.st.ListLabels(e.ctx, calendarID)
	if err != nil || len(ls) == 0 {
		e.t.Fatalf("list labels for calendar %d: %v", calendarID, err)
	}
	return ls[0].ID
}

func (e *env) join(calendarID, userID int64) {
	e.t.Helper()
	if err := e.st.AddMember(e.ctx, calendarID, userID); err != nil {
		e.t.Fatalf("add member: %v", err)
	}
}

func (e *env) membership(calendarID, userID int64, muted, participatingOnly bool) {
	e.t.Helper()
	m, err := e.st.Membership(e.ctx, calendarID, userID)
	if err != nil {
		e.t.Fatalf("membership: %v", err)
	}
	m.Muted, m.ParticipatingOnly = muted, participatingOnly
	if err := e.st.UpdateMembership(e.ctx, m); err != nil {
		e.t.Fatalf("update membership: %v", err)
	}
}

// timedEvent creates an event at a family-tz wall clock time, which is how the
// family thinks about it and the only way to write a DST test that means anything.
func (e *env) timedEvent(cal domain.Calendar, actor int64, title string, y int, mo time.Month, d, h, min int, dur time.Duration, rec *domain.Recurrence) domain.Event {
	e.t.Helper()
	start := time.Date(y, mo, d, h, min, 0, 0, paris)
	ev, err := e.ev.Create(e.ctx, actor, events.Input{
		CalendarID:   cal.ID,
		Title:        title,
		StartsAt:     start.UTC(),
		EndsAt:       start.Add(dur).UTC(),
		LabelID:      e.label(cal.ID),
		Participants: []int64{actor},
		Recurrence:   rec,
	})
	if err != nil {
		e.t.Fatalf("create event %s: %v", title, err)
	}
	return ev
}

func (e *env) allDayEvent(cal domain.Calendar, actor int64, title string, day domain.Date) domain.Event {
	e.t.Helper()
	ev, err := e.ev.Create(e.ctx, actor, events.Input{
		CalendarID:   cal.ID,
		Title:        title,
		AllDay:       true,
		StartDate:    day,
		EndDate:      day,
		LabelID:      e.label(cal.ID),
		Participants: []int64{actor},
	})
	if err != nil {
		e.t.Fatalf("create all-day event %s: %v", title, err)
	}
	return ev
}

func (e *env) reminderMinutes(ev domain.Event, userID int64, minutes int) {
	e.t.Helper()
	e.setReminders(ev, userID, []domain.Reminder{{OffsetMinutes: &minutes}})
}

func (e *env) reminderAt(ev domain.Event, userID int64, daysBefore int, at string) {
	e.t.Helper()
	e.setReminders(ev, userID, []domain.Reminder{{DaysBefore: &daysBefore, AtTimeLocal: at}})
}

// setReminders attaches reminders to the series when the event has one, and to the
// event itself otherwise — the same choice the UI makes.
func (e *env) setReminders(ev domain.Event, userID int64, rs []domain.Reminder) {
	e.t.Helper()
	var eventID, recurrenceID *int64
	if ev.RecurrenceID != nil {
		recurrenceID = ev.RecurrenceID
	} else {
		id := ev.ID
		eventID = &id
	}
	if err := e.st.ReplaceReminders(e.ctx, eventID, recurrenceID, userID, rs); err != nil {
		e.t.Fatalf("replace reminders: %v", err)
	}
}

// subscribe registers a push subscription with real P-256 keys, so the encryption
// path runs for real rather than being stubbed out.
func (e *env) subscribe(userID int64, name string) domain.PushSubscription {
	e.t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatalf("generate subscription key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		e.t.Fatalf("generate auth secret: %v", err)
	}
	sub := domain.PushSubscription{
		UserID:   userID,
		Endpoint: e.push.endpoint(name),
		P256DH:   base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
		UALabel:  name,
	}
	if err := e.st.UpsertPushSubscription(e.ctx, sub); err != nil {
		e.t.Fatalf("upsert push subscription: %v", err)
	}
	subs, err := e.st.ListPushSubscriptions(e.ctx, userID)
	if err != nil {
		e.t.Fatalf("list push subscriptions: %v", err)
	}
	for _, s := range subs {
		if s.Endpoint == sub.Endpoint {
			return s
		}
	}
	e.t.Fatal("subscription did not persist")
	return domain.PushSubscription{}
}

func (e *env) setPrefs(p domain.NotificationPrefs) {
	e.t.Helper()
	if err := e.st.UpdatePrefs(e.ctx, p); err != nil {
		e.t.Fatalf("update prefs: %v", err)
	}
}

// noDigests turns the daily digest off for everyone, so a test about reminders
// only sees reminders.
func (e *env) noDigests() {
	e.t.Helper()
	all, err := e.st.ListAllPrefs(e.ctx)
	if err != nil {
		e.t.Fatalf("list prefs: %v", err)
	}
	for _, p := range all {
		p.DigestEnabled = false
		e.setPrefs(p)
	}
}

// ---------------------------------------------------------------------------
// Queue inspection
// ---------------------------------------------------------------------------

type queueRow struct {
	ID          int64
	UserID      int64
	Kind        domain.NotificationKind
	SourceRef   string
	DueAt       time.Time
	SentAt      time.Time
	EmailSentAt time.Time
	Skipped     string
	Payload     string
	Attempts    int
}

// queue reads the whole outbox, including sent and skipped rows, which the store
// API deliberately does not expose (it has no reason to outside a test).
func (e *env) queue() []queueRow {
	e.t.Helper()
	rows, err := e.st.DB().QueryContext(e.ctx,
		`SELECT id, user_id, kind, source_ref, due_at, COALESCE(sent_at,''), COALESCE(email_sent_at,''),
		        COALESCE(skipped,''), payload, attempts
		   FROM notification_queue ORDER BY due_at, id`)
	if err != nil {
		e.t.Fatalf("read queue: %v", err)
	}
	defer rows.Close()
	var out []queueRow
	for rows.Next() {
		var r queueRow
		var due, sent, mailed string
		if err := rows.Scan(&r.ID, &r.UserID, &r.Kind, &r.SourceRef, &due, &sent, &mailed,
			&r.Skipped, &r.Payload, &r.Attempts); err != nil {
			e.t.Fatalf("scan queue row: %v", err)
		}
		r.DueAt = mustParseInstant(e.t, due)
		if sent != "" {
			r.SentAt = mustParseInstant(e.t, sent)
		}
		if mailed != "" {
			r.EmailSentAt = mustParseInstant(e.t, mailed)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("read queue: %v", err)
	}
	return out
}

func (e *env) queueOfKind(kind domain.NotificationKind) []queueRow {
	e.t.Helper()
	var out []queueRow
	for _, r := range e.queue() {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func (e *env) payloadOf(r queueRow) payload {
	e.t.Helper()
	var p payload
	if err := json.Unmarshal([]byte(r.Payload), &p); err != nil {
		e.t.Fatalf("decode payload %q: %v", r.Payload, err)
	}
	return p
}

func mustParseInstant(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse instant %q: %v", s, err)
	}
	return v.UTC()
}

func (e *env) plan() {
	e.t.Helper()
	if err := e.n.Plan(e.ctx); err != nil {
		e.t.Fatalf("plan: %v", err)
	}
}

func (e *env) dispatch() {
	e.t.Helper()
	if err := e.n.Dispatch(e.ctx); err != nil {
		e.t.Fatalf("dispatch: %v", err)
	}
}

// wall renders an instant as family-tz wall-clock, which is how every assertion
// in these tests is phrased: "the reminder fires at 16:00 Paris" is the claim
// worth pinning, not "at 14:00Z".
func wall(t time.Time) string { return t.In(paris).Format("2006-01-02 15:04 MST") }

func date(y int, m time.Month, d int) domain.Date { return domain.NewDate(y, m, d) }
