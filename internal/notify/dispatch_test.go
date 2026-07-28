package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"almanack/internal/domain"
	"almanack/internal/events"
	"almanack/internal/i18n"
	"almanack/internal/webpush"
)

// decodePush reads the JSON the notifier handed to webpush.Send. It is reached
// through render rather than through the fake service, because the body on the
// wire is encrypted — the ciphertext is internal/webpush's business and has its
// own RFC 8291 vectors.
func decodePush(t *testing.T, b []byte) pushPayload {
	t.Helper()
	var pp pushPayload
	if err := json.Unmarshal(b, &pp); err != nil {
		t.Fatalf("decode push payload %q: %v", b, err)
	}
	return pp
}

func (e *env) recipientOf(t *testing.T, userID int64) recipient {
	t.Helper()
	u, err := e.st.UserByID(e.ctx, userID)
	if err != nil {
		t.Fatalf("user %d: %v", userID, err)
	}
	p, err := e.st.Prefs(e.ctx, userID)
	if err != nil {
		t.Fatalf("prefs %d: %v", userID, err)
	}
	return recipient{user: u, prefs: p}
}

// TestErrGoneDeletesSubscription: a 404 or 410 is terminal. If the row is not
// deleted the endpoint answers the same way forever, and the failure counter never
// catches it because the service replies promptly every single time.
func TestErrGoneDeletesSubscription(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	dead := e.subscribe(u.ID, "old-iphone")
	live := e.subscribe(u.ID, "laptop")
	e.push.setStatus("old-iphone", http.StatusGone)

	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC)) // 08:30 Paris, the slot
	e.dispatch()

	subs, err := e.st.ListPushSubscriptions(e.ctx, u.ID)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	if len(subs) != 1 || subs[0].Endpoint != live.Endpoint {
		t.Fatalf("subscriptions after a 410 = %+v, want only %s", subs, live.Endpoint)
	}
	for _, s := range subs {
		if s.Endpoint == dead.Endpoint {
			t.Fatalf("the subscription that answered 410 survived; it will answer 410 forever")
		}
	}

	// The live device still got its reminder, and the row is done.
	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 || rows[0].SentAt.IsZero() {
		t.Fatalf("reminder row = %+v, want one row marked sent", rows)
	}
}

// TestEmailIsAParallelChannel is the iOS silent-death case, and the reason
// email_reminders defaults on: the push service returned 201, the notification
// will never be displayed, and nothing in the delivery result says so. Email must
// go out anyway — it is not a fallback waiting for an error that never comes.
func TestEmailIsAParallelChannel(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")

	ev := e.timedEvent(cal, u.ID, "Dentiste Léo", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	e.dispatch()

	if got := len(e.push.received()); got != 1 {
		t.Fatalf("push deliveries = %d, want 1", got)
	}
	msgs := e.mail.messages()
	if len(msgs) != 1 {
		t.Fatalf("emails = %d, want 1 even though the push succeeded", len(msgs))
	}
	if msgs[0].To != u.Email {
		t.Errorf("email went to %q, want %q", msgs[0].To, u.Email)
	}
	if !strings.Contains(msgs[0].Subject, "Dentiste Léo") {
		t.Errorf("email subject = %q, want it to name the event", msgs[0].Subject)
	}
	if !strings.Contains(msgs[0].Text, "https://almanack.example.org/#/event/") {
		t.Errorf("email body = %q, want an absolute link back to the app", msgs[0].Text)
	}
}

// TestEmailFollowsPreferences: email_digest defaults off, email_reminders on.
func TestEmailFollowsPreferences(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 3, 0, 0, 0, time.UTC))
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestEnabled: true, DigestTime: "07:30", SummaryTime: "20:00",
		EmailReminders: false, EmailDigest: true,
	})
	e.timedEvent(cal, u.ID, "Marché", 2027, 6, 1, 10, 0, time.Hour, nil)
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)

	e.plan()
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	e.dispatch()

	msgs := e.mail.messages()
	if len(msgs) != 1 {
		t.Fatalf("emails = %d, want exactly the digest", len(msgs))
	}
	if !strings.Contains(msgs[0].Subject, "journée") {
		t.Errorf("email subject = %q, want the digest subject (mail.subject.digest)", msgs[0].Subject)
	}
}

// TestPushHeaderMatrix pins the notification rules in docs/architecture.md's delivery contract. These headers
// are the difference between a reminder that wakes a sleeping phone and one that
// arrives after the appointment.
func TestPushHeaderMatrix(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 3, 0, 0, 0, time.UTC))
	u := e.user("alice")
	other := e.user("bruno")
	cal := e.calendar("Famille", u.ID)
	e.join(cal.ID, other.ID)
	e.subscribe(u.ID, "iphone")
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestEnabled: true, DigestTime: "07:30", DigestOnEmpty: true,
		SummaryTime: "20:00", EmailReminders: true, ActivityPush: true,
	})

	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()                                                                // sets the activity cursor
	e.timedEvent(cal, other.ID, "Course", 2027, 6, 3, 9, 0, time.Hour, nil) // someone else's change
	e.plan()

	// Each kind is drained on its own so the assertions cannot depend on the
	// order rows happen to come out of the queue.
	one := func(at time.Time, what string) pushCall {
		t.Helper()
		e.clk.Set(at)
		e.push.reset()
		e.dispatch()
		calls := e.push.received()
		if len(calls) != 1 {
			t.Fatalf("expected exactly one %s push, got %d", what, len(calls))
		}
		return calls[0]
	}

	activity := one(time.Date(2027, 6, 1, 4, 0, 0, 0, time.UTC), "activity")
	digest := one(time.Date(2027, 6, 1, 5, 30, 0, 0, time.UTC), "digest")     // 07:30 Paris
	reminder := one(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC), "reminder") // 30 min before the event

	if activity.Urgency != string(webpush.UrgencyLow) || activity.TTL != "86400" {
		t.Errorf("activity headers = TTL %s / Urgency %s, want 86400 / low", activity.TTL, activity.Urgency)
	}
	if activity.Topic != "" {
		t.Errorf("activity Topic = %q, want none (each change stands alone)", activity.Topic)
	}
	if digest.Urgency != string(webpush.UrgencyNormal) || digest.TTL != "21600" {
		t.Errorf("digest headers = TTL %s / Urgency %s, want 21600 / normal", digest.TTL, digest.Urgency)
	}
	if digest.Topic != "digest" {
		t.Errorf("digest Topic = %q, want \"digest\" so a superseded digest collapses instead of stacking", digest.Topic)
	}
	if reminder.Urgency != string(webpush.UrgencyHigh) {
		t.Errorf("reminder Urgency = %s, want high", reminder.Urgency)
	}
	if reminder.TTL != "1800" {
		t.Errorf("reminder TTL = %s, want 1800 — min(time to the event, 6 h)", reminder.TTL)
	}
}

// TestReminderTTLIsCappedAndFloored: the cap keeps a week-ahead reminder from
// asking a push service to hold a message for a week; the floor keeps it off
// zero, which means "discard unless the device is connected right now".
func TestReminderTTLIsCappedAndFloored(t *testing.T) {
	now := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		p    payload
		want time.Duration
	}{
		{"a day away is capped at six hours", payload{EventStart: now.Add(24 * time.Hour)}, 6 * time.Hour},
		{"two hours away", payload{EventStart: now.Add(2 * time.Hour)}, 2 * time.Hour},
		{"already started is floored, not zeroed", payload{EventStart: now.Add(-time.Hour)}, minReminderTTL},
		{"an all-day event is relevant all day", payload{AllDay: true, EventDate: date(2027, 6, 1)}, 6 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reminderTTL(tc.p, now); got != tc.want {
				t.Errorf("reminderTTL = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestDigestPayloadStaysUnderTheCeiling is the aes128gcm limit made into a test:
// a busy Saturday with long French titles must still produce a body a push
// service will accept, because webpush.Send rejects an oversize payload before it
// ever reaches the network and the notification would simply never arrive.
func TestDigestPayloadStaysUnderTheCeiling(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 3, 0, 0, 0, time.UTC))
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestEnabled: true, DigestTime: "07:30", SummaryTime: "20:00",
		EmailReminders: true, EmailDigest: true,
	})

	const long = "Répétition générale du spectacle de fin d'année à l'école élémentaire Jean-Baptiste Corot"
	for i := 0; i < 24; i++ {
		e.timedEvent(cal, u.ID, fmt.Sprintf("%s — séance %d", long, i+1), 2027, 6, 1, 8+i%12, i%60, time.Hour, nil)
	}
	e.plan()

	rows := e.queueOfKind(domain.KindDigest)
	if len(rows) == 0 {
		t.Fatal("no digest was planned")
	}
	row := rows[0]
	p, err := e.n.fillDigest(e.ctx, u.ID, e.payloadOf(row))
	if err != nil {
		t.Fatalf("fill digest: %v", err)
	}
	if p.Total != 24 {
		t.Fatalf("digest total = %d, want 24", p.Total)
	}

	body, email, err := e.n.render(p, e.recipientOf(t, u.ID), row.SourceRef, e.clk.Now())
	if err != nil {
		t.Fatalf("render digest: %v", err)
	}
	if len(body) > webpush.MaxPayloadBytes {
		t.Fatalf("digest push payload is %d bytes, over the %d limit", len(body), webpush.MaxPayloadBytes)
	}
	pp := decodePush(t, body)
	if !strings.Contains(pp.Body, "24 événements") {
		t.Errorf("digest body = %q, want it to carry the count", pp.Body)
	}
	if !strings.Contains(pp.Body, "de plus") {
		t.Errorf("digest body = %q, want it to say how many titles were left out", pp.Body)
	}
	// Email has no such ceiling and carries everything the push had to drop.
	if email == nil || !strings.Contains(email.Text, "24 événements") {
		t.Errorf("digest email = %+v, want the full list", email)
	}

	// And end to end: the row is actually delivered, which is the real proof that
	// webpush.Send did not reject it.
	e.clk.Set(time.Date(2027, 6, 1, 5, 30, 0, 0, time.UTC))
	e.dispatch()
	for _, r := range e.queueOfKind(domain.KindDigest) {
		if r.ID == row.ID && r.SentAt.IsZero() {
			t.Fatalf("the digest was not delivered: %+v", r)
		}
	}
}

// TestPayloadShrinksBeforeItIsDropped: even a pathological single title must not
// produce an undeliverable push. Displaying *something* is what keeps an iOS
// subscription alive — Apple revokes after roughly three silent pushes.
func TestPayloadShrinksBeforeItIsDropped(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 3, 0, 0, 0, time.UTC))
	u := e.user("alice")
	to := e.recipientOf(t, u.ID)

	p := payload{Kind: domain.KindDigest, Day: date(2027, 6, 1), Total: 3}
	for i := 0; i < 3; i++ {
		p.Items = append(p.Items, digestItem{Title: strings.Repeat("é", 4000), StartsAt: e.clk.Now()})
	}
	body, _, err := e.n.render(p, to, "digest:2027-06-01", e.clk.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(body) > webpush.MaxPayloadBytes {
		t.Fatalf("payload is %d bytes, over the %d limit", len(body), webpush.MaxPayloadBytes)
	}
	if pp := decodePush(t, body); pp.Title == "" || pp.Body == "" {
		t.Errorf("shrunk payload = %+v, want it to still display something", pp)
	}
}

// TestTextIsLocalizedPerRecipient: two people, one language each, one event. The
// wording is composed at delivery time from users.lang, so a language change
// between planning and delivery takes effect immediately.
func TestTextIsLocalizedPerRecipient(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	fr := e.user("alice")
	en := e.user("bruno")
	u, err := e.st.UserByID(e.ctx, en.ID)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	u.Lang = domain.LangEN
	if err := e.st.UpdateUser(e.ctx, u); err != nil {
		t.Fatalf("update user: %v", err)
	}

	p := payload{
		Kind:       domain.KindReminder,
		EventID:    12,
		OccDate:    date(2027, 6, 2),
		Title:      "Dentiste Léo",
		EventStart: time.Date(2027, 6, 2, 14, 30, 0, 0, paris).UTC(),
	}
	now := time.Date(2027, 6, 1, 10, 0, 0, 0, time.UTC)

	frBody, frMail, err := e.n.render(p, e.recipientOf(t, fr.ID), "reminder:12:2027-06-02:1", now)
	if err != nil {
		t.Fatalf("render fr: %v", err)
	}
	enBody, enMail, err := e.n.render(p, e.recipientOf(t, en.ID), "reminder:12:2027-06-02:1", now)
	if err != nil {
		t.Fatalf("render en: %v", err)
	}

	frPush, enPush := decodePush(t, frBody), decodePush(t, enBody)
	if frPush.Body != "Demain à 14:30" {
		t.Errorf("French body = %q, want %q", frPush.Body, "Demain à 14:30")
	}
	if enPush.Body != "Tomorrow at 14:30" {
		t.Errorf("English body = %q, want %q", enPush.Body, "Tomorrow at 14:30")
	}
	if frPush.Lang != "fr" || enPush.Lang != "en" {
		t.Errorf("payload langs = %q / %q, want fr / en", frPush.Lang, enPush.Lang)
	}
	if frPush.URL != "/#/event/12/2027-06-02" {
		t.Errorf("deep link = %q, want /#/event/12/2027-06-02", frPush.URL)
	}
	if frPush.Tag != "reminder:12:2027-06-02:1" {
		t.Errorf("tag = %q, want the source reference so a duplicate replaces rather than stacks", frPush.Tag)
	}
	if frMail == nil || !strings.HasPrefix(frMail.Subject, "Rappel") {
		t.Errorf("French subject = %+v, want it to start with Rappel", frMail)
	}
	if enMail == nil || !strings.HasPrefix(enMail.Subject, "Reminder") {
		t.Errorf("English subject = %+v, want it to start with Reminder", enMail)
	}
}

// TestAllDayReminderText: an all-day event has no time of day, so the wording
// must not invent one.
func TestAllDayReminderText(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	u := e.user("alice")
	p := payload{
		Kind: domain.KindReminder, EventID: 3, OccDate: date(2027, 6, 1),
		Title: "Sortie scolaire", AllDay: true, EventDate: date(2027, 6, 1),
		Location: "Musée",
	}
	body, _, err := e.n.render(p, e.recipientOf(t, u.ID), "reminder:3:2027-06-01:1", e.clk.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pp := decodePush(t, body)
	if pp.Body != "Aujourd'hui · Journée entière · Musée" {
		t.Errorf("body = %q, want %q", pp.Body, "Aujourd'hui · Journée entière · Musée")
	}
}

// TestDegradesToEmailWithoutVAPID: an unconfigured push sender must not crash the
// scheduler or block delivery. A family server with no VAPID keys still has to
// send reminders.
func TestDegradesToEmailWithoutVAPID(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)

	n, err := New(Options{
		Store: e.st, Events: e.ev, Push: nil, Mailer: e.mail,
		Catalog: i18n.MustLoad(), Clock: e.clk, Location: paris,
		BaseURL: "https://almanack.example.org",
	})
	if err != nil {
		t.Fatalf("new notifier without VAPID: %v", err)
	}
	if err := n.Plan(e.ctx); err != nil {
		t.Fatalf("plan: %v", err)
	}
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	if err := n.Dispatch(e.ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if got := len(e.push.received()); got != 0 {
		t.Errorf("push deliveries = %d, want 0 with no sender configured", got)
	}
	if got := len(e.mail.messages()); got != 1 {
		t.Errorf("emails = %d, want 1", got)
	}
	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 || rows[0].SentAt.IsZero() {
		t.Errorf("reminder row = %+v, want it delivered by email alone", rows)
	}
}

// TestPushIsNotSentToAnEndpointOutsideTheAllowlist covers the rows that are already
// in the table. Registration is the real gate (internal/httpapi refuses an endpoint
// whose host is not a push service), but a row written before that gate existed, or
// one whose host an operator has since narrowed out of ALMANACK_PUSH_HOSTS, must not
// be dialled either — a subscription endpoint is a URL somebody else chose, and the
// last thing between it and a request is this check.
//
// It skips the device rather than counting a failure against it: a notification held
// open for an endpoint that will never be tried would be retried until it went stale,
// and the endpoint is not what is broken.
func TestPushIsNotSentToAnEndpointOutsideTheAllowlist(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	sub := e.subscribe(u.ID, "iphone")
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)

	// The same pipeline, with an allowlist that does not name the fake service the
	// subscription points at.
	n, err := New(Options{
		Store: e.st, Events: e.ev, Push: e.n.push, Mailer: e.mail,
		Catalog: i18n.MustLoad(), Clock: e.clk, Location: paris,
		BaseURL: "https://almanack.example.org", PushHosts: []string{"web.push.apple.com"},
	})
	if err != nil {
		t.Fatalf("new notifier: %v", err)
	}
	if err := n.Plan(e.ctx); err != nil {
		t.Fatalf("plan: %v", err)
	}
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	if err := n.Dispatch(e.ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if got := e.push.received(); len(got) != 0 {
		t.Fatalf("the endpoint was dialled anyway: %+v", got)
	}
	// The other channel is unaffected, and the row is finished rather than left to
	// be retried against a device that will never be tried.
	if got := len(e.mail.messages()); got != 1 {
		t.Errorf("emails = %d, want 1", got)
	}
	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 || rows[0].SentAt.IsZero() {
		t.Errorf("reminder row = %+v, want it retired by the email leg", rows)
	}

	// The subscription itself is left alone. When this fires it is at least as
	// likely that the allowlist is wrong as that the device is, and deleting a
	// family member's registration over a configuration edit would be a poor trade.
	subs, err := e.st.ListPushSubscriptions(e.ctx, u.ID)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	if len(subs) != 1 || subs[0].ID != sub.ID {
		t.Fatalf("subscriptions = %+v, want the row left in place", subs)
	}
	if subs[0].Failures != 0 {
		t.Errorf("subscription failure count = %d; not dialling a device is not a delivery failure", subs[0].Failures)
	}
}

// TestSentAtIsWrittenOnlyAfterAcceptance is the at-least-once contract. When every
// provider refuses, the row stays queued and is retried; it is never marked sent
// on the way in. Reversing this is the "fix" that silently turns delivery into
// at-most-once, so it gets a test of its own.
func TestSentAtIsWrittenOnlyAfterAcceptance(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	sub := e.subscribe(u.ID, "iphone")
	e.push.setStatus("iphone", http.StatusInternalServerError)
	e.mail.fail = true

	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	e.dispatch()

	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 {
		t.Fatalf("got %d reminder rows, want 1", len(rows))
	}
	if !rows[0].SentAt.IsZero() {
		t.Fatal("the row was marked sent even though no provider accepted it")
	}
	if rows[0].Skipped != "" {
		t.Fatalf("the row was retired after one failure: %q", rows[0].Skipped)
	}
	subs, err := e.st.ListPushSubscriptions(e.ctx, u.ID)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	if len(subs) != 1 || subs[0].ID != sub.ID {
		t.Fatalf("a 500 deleted the subscription; only 404/410 may do that: %+v", subs)
	}
	if subs[0].Failures != 1 {
		t.Errorf("subscription failure count = %d, want 1", subs[0].Failures)
	}
	if h := e.n.Health(e.ctx); len(h.PushFailures) != 1 {
		t.Errorf("Health.PushFailures = %+v, want one service counted", h.PushFailures)
	}

	// The service comes back; the retry succeeds and only then is sent_at written.
	// The clock moves on because a failed row is not retried in the same breath —
	// see TestRetriesBackOff.
	e.push.setStatus("iphone", http.StatusCreated)
	e.mail.fail = false
	e.clk.Set(time.Date(2027, 6, 1, 6, 32, 0, 0, time.UTC))
	e.dispatch()
	rows = e.queueOfKind(domain.KindReminder)
	if rows[0].SentAt.IsZero() {
		t.Fatal("the retry did not deliver")
	}
	if h := e.n.Health(e.ctx); len(h.PushFailures) != 0 {
		t.Errorf("Health.PushFailures = %+v, want the counter cleared after a success", h.PushFailures)
	}
}

// TestUndeliverableRowIsRetired: a row that never succeeds must not be retried
// until 2040. It is retired as a skip with a reason, which is evidence, not
// silence — and the reason is that the appointment it warned about has happened,
// not that some attempt counter ran out while it was still hours away.
func TestUndeliverableRowIsRetired(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.push.setStatus("iphone", http.StatusInternalServerError)
	e.mail.fail = true

	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 23, 30, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()
	e.clk.Set(time.Date(2027, 6, 1, 21, 0, 0, 0, time.UTC)) // 23:00 Paris, the slot
	e.dispatch()

	// While the appointment is ahead the row is kept, however badly the last
	// attempt went.
	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 {
		t.Fatalf("got %d reminder rows, want 1", len(rows))
	}
	if rows[0].Skipped != "" {
		t.Fatalf("a row for an appointment two hours away was retired: %q", rows[0].Skipped)
	}

	// The appointment happens. Now there is nothing left to warn about.
	e.clk.Set(time.Date(2027, 6, 2, 0, 0, 0, 0, time.UTC)) // 02:00 Paris, half an hour late
	e.dispatch()

	rows = e.queueOfKind(domain.KindReminder)
	if rows[0].Skipped == "" {
		t.Fatalf("the row is still being retried after the appointment: %+v", rows[0])
	}
	if !rows[0].SentAt.IsZero() {
		t.Error("a retired row must not be recorded as sent")
	}
}

// TestAnAcceptedPushDoesNotCoverForAFailedEmail is the bug this whole file is
// arranged around. Push is the channel that cannot be trusted — an iOS
// subscription dies with the service still answering 201 — so email exists to
// cover for it. Counting one acceptance across both channels made the untrusted
// one vouch for the trustworthy one, and the mail was dropped with a log line.
func TestAnAcceptedPushDoesNotCoverForAFailedEmail(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone") // answers 201
	e.mail.fail = true          // the MTA is briefly down

	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	e.dispatch()

	if got := len(e.push.received()); got != 1 {
		t.Fatalf("push deliveries = %d, want 1", got)
	}
	if got := len(e.mail.messages()); got != 0 {
		t.Fatalf("emails = %d, want 0 — the MTA refused", got)
	}
	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 {
		t.Fatalf("got %d reminder rows, want 1", len(rows))
	}
	if !rows[0].SentAt.IsZero() {
		t.Fatal("the push acceptance retired the row; the email is now lost, and it was the channel that exists because push lies")
	}
	if rows[0].Skipped != "" {
		t.Fatalf("the row was retired after one failure: %q", rows[0].Skipped)
	}
	if !rows[0].EmailSentAt.IsZero() {
		t.Error("email_sent_at was written for an email the MTA refused")
	}

	// The MTA comes back and the outstanding leg goes out.
	e.mail.fail = false
	e.clk.Set(time.Date(2027, 6, 1, 6, 32, 0, 0, time.UTC))
	e.dispatch()

	msgs := e.mail.messages()
	if len(msgs) != 1 {
		t.Fatalf("emails after the MTA recovered = %d, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Subject, "Dentiste") {
		t.Errorf("email subject = %q, want it to name the event", msgs[0].Subject)
	}
	rows = e.queueOfKind(domain.KindReminder)
	if rows[0].SentAt.IsZero() {
		t.Fatalf("the row was not retired once both channels had gone: %+v", rows[0])
	}
	if rows[0].EmailSentAt.IsZero() {
		t.Error("the email went but email_sent_at was not recorded")
	}
}

// TestTheEmailLegIsNotSentAgainOnARetry is the guard that makes retrying safe.
// Once the row is retried until its event is past rather than ten times, an email
// leg that is re-sent on every attempt means the family is mailed the same
// reminder every backoff interval for hours. The recorded leg is skipped instead.
func TestTheEmailLegIsNotSentAgainOnARetry(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.push.setStatus("iphone", http.StatusInternalServerError) // the push service is down
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestTime: "07:30", SummaryTime: "20:00", EmailReminders: true,
	})

	// The appointment is a day off, so the row stays relevant across every retry
	// below and nothing here depends on the staleness policy.
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 2, 10, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 24*60)
	e.plan()

	slot := time.Date(2027, 6, 1, 8, 0, 0, 0, time.UTC) // 10:00 Paris, the day before
	e.clk.Set(slot)
	e.dispatch()

	if got := len(e.mail.messages()); got != 1 {
		t.Fatalf("emails = %d, want 1: the mail leg succeeded even though push did not", got)
	}
	row := e.queueOfKind(domain.KindReminder)[0]
	if row.EmailSentAt.IsZero() {
		t.Fatal("the email went out but email_sent_at was not recorded, so the next retry will send it again")
	}
	if !row.SentAt.IsZero() {
		t.Fatal("the row was retired while the push leg had not been accepted")
	}

	// Six hours of retries, spaced well past the longest backoff.
	for i := 1; i <= 6; i++ {
		e.clk.Set(slot.Add(time.Duration(i) * time.Hour))
		e.dispatch()
	}
	if got := len(e.mail.messages()); got != 1 {
		t.Fatalf("emails after six hours of push failures = %d, want 1; the family has been mailed the same reminder %d times", got, got)
	}
	if got := len(e.push.received()); got < 6 {
		t.Errorf("push attempts = %d, want the retries to have kept trying", got)
	}

	// The push service comes back. The row retires, and still only one email.
	e.push.setStatus("iphone", http.StatusCreated)
	e.clk.Set(slot.Add(7 * time.Hour))
	e.dispatch()

	row = e.queueOfKind(domain.KindReminder)[0]
	if row.SentAt.IsZero() {
		t.Fatalf("the row was not retired once push recovered: %+v", row)
	}
	if got := len(e.mail.messages()); got != 1 {
		t.Errorf("emails = %d, want 1 for one reminder", got)
	}
}

// TestAFailedSubscriptionLookupDoesNotRetireTheRow: the read that finds a person's
// devices can fail like any other. Logging it and carrying on left subs nil, so
// the push leg was silently skipped and the row was marked sent — the only
// notification failure mode that leaves no trace anywhere.
func TestAFailedSubscriptionLookupDoesNotRetireTheRow(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()

	e.breakSubscriptionReads()
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	e.dispatch()

	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 {
		t.Fatalf("got %d reminder rows, want 1", len(rows))
	}
	if !rows[0].SentAt.IsZero() {
		t.Fatal("a row was marked sent although the devices to send it to could not be read")
	}
	if rows[0].Skipped != "" {
		t.Fatalf("the row was retired on a database error: %q", rows[0].Skipped)
	}
	if got := len(e.push.received()); got != 0 {
		t.Errorf("push deliveries = %d, want 0", got)
	}

	// The database answers again and the reminder goes out.
	e.subscriptionReadsWorkAgain()
	e.clk.Set(time.Date(2027, 6, 1, 6, 32, 0, 0, time.UTC))
	e.dispatch()

	rows = e.queueOfKind(domain.KindReminder)
	if rows[0].SentAt.IsZero() {
		t.Fatalf("the retry did not deliver: %+v", rows[0])
	}
	if got := len(e.push.received()); got != 1 {
		t.Errorf("push deliveries = %d, want 1", got)
	}
}

// TestAnOutageOutlastingTheOldAttemptCapStillDelivers: retirement is a question
// about the event, not about a counter. Ten attempts one tick apart is five
// minutes; a push service is down for longer than that on a bad afternoon, and
// the reminder it retired was for something the next morning.
func TestAnOutageOutlastingTheOldAttemptCapStillDelivers(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.push.setStatus("iphone", http.StatusInternalServerError)
	e.setPrefs(domain.NotificationPrefs{ // push only, so nothing else can retire the row
		UserID: u.ID, DigestTime: "07:30", SummaryTime: "20:00", EmailReminders: false,
	})

	// A reminder a day ahead of the appointment: the row is due at 10:00 on the
	// 2nd for a swimming lesson at 10:00 on the 3rd.
	ev := e.timedEvent(cal, u.ID, "Piscine", 2027, 6, 3, 10, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 24*60)
	e.plan()

	slot := time.Date(2027, 6, 2, 8, 0, 0, 0, time.UTC) // 10:00 Paris, the day before
	const outageHours = 20
	for i := 0; i < outageHours; i++ {
		e.clk.Set(slot.Add(time.Duration(i) * time.Hour))
		e.dispatch()
	}

	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 {
		t.Fatalf("got %d reminder rows, want 1", len(rows))
	}
	if rows[0].Skipped != "" {
		t.Fatalf("a %d-hour outage retired a reminder for a lesson that has not happened yet: %q",
			outageHours, rows[0].Skipped)
	}
	if rows[0].Attempts <= 10 {
		t.Fatalf("the row was attempted %d times over %d hours; delivery gave up early",
			rows[0].Attempts, outageHours)
	}

	// The service comes back with two hours to spare, and the reminder still lands.
	e.push.setStatus("iphone", http.StatusCreated)
	e.clk.Set(time.Date(2027, 6, 3, 6, 0, 0, 0, time.UTC)) // 08:00 Paris
	e.dispatch()

	rows = e.queueOfKind(domain.KindReminder)
	if rows[0].SentAt.IsZero() {
		t.Fatalf("the reminder was not delivered once push recovered: %+v", rows[0])
	}
}

// TestRetiredOnceTheEventIsPast is the other half: nothing became immortal when
// the attempt cap went. A row whose event has happened is retired on the next
// pass, whatever its attempt count.
func TestRetiredOnceTheEventIsPast(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.push.setStatus("iphone", http.StatusInternalServerError)
	e.mail.fail = true

	ev := e.timedEvent(cal, u.ID, "Piscine", 2027, 6, 3, 10, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 24*60)
	e.plan()
	e.clk.Set(time.Date(2027, 6, 2, 8, 0, 0, 0, time.UTC))
	e.dispatch()

	// The lesson comes and goes with the outage still on.
	e.clk.Set(time.Date(2027, 6, 3, 12, 0, 0, 0, time.UTC)) // 14:00 Paris, four hours late
	e.dispatch()

	rows := e.queueOfKind(domain.KindReminder)
	if rows[0].Skipped == "" {
		t.Fatalf("the row survived its own event: %+v", rows[0])
	}
	if !rows[0].SentAt.IsZero() {
		t.Error("a retired row must not be recorded as sent")
	}
	if got := len(e.queue()); got == 0 {
		t.Error("the skipped row was deleted; the reason it carries is the evidence")
	}
}

// TestRetryBackoffCurve pins the schedule. Retrying a failing row every 30 seconds
// for as long as it stays relevant is up to ~5,700 requests per subscription
// across a 48-hour reminder window, which push services rate-limit.
func TestRetryBackoffCurve(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{0, 0},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{6, 32 * time.Minute},
		{7, time.Hour},
		{8, time.Hour},
		{100, time.Hour},
	} {
		if got := retryBackoff(tc.attempts); got != tc.want {
			t.Errorf("retryBackoff(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// TestRetriesBackOff: the curve, observed through the scheduler. A tick that lands
// inside the backoff window must not touch the providers at all.
func TestRetriesBackOff(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.push.setStatus("iphone", http.StatusInternalServerError)
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestTime: "07:30", SummaryTime: "20:00", EmailReminders: false,
	})

	ev := e.timedEvent(cal, u.ID, "Piscine", 2027, 6, 2, 10, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 24*60)
	e.plan()

	slot := time.Date(2027, 6, 1, 8, 0, 0, 0, time.UTC)
	// after is the offset from the slot at which a tick lands; want is how many
	// times the push service should have been called by then. The first failure
	// buys a minute, the second two, the third four.
	for _, tc := range []struct {
		after time.Duration
		want  int
	}{
		{0, 1},
		{29 * time.Second, 1},
		{59 * time.Second, 1},
		{time.Minute, 2},
		{2 * time.Minute, 2},
		{3*time.Minute + 1*time.Second, 3},
		{5 * time.Minute, 3},
		{7*time.Minute + 2*time.Second, 4},
	} {
		e.clk.Set(slot.Add(tc.after))
		e.dispatch()
		if got := len(e.push.received()); got != tc.want {
			t.Errorf("%s after the slot: %d push attempts, want %d", tc.after, got, tc.want)
		}
	}
}

// breakSubscriptionReads makes reading a user's devices fail the way any database
// read can. The store API has no way to say "fail this query", so this reaches
// past it through Store.DB, which exists for exactly that (see failFanOutOf).
func (e *env) breakSubscriptionReads() {
	e.t.Helper()
	if _, err := e.st.DB().ExecContext(e.ctx,
		`ALTER TABLE push_subscriptions RENAME TO push_subscriptions_unavailable`); err != nil {
		e.t.Fatalf("install the failure: %v", err)
	}
}

func (e *env) subscriptionReadsWorkAgain() {
	e.t.Helper()
	if _, err := e.st.DB().ExecContext(e.ctx,
		`ALTER TABLE push_subscriptions_unavailable RENAME TO push_subscriptions`); err != nil {
		e.t.Fatalf("clear the failure: %v", err)
	}
}

// TestDigestIsResolvedAtDelivery: the morning digest describes the day as it
// stands when it goes out, not as it stood up to 48 hours earlier when its row
// was materialized. The outbox is INSERT OR IGNORE on (user, kind, source_ref,
// due_at), so no later pass can rewrite a payload once it is written — a digest
// carrying its agenda would be frozen against every edit made in between, which
// on a family calendar is most days.
func TestDigestIsResolvedAtDelivery(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 3, 0, 0, 0, time.UTC)) // 05:00 Paris
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestEnabled: true, DigestTime: "07:30", SummaryTime: "20:00",
		EmailReminders: true, EmailDigest: true,
	})

	// Tomorrow already has something on it, which is what makes this the case that
	// bites: a day that was empty heals itself, because there was no row to freeze.
	tomorrow := date(2027, 6, 2)
	e.timedEvent(cal, u.ID, "Piscine", 2027, 6, 2, 10, 0, time.Hour, nil)
	dentiste := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 2, 14, 0, time.Hour, nil)
	judo := e.timedEvent(cal, u.ID, "Judo", 2027, 6, 2, 17, 0, time.Hour, nil)
	e.plan()
	if _, ok := findRow(e.queueOfKind(domain.KindDigest), events.DigestSourceRef(tomorrow)); !ok {
		t.Fatal("no digest was queued for tomorrow, so this proves nothing about a frozen one")
	}

	// The day then changes, as a day does: two things added, one cancelled, one
	// renamed.
	e.timedEvent(cal, u.ID, "Cirque", 2027, 6, 2, 9, 0, 2*time.Hour, nil)
	e.timedEvent(cal, u.ID, "Goûter chez Camille", 2027, 6, 2, 16, 0, time.Hour, nil)
	if err := e.ev.Delete(e.ctx, u.ID, dentiste.ID, "", domain.Date{}); err != nil {
		t.Fatalf("cancel the appointment: %v", err)
	}
	if _, err := e.ev.Update(e.ctx, u.ID, judo.ID, "", domain.Date{}, events.Input{
		CalendarID: cal.ID, Title: "Judo à Vincennes", StartsAt: judo.StartsAt, EndsAt: judo.EndsAt,
		LabelID: judo.LabelID, Participants: []int64{u.ID},
	}); err != nil {
		t.Fatalf("rename the lesson: %v", err)
	}
	e.plan() // and a later pass cannot rewrite the row an earlier one wrote

	e.mail.reset()
	e.push.reset()
	e.clk.Set(time.Date(2027, 6, 2, 5, 30, 0, 0, time.UTC)) // 07:30 Paris, the slot
	e.dispatch()

	row, ok := findRow(e.queueOfKind(domain.KindDigest), events.DigestSourceRef(tomorrow))
	if !ok || row.SentAt.IsZero() {
		t.Fatalf("the digest for %s was not delivered: %+v", tomorrow, row)
	}
	if got := len(e.push.received()); got != 1 {
		t.Fatalf("digest pushes = %d, want 1", got)
	}
	msgs := e.mail.messages()
	if len(msgs) != 1 {
		t.Fatalf("digest emails = %d, want 1", len(msgs))
	}
	text := msgs[0].Text
	for _, want := range []string{"4 événements", "Cirque", "Goûter chez Camille", "Judo à Vincennes"} {
		if !strings.Contains(text, want) {
			t.Errorf("the delivered digest does not carry %q; it describes the day as it was when the row was planned:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Dentiste") {
		t.Errorf("the delivered digest still announces an appointment cancelled the day before:\n%s", text)
	}
}

// TestSummaryIsResolvedAtDelivery: a batched activity summary cannot carry its
// content at planning time, because the changes it reports have not happened when
// the row is materialized two days ahead.
func TestSummaryIsResolvedAtDelivery(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 3, 0, 0, 0, time.UTC))
	watcher := e.user("alice")
	actor := e.user("bruno")
	cal := e.calendar("Famille", actor.ID)
	e.join(cal.ID, watcher.ID)
	e.subscribe(watcher.ID, "iphone")
	e.setPrefs(domain.NotificationPrefs{
		UserID: watcher.ID, DigestTime: "07:30", DailySummaryMode: true,
		SummaryTime: "20:00", EmailReminders: true, ActivityPush: true,
	})
	e.plan()

	rows := e.queueOfKind(domain.KindSummary)
	if len(rows) == 0 {
		t.Fatal("no summary was planned")
	}
	if p := e.payloadOf(rows[0]); p.Total != 0 {
		t.Errorf("summary was planned with a count of %d; its content does not exist yet", p.Total)
	}

	// Two changes happen during the day.
	e.timedEvent(cal, actor.ID, "Courses", 2027, 6, 4, 10, 0, time.Hour, nil)
	e.timedEvent(cal, actor.ID, "Piscine", 2027, 6, 4, 15, 0, time.Hour, nil)

	slot := time.Date(2027, 6, 1, 18, 0, 0, 0, time.UTC) // 20:00 Paris
	e.clk.Set(slot)
	e.dispatch()

	summary, ok := findRow(e.queueOfKind(domain.KindSummary), events.SummarySourceRef(date(2027, 6, 1)))
	if !ok {
		t.Fatal("today's summary row disappeared")
	}
	if !summary.DueAt.Equal(slot) {
		t.Fatalf("summary due at %s, want %s", summary.DueAt, slot)
	}
	if summary.SentAt.IsZero() {
		t.Fatalf("the summary was not delivered: %+v", summary)
	}
	if got := len(e.push.received()); got != 1 {
		t.Fatalf("summary pushes = %d, want 1", got)
	}

	// The wording is composed from what actually happened, counted at delivery.
	filled, err := e.n.fillSummary(e.ctx, watcher.ID, e.payloadOf(summary), "20:00")
	if err != nil {
		t.Fatalf("fill summary: %v", err)
	}
	if filled.Total != 2 {
		t.Fatalf("summary counted %d changes, want 2", filled.Total)
	}
	body, _, err := e.n.render(filled, e.recipientOf(t, watcher.ID), summary.SourceRef, e.clk.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if pp := decodePush(t, body); pp.Body != "2 changements aujourd'hui" {
		t.Errorf("summary body = %q, want %q", pp.Body, "2 changements aujourd'hui")
	}
}

// TestSummaryWithNothingToReportIsSkipped: "0 changements aujourd'hui" is not
// worth a notification.
func TestSummaryWithNothingToReportIsSkipped(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 3, 0, 0, 0, time.UTC))
	u := e.user("alice")
	e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestTime: "07:30", DailySummaryMode: true,
		SummaryTime: "20:00", EmailReminders: true, ActivityPush: true,
	})
	e.plan()
	e.clk.Set(time.Date(2027, 6, 1, 18, 0, 0, 0, time.UTC))
	e.dispatch()

	for _, r := range e.queueOfKind(domain.KindSummary) {
		if r.DueAt.Equal(time.Date(2027, 6, 1, 18, 0, 0, 0, time.UTC)) {
			if r.Skipped == "" {
				t.Errorf("an empty summary was pushed: %+v", r)
			}
			if len(e.push.received()) != 0 {
				t.Errorf("push deliveries = %d, want 0", len(e.push.received()))
			}
			return
		}
	}
	t.Fatal("today's summary row was never planned")
}

// TestNoChannelsStillRetiresTheRow: a person with no device and no email
// preference would otherwise leave immortal rows in the queue.
func TestNoChannelsStillRetiresTheRow(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestTime: "07:30", SummaryTime: "20:00", EmailReminders: false,
	})
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()
	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	e.dispatch()

	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 || rows[0].SentAt.IsZero() {
		t.Fatalf("reminder row = %+v, want it retired rather than retried forever", rows)
	}
}

// TestClockSanity: a server that boots in 1970 after a dead CMOS battery must
// refuse to run rather than flush or bury the whole queue.
func TestClockSanity(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))

	if _, err := New(Options{
		Store: e.st, Events: e.ev, Catalog: i18n.MustLoad(),
		Clock: clockAt(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)), Location: paris,
	}); err == nil {
		t.Fatal("New accepted a 1970 clock")
	}

	// And a clock that goes bad after startup stops the scheduler doing damage.
	e.clk.Set(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := e.n.Tick(e.ctx); err == nil {
		t.Fatal("Tick ran with an implausible clock")
	}
	if err := e.n.Plan(e.ctx); err == nil {
		t.Fatal("Plan ran with an implausible clock")
	}
	if err := e.n.Dispatch(e.ctx); err == nil {
		t.Fatal("Dispatch ran with an implausible clock")
	}
	if h := e.n.Health(e.ctx); h.ClockOK {
		t.Error("Health reported the clock as fine")
	}
	if err := e.n.Run(e.ctx, nil); err == nil {
		t.Fatal("Run started with an implausible clock")
	}
}

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c).UTC() }

func clockAt(t time.Time) fixedClock { return fixedClock(t) }

// TestTickAndHeartbeat: the loop's two observable outputs.
func TestTickAndHeartbeat(t *testing.T) {
	now := time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)

	if !e.n.Heartbeat().IsZero() {
		t.Error("heartbeat is set before the first tick")
	}
	if err := e.n.Tick(e.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := e.n.Heartbeat(); !got.Equal(now) {
		t.Errorf("heartbeat = %s, want %s", got, now)
	}
	h := e.n.Health(e.ctx)
	if h.QueueDepth == 0 {
		t.Error("Health.QueueDepth = 0 after planning a reminder")
	}
	if h.LastError != "" {
		t.Errorf("Health.LastError = %q, want empty", h.LastError)
	}
	if !h.ClockOK {
		t.Error("Health.ClockOK = false on a good clock")
	}
}

// TestRunStopsOnCancellation: the scheduler goroutine must exit cleanly, and it
// must ping the watchdog while it lives.
func TestRunStopsOnCancellation(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.n.tick = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	pinged := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- e.n.Run(ctx, func() {
			select {
			case pinged <- struct{}{}:
			default:
			}
		})
	}()

	select {
	case <-pinged:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the watchdog was never pinged")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}

	// Run performs the boot catch-up when the caller has not.
	if e.n.Health(context.Background()).CatchUp.At.IsZero() {
		t.Error("Run did not run the boot catch-up")
	}
}
