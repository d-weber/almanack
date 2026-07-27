package notify

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"almanack/internal/domain"
	"almanack/internal/events"
)

// Regressions found by an adversarial review of the built system. Each of these
// failed before the fix in the same commit, and each describes a way the family
// would have been told the wrong thing — or nothing at all.

// A reminder whose slot has already passed used to be dropped by the planner,
// which meant an appointment added shortly before it starts produced no
// notification on that tick or on any later one. The planner now queues it and
// lets delivery decide, which is where the "late warning beats no warning" rule
// already lived.
func TestReminderForAnImminentEventIsStillPlanned(t *testing.T) {
	now := time.Date(2027, 6, 7, 6, 0, 0, 0, time.UTC) // 08:00 in Paris
	e := newEnv(t, now)
	ctx := context.Background()

	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	// An appointment twenty minutes away, with a reminder that should have gone
	// out ten minutes ago.
	ev := e.timedEvent(cal, alice.ID, "Dentiste", 2027, time.June, 7, 8, 20, time.Hour, nil)
	e.reminderMinutes(ev, alice.ID, 30)

	if err := e.n.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := len(e.push.received()); got != 1 {
		t.Fatalf("an appointment 20 minutes away produced %d notifications, want 1"+
			" (the reminder slot was already past, which is exactly when it matters most)", got)
	}
}

// The same slot, but for an event that has already started, must NOT be
// resurrected — the planner's new leniency is bounded by the event itself.
func TestReminderForAnEventAlreadyStartedIsNotPlanned(t *testing.T) {
	now := time.Date(2027, 6, 7, 8, 0, 0, 0, time.UTC) // 10:00 Paris
	e := newEnv(t, now)
	ctx := context.Background()

	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	ev := e.timedEvent(cal, alice.ID, "Déjà commencé", 2027, time.June, 7, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, alice.ID, 30)

	if err := e.n.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := len(e.push.received()); got != 0 {
		t.Errorf("an event that started an hour ago produced %d notifications, want 0", got)
	}
}

// Replacing a reminder used to leave the old one queued, so changing "60 minutes
// before" to "30 minutes before" produced two notifications, and deleting a
// reminder still fired it.
func TestReplacingAReminderRemovesTheQueuedOne(t *testing.T) {
	now := time.Date(2027, 6, 7, 6, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	ctx := context.Background()

	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	ev := e.timedEvent(cal, alice.ID, "Dentiste", 2027, time.June, 7, 17, 0, time.Hour, nil)
	e.reminderMinutes(ev, alice.ID, 60)
	if err := e.n.Plan(ctx); err != nil {
		t.Fatalf("plan: %v", err)
	}

	// The family changes its mind: half an hour is enough.
	e.setReminders(ev, alice.ID, []domain.Reminder{
		{EventID: &ev.ID, UserID: alice.ID, OffsetMinutes: ptrInt(30)},
	})
	if err := e.n.Plan(ctx); err != nil {
		t.Fatalf("re-plan: %v", err)
	}

	var pending int
	for _, row := range e.queue() {
		if row.Kind == domain.KindReminder && row.SentAt.IsZero() && row.Skipped == "" {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("%d reminders queued for one appointment after changing 60 minutes to 30, want 1", pending)
	}
}

// Switching the daily digest off left the next two days' digests in the outbox,
// so it arrived anyway; moving its time queued a second one.
func TestChangingDigestPreferencesClearsTheQueuedOnes(t *testing.T) {
	now := time.Date(2027, 6, 7, 6, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	ctx := context.Background()

	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.timedEvent(cal, alice.ID, "Piscine", 2027, time.June, 8, 17, 30, time.Hour, nil)

	e.setPrefs(domain.NotificationPrefs{
		UserID: alice.ID, DigestEnabled: true, DigestTime: "07:30", SummaryTime: "20:00",
	})
	if err := e.n.Plan(ctx); err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Off it goes.
	e.setPrefs(domain.NotificationPrefs{
		UserID: alice.ID, DigestEnabled: false, DigestTime: "07:30", SummaryTime: "20:00",
	})
	if err := e.n.Plan(ctx); err != nil {
		t.Fatalf("re-plan: %v", err)
	}

	for _, row := range e.queue() {
		if row.Kind == domain.KindDigest && row.SentAt.IsZero() && row.Skipped == "" {
			t.Errorf("a digest is still queued for %s after the digest was switched off", row.DueAt)
		}
	}
}

// activity_log.id is INTEGER PRIMARY KEY without AUTOINCREMENT, so SQLite hands the
// ids of deleted rows out again. Deleting the calendar that held the newest changes
// therefore leaves the planner's stored cursor above every id the log will produce
// next, and everything logged afterwards arrives *behind* it — announced to nobody
// until the log climbs back to the old high-water mark.
func TestActivityCursorSurvivesAReusedID(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	ctx := context.Background()
	e.noDigests()

	actor := e.user("alice")
	watcher := e.user("bruno")
	family := e.calendar("Famille", actor.ID)
	trip := e.calendar("Vacances", actor.ID)
	e.join(family.ID, watcher.ID)
	e.join(trip.ID, watcher.ID)

	e.plan() // the first pass only takes the high-water mark

	// One change in the calendar that survives, then three in the one that is about
	// to go: those take the top of the log and the cursor follows them up.
	e.timedEvent(family, actor.ID, "Dentiste", 2027, time.June, 2, 16, 30, time.Hour, nil)
	for i := 1; i <= 3; i++ {
		e.timedEvent(trip, actor.ID, fmt.Sprintf("Ferry %d", i), 2027, time.June, 3, 9, 0, time.Hour, nil)
	}
	e.plan()
	if n := len(e.queueOfKind(domain.KindActivity)); n != 4 {
		t.Fatalf("four changes produced %d activity notifications, want 4", n)
	}

	if err := e.st.DeleteCalendar(ctx, trip.ID); err != nil {
		t.Fatalf("delete the calendar holding the newest changes: %v", err)
	}

	// A change in the calendar that is still there, a minute later so that its
	// notification cannot be mistaken for one of the deleted calendar's.
	e.clk.Advance(time.Minute)
	e.timedEvent(family, actor.ID, "Piscine", 2027, time.June, 4, 17, 0, time.Hour, nil)

	newest, err := e.st.ListActivity(ctx, []int64{family.ID}, 1, 0)
	if err != nil || len(newest) == 0 {
		t.Fatalf("read the newest activity row: %v", err)
	}
	stored, err := e.st.GetMeta(ctx, MetaActivityCursor)
	if err != nil {
		t.Fatalf("read the activity cursor: %v", err)
	}
	cursor, err := strconv.ParseInt(stored, 10, 64)
	if err != nil {
		t.Fatalf("activity cursor %q: %v", stored, err)
	}
	if newest[0].ID > cursor {
		t.Fatalf("the new log row took id %d, above the stored cursor %d: this SQLite is not "+
			"reusing the ids of deleted rows, so this test no longer reproduces the fault",
			newest[0].ID, cursor)
	}

	e.plan()

	// The source reference alone would not do: it is built from the activity id, and
	// the deleted calendar's notifications are still in the outbox under that very id.
	// The payload is what says which change a row announces.
	byTitle := map[string]int{}
	for _, row := range e.queueOfKind(domain.KindActivity) {
		byTitle[e.payloadOf(row).Title]++
	}
	if byTitle["Piscine"] != 1 {
		t.Errorf("the change made after a calendar was deleted produced %d notifications, want 1: its "+
			"log row took the reused id %d, below the cursor stranded at %d by the deletion",
			byTitle["Piscine"], newest[0].ID, cursor)
	}
	// And repairing the cursor must not re-announce what the family has already been
	// told: a dropped notification and a duplicated one are both failures here.
	if byTitle["Dentiste"] != 1 {
		t.Errorf("the change announced before the deletion is queued %d times, want 1", byTitle["Dentiste"])
	}
}

// editOneOccurrence is the sequence the app performs when somebody moves a single
// lesson: the occurrence is edited, which leaves a standalone copy of the event
// behind, and the caller's reminder list is then filed against the id the edit
// answered with — the copy's. Both tests below start from it.
func editOneOccurrence(t *testing.T, e *env, cal domain.Calendar, series domain.Event,
	userID int64, occDate domain.Date, newStart time.Time, reminders []domain.Reminder) domain.Event {
	t.Helper()
	copyEvent, err := e.ev.Update(e.ctx, userID, series.ID, domain.ScopeThis, occDate, events.Input{
		CalendarID: cal.ID, Title: series.Title,
		StartsAt: newStart.UTC(), EndsAt: newStart.Add(time.Hour).UTC(),
		LabelID: series.LabelID, Participants: []int64{userID},
	})
	if err != nil {
		t.Fatalf("move the occurrence of %s on %s: %v", series.Title, occDate, err)
	}
	if copyEvent.ID == series.ID {
		t.Fatalf("editing one occurrence answered with the series template itself")
	}
	e.setReminders(copyEvent, userID, reminders)
	return copyEvent
}

// An edited occurrence used to be reminded twice. The copy left behind by the edit
// carries the reminder list the editor was showing, and the planner went on firing
// the series' reminders for that date as well: two rows, two different reminder ids,
// two identical pushes for one swimming lesson.
func TestEditingOneOccurrenceDoesNotDoubleItsReminder(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 8, 6, 0, 0, 0, time.UTC))
	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	series := e.timedEvent(cal, alice.ID, "Piscine", 2027, time.June, 1, 17, 0, time.Hour, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	e.reminderMinutes(series, alice.ID, 30)

	moved := date(2027, 6, 8)
	editOneOccurrence(t, e, cal, series, alice.ID, moved,
		time.Date(2027, 6, 8, 18, 0, 0, 0, paris),
		[]domain.Reminder{{OffsetMinutes: ptrInt(30)}})

	e.plan()
	e.clk.Set(time.Date(2027, 6, 8, 17, 30, 0, 0, paris)) // half an hour before the moved lesson
	e.dispatch()

	if got := len(e.push.received()); got != 1 {
		t.Errorf("moving one lesson of a weekly series produced %d pushes for it, want 1", got)
	}
}

// The other half of the same question. The editor lists the reminders on the copy,
// so a member who removes the reminder from one occurrence is shown an empty list —
// and used to be reminded anyway, because the planner still fired the series'.
func TestRemovingTheReminderFromOneOccurrenceStopsIt(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 8, 6, 0, 0, 0, time.UTC))
	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	series := e.timedEvent(cal, alice.ID, "Piscine", 2027, time.June, 1, 17, 0, time.Hour, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	e.reminderMinutes(series, alice.ID, 30)

	moved := date(2027, 6, 8)
	editOneOccurrence(t, e, cal, series, alice.ID, moved,
		time.Date(2027, 6, 8, 18, 0, 0, 0, paris), nil)

	e.plan()
	e.clk.Set(time.Date(2027, 6, 8, 17, 30, 0, 0, paris))
	e.dispatch()

	if got := len(e.push.received()); got != 0 {
		t.Errorf("%d pushes for an occurrence whose reminder was removed, want 0", got)
	}

	// Next week's lesson, which nobody touched, still has the series' reminder.
	e.push.reset()
	e.clk.Set(time.Date(2027, 6, 15, 12, 0, 0, 0, paris))
	e.plan()
	e.clk.Set(time.Date(2027, 6, 15, 16, 30, 0, 0, paris))
	e.dispatch()
	if got := len(e.push.received()); got != 1 {
		t.Errorf("%d pushes for the untouched occurrence a week later, want 1: removing a reminder"+
			" from one lesson must not remove it from the series", got)
	}
}

func ptrInt(v int) *int { return &v }
