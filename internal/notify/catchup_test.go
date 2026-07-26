package notify

import (
	"strings"
	"testing"
	"time"

	"agenda/internal/domain"
	"agenda/internal/events"
)

// TestOutageCatchUp is the boot policy of the notification rules in docs/architecture.md, end to end, and it
// is as correctness-critical as the DST tests. A family server is a machine
// nobody watches: it will be off for a week at some point, and what it does when
// it comes back decides whether anyone notices.
//
// The scenario: seven days of downtime spanning several reminders and a digest a
// day. Coming back, the server must
//
//  1. backfill the materialization gap — the rows for those seven days were never
//     created, and no delivery logic can deliver a row that does not exist;
//  2. deliver the overdue reminders whose events are still ahead;
//  3. stale-skip, with a reason, the overdue reminders whose events have passed —
//     neither delivering them nor silently dropping them;
//  4. deliver only a digest whose slot passed less than four hours ago, and drop
//     the older ones rather than pushing last Wednesday's plan.
func TestOutageCatchUp(t *testing.T) {
	// Before: the server is up and plans its 48-hour window.
	before := time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC) // 08:00 Paris
	e := newEnv(t, before)
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	e.setPrefs(domain.NotificationPrefs{
		UserID: u.ID, DigestEnabled: true, DigestTime: "07:30", DigestOnEmpty: true,
		SummaryTime: "20:00", EmailReminders: true,
	})

	// Four appointments, each with a reminder, chosen so that every branch of the
	// policy is exercised on the way back up.
	//
	//   earlyPast   its slot AND its event fall inside the outage, and the row was
	//               already materialized before the server went down
	//   deepPast    both fall inside the outage, and the row was never materialized
	//               at all — this one only exists if the backfill works
	//   stillAhead  its slot fell inside the outage but the event is in the future
	//               when the server returns (a two-day reminder)
	//   future      entirely after the return; ordinary planning
	earlyPast := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 2, 10, 0, time.Hour, nil)
	deepPast := e.timedEvent(cal, u.ID, "Kiné", 2027, 6, 5, 10, 0, time.Hour, nil)
	stillAhead := e.timedEvent(cal, u.ID, "Vol Marseille", 2027, 6, 8, 20, 0, 2*time.Hour, nil)
	future := e.timedEvent(cal, u.ID, "Piscine", 2027, 6, 9, 18, 0, time.Hour, nil)

	e.reminderMinutes(earlyPast, u.ID, 30)
	e.reminderMinutes(deepPast, u.ID, 30)
	e.reminderMinutes(stillAhead, u.ID, 2*24*60) // two days ahead: slot on 6 June
	e.reminderMinutes(future, u.ID, 30)

	if _, err := e.n.CatchUp(e.ctx); err != nil {
		t.Fatalf("first boot catch-up: %v", err)
	}

	// What the pre-outage pass could see: only the first 48 hours.
	preOutage := map[string]bool{}
	for _, r := range e.queue() {
		preOutage[r.SourceRef] = true
	}
	if !preOutage[reminderRef(t, e, earlyPast, u.ID, date(2027, 6, 2))] {
		t.Fatal("the pre-outage pass did not materialize the reminder inside its horizon")
	}
	if preOutage[reminderRef(t, e, deepPast, u.ID, date(2027, 6, 5))] {
		t.Fatal("the pre-outage pass reached beyond its 48-hour horizon; the test proves nothing about backfilling")
	}

	// --- the server is off for a week ---
	back := time.Date(2027, 6, 8, 8, 0, 0, 0, time.UTC) // 10:00 Paris
	e.clk.Set(back)
	e.push.reset()
	e.mail.reset()

	sum, err := e.n.CatchUp(e.ctx)
	if err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	// 1. The gap was backfilled.
	if got, want := sum.Gap, back.Sub(before.Add(DefaultHorizon)); got != want {
		t.Errorf("summary gap = %s, want %s (from the last planned horizon to now)", got, want)
	}
	if sum.Backfilled == 0 {
		t.Error("summary reports nothing backfilled; the week-long hole was not filled")
	}
	if sum.Truncated {
		t.Error("a seven-day outage should not trip the backfill limit")
	}

	rows := byRef(e.queue())

	// 2. A reminder never materialized before the outage now exists — and, its
	//    event having passed, was skipped rather than delivered.
	deepRef := reminderRef(t, e, deepPast, u.ID, date(2027, 6, 5))
	deep, ok := rows[deepRef]
	if !ok {
		t.Fatalf("the backfill did not materialize %s: a black hole of never-created reminders is exactly what step 1 exists to prevent", deepRef)
	}
	assertSkipped(t, deep, "event already past")

	// 3. The reminder that was materialized before the outage and whose event has
	//    since passed is skipped too, with a reason — not delivered, not dropped.
	assertSkipped(t, rows[reminderRef(t, e, earlyPast, u.ID, date(2027, 6, 2))], "event already past")

	// 4. The reminder whose slot passed but whose event is still ahead goes out
	//    late: a late warning beats no warning.
	ahead := rows[reminderRef(t, e, stillAhead, u.ID, date(2027, 6, 8))]
	if ahead.SentAt.IsZero() {
		t.Errorf("the reminder for an event still eight hours away was not delivered: %+v", ahead)
	}
	if ahead.Skipped != "" {
		t.Errorf("the reminder for a future event was skipped: %q", ahead.Skipped)
	}

	// 5. The reminder for an event after the return is simply queued, untouched.
	futureRow := rows[reminderRef(t, e, future, u.ID, date(2027, 6, 9))]
	if !futureRow.SentAt.IsZero() || futureRow.Skipped != "" {
		t.Errorf("a reminder for a future event was acted on during catch-up: %+v", futureRow)
	}

	// 6. Digests: only today's survives, and only because its 07:30 slot was two
	//    and a half hours ago. Everything older is dropped.
	var sentDigests, skippedDigests []queueRow
	for _, r := range e.queueOfKind(domain.KindDigest) {
		switch {
		case !r.SentAt.IsZero():
			sentDigests = append(sentDigests, r)
		case r.Skipped != "":
			skippedDigests = append(skippedDigests, r)
		}
	}
	if len(sentDigests) != 1 {
		t.Fatalf("delivered %d digests, want exactly today's: %+v", len(sentDigests), sentDigests)
	}
	if got, want := sentDigests[0].SourceRef, events.DigestSourceRef(date(2027, 6, 8)); got != want {
		t.Errorf("delivered digest %s, want %s", got, want)
	}
	if len(skippedDigests) < 5 {
		t.Errorf("only %d stale digests were dropped; the whole week should have been", len(skippedDigests))
	}
	for _, r := range skippedDigests {
		if !strings.Contains(r.Skipped, "slot passed") {
			t.Errorf("digest %s skipped with reason %q, want it to say the slot passed", r.SourceRef, r.Skipped)
		}
	}

	// 7. The summary the operator logs adds up.
	if sum.Delivered < 2 {
		t.Errorf("summary delivered = %d, want at least the late reminder and today's digest", sum.Delivered)
	}
	if sum.Skipped < 6 {
		t.Errorf("summary skipped = %d, want the stale reminders and the week of digests", sum.Skipped)
	}
	if !strings.Contains(sum.String(), "catch-up: gap") {
		t.Errorf("summary line = %q, want something the ops heartbeat can carry", sum.String())
	}
	if got := e.n.Health(e.ctx).CatchUp; got.At.IsZero() {
		t.Error("Health does not carry the catch-up summary")
	}

	// 8. Nothing was delivered twice: skipped rows stay skipped through the next
	//    ordinary pass, because the planner's INSERT OR IGNORE cannot resurrect them.
	sentBefore := countSent(e.queue())
	if err := e.n.Tick(e.ctx); err != nil {
		t.Fatalf("tick after catch-up: %v", err)
	}
	if got := countSent(e.queue()); got != sentBefore {
		t.Errorf("a tick right after catch-up delivered %d more rows, want 0", got-sentBefore)
	}
	assertSkipped(t, byRef(e.queue())[deepRef], "event already past")
}

// TestCatchUpOnAFreshDatabase: no horizon marker means no gap, not a walk back to
// the epoch.
func TestCatchUpOnAFreshDatabase(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	sum, err := e.n.CatchUp(e.ctx)
	if err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	if sum.Gap != 0 {
		t.Errorf("gap on a fresh database = %s, want 0", sum.Gap)
	}
	if sum.Truncated {
		t.Error("a fresh database must not trip the backfill limit")
	}
}

// TestCatchUpTruncatesAnAbsurdGap: a server that has been off for a year must not
// expand a year of recurrences to prove that every one of those reminders is
// stale. The limit is reported, not hidden.
func TestCatchUpTruncatesAnAbsurdGap(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	u := e.user("alice")
	e.calendar("Famille", u.ID)
	e.plan()

	e.clk.Set(time.Date(2028, 6, 1, 6, 0, 0, 0, time.UTC))
	sum, err := e.n.CatchUp(e.ctx)
	if err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	if !sum.Truncated {
		t.Error("a year-long gap did not trip the backfill limit")
	}
	if sum.Gap < 300*24*time.Hour {
		t.Errorf("gap = %s, want it reported honestly even when the backfill is truncated", sum.Gap)
	}
}

// TestCatchUpIsIdempotent: running it twice must not deliver anything twice.
func TestCatchUpIsIdempotent(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 1, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()

	e.clk.Set(time.Date(2027, 6, 1, 6, 30, 0, 0, time.UTC))
	if _, err := e.n.CatchUp(e.ctx); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	first := len(e.push.received())
	if first == 0 {
		t.Fatal("catch-up delivered nothing")
	}
	if _, err := e.n.CatchUp(e.ctx); err != nil {
		t.Fatalf("second catch-up: %v", err)
	}
	if got := len(e.push.received()); got != first {
		t.Errorf("a second catch-up sent %d more pushes, want 0", got-first)
	}
}

// TestStalenessPolicy is the decision table of the notification rules in docs/architecture.md as a
// unit test, including the rule that keeps normal operation working: a row that
// is barely late is always delivered, or every at-the-start reminder would be
// skipped the moment a tick landed a second after the event began.
func TestStalenessPolicy(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 8, 8, 0, 0, 0, time.UTC))
	now := e.clk.Now()

	tests := []struct {
		name      string
		kind      domain.NotificationKind
		due       time.Time
		p         payload
		wantStale bool
	}{
		{
			name: "an on-time reminder for an event starting right now",
			kind: domain.KindReminder, due: now.Add(-20 * time.Second),
			p:         payload{Kind: domain.KindReminder, EventStart: now},
			wantStale: false,
		},
		{
			name: "a late reminder whose event is still ahead",
			kind: domain.KindReminder, due: now.Add(-48 * time.Hour),
			p:         payload{Kind: domain.KindReminder, EventStart: now.Add(6 * time.Hour)},
			wantStale: false,
		},
		{
			name: "a late reminder whose event has passed",
			kind: domain.KindReminder, due: now.Add(-48 * time.Hour),
			p:         payload{Kind: domain.KindReminder, EventStart: now.Add(-24 * time.Hour)},
			wantStale: true,
		},
		{
			name: "an all-day reminder is still worth sending on the day itself",
			kind: domain.KindReminder, due: now.Add(-6 * time.Hour),
			p:         payload{Kind: domain.KindReminder, AllDay: true, EventDate: date(2027, 6, 8)},
			wantStale: false,
		},
		{
			name: "an all-day reminder for yesterday is not",
			kind: domain.KindReminder, due: now.Add(-30 * time.Hour),
			p:         payload{Kind: domain.KindReminder, AllDay: true, EventDate: date(2027, 6, 7)},
			wantStale: true,
		},
		{
			name: "a digest whose slot passed under four hours ago",
			kind: domain.KindDigest, due: now.Add(-3 * time.Hour),
			p:         payload{Kind: domain.KindDigest, Day: date(2027, 6, 8)},
			wantStale: false,
		},
		{
			name: "a digest whose slot passed over four hours ago",
			kind: domain.KindDigest, due: now.Add(-5 * time.Hour),
			p:         payload{Kind: domain.KindDigest, Day: date(2027, 6, 8)},
			wantStale: true,
		},
		{
			name: "an activity notification from this morning",
			kind: domain.KindActivity, due: now.Add(-6 * time.Hour),
			p:         payload{Kind: domain.KindActivity},
			wantStale: false,
		},
		{
			name: "an activity notification from last week",
			kind: domain.KindActivity, due: now.Add(-7 * 24 * time.Hour),
			p:         payload{Kind: domain.KindActivity},
			wantStale: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := domain.QueuedNotification{Kind: tc.kind, DueAt: tc.due}
			reason, stale := e.n.staleness(q, tc.p, now)
			if stale != tc.wantStale {
				t.Errorf("stale = %v (%q), want %v", stale, reason, tc.wantStale)
			}
			if stale && reason == "" {
				t.Error("a skipped row must record why")
			}
		})
	}
}

// ---------------------------------------------------------------------------

func byRef(rows []queueRow) map[string]queueRow {
	out := make(map[string]queueRow, len(rows))
	for _, r := range rows {
		out[r.SourceRef] = r
	}
	return out
}

func countSent(rows []queueRow) int {
	n := 0
	for _, r := range rows {
		if !r.SentAt.IsZero() {
			n++
		}
	}
	return n
}

// reminderRef rebuilds the source reference the planner would have written for
// one user's single reminder on an event, so assertions name rows the same way
// the code does.
func reminderRef(t *testing.T, e *env, ev domain.Event, userID int64, occ domain.Date) string {
	t.Helper()
	all, err := e.st.ListAllReminders(e.ctx)
	if err != nil {
		t.Fatalf("list reminders: %v", err)
	}
	for _, r := range all {
		if r.UserID != userID {
			continue
		}
		if (r.EventID != nil && *r.EventID == ev.ID) ||
			(r.RecurrenceID != nil && ev.RecurrenceID != nil && *r.RecurrenceID == *ev.RecurrenceID) {
			return events.ReminderSourceRef(ev.ID, occ, r.ID)
		}
	}
	t.Fatalf("no reminder for user %d on event %d", userID, ev.ID)
	return ""
}

func assertSkipped(t *testing.T, r queueRow, wantReason string) {
	t.Helper()
	if r.SourceRef == "" {
		t.Fatal("expected a queued row, found none")
	}
	if !r.SentAt.IsZero() {
		t.Errorf("%s was delivered, want it stale-skipped", r.SourceRef)
	}
	if r.Skipped == "" {
		t.Errorf("%s was neither delivered nor skipped: a silently dropped row leaves no evidence it was a decision", r.SourceRef)
		return
	}
	if !strings.Contains(r.Skipped, wantReason) {
		t.Errorf("%s skipped with reason %q, want it to mention %q", r.SourceRef, r.Skipped, wantReason)
	}
}
