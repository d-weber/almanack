package notify

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
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
//
// The table is the whole of the fault, not one point in it. Reused ids climb back
// towards the stranded cursor one change at a time, so how much is lost depends on
// two numbers: how many rows the deleted calendar held above the surviving maximum
// (deleted), and how many changes are made before the next planning pass notices
// (made). The first min(deleted, made) of those changes land at or below the cursor.
// deleted=1 — the deleted calendar held the single newest entry — is the ordinary
// case, and "delete the holiday calendar, then add an event to the family one" is
// exactly deleted=1, made=1.
func TestActivityCursorSurvivesAReusedID(t *testing.T) {
	for _, tc := range []struct{ deleted, made int }{
		{deleted: 1, made: 1}, // the ordinary case: one calendar gone, one change after it
		{deleted: 2, made: 2},
		{deleted: 3, made: 3},
		{deleted: 1, made: 3},
		{deleted: 2, made: 5},
		{deleted: 3, made: 2},
	} {
		t.Run(fmt.Sprintf("deleted=%d,made=%d", tc.deleted, tc.made), func(t *testing.T) {
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

			// One change in the calendar that survives, then the deleted calendar's:
			// those take the top of the log and the cursor follows them up.
			e.timedEvent(family, actor.ID, "Dentiste", 2027, time.June, 2, 16, 30, time.Hour, nil)
			e.clk.Advance(time.Second)
			for i := 1; i <= tc.deleted; i++ {
				e.timedEvent(trip, actor.ID, fmt.Sprintf("Ferry %d", i), 2027, time.June, 3, 9, 0, time.Hour, nil)
				e.clk.Advance(time.Second)
			}
			e.plan()
			if n, want := len(e.queueOfKind(domain.KindActivity)), tc.deleted+1; n != want {
				t.Fatalf("%d changes produced %d activity notifications, want %d", want, n, want)
			}

			if err := e.st.DeleteCalendar(ctx, trip.ID); err != nil {
				t.Fatalf("delete the calendar holding the newest changes: %v", err)
			}
			stranded := e.activityCursor()

			// Changes in the calendar that is still there, a minute later so that their
			// notifications cannot be mistaken for the deleted calendar's, and a second
			// apart so that each is its own row in the outbox.
			e.clk.Advance(time.Minute)
			var reused []int64
			for i := 1; i <= tc.made; i++ {
				e.timedEvent(family, actor.ID, fmt.Sprintf("Piscine %d", i), 2027, time.June, 4, 17, 0, time.Hour, nil)
				e.clk.Advance(time.Second)
				newest, err := e.st.ListActivity(ctx, []int64{family.ID}, 1, 0)
				if err != nil || len(newest) == 0 {
					t.Fatalf("read the newest activity row: %v", err)
				}
				reused = append(reused, newest[0].ID)
			}
			if reused[0] > stranded {
				t.Fatalf("the first new log row took id %d, above the stored cursor %d: this SQLite is not "+
					"reusing the ids of deleted rows, so this test no longer reproduces the fault",
					reused[0], stranded)
			}

			e.plan()

			// The source reference alone would not do: it is built from the activity id, and
			// the deleted calendar's notifications are still in the outbox under that very id.
			// The payload is what says which change a row announces.
			byTitle := map[string]int{}
			for _, row := range e.queueOfKind(domain.KindActivity) {
				byTitle[e.payloadOf(row).Title]++
			}
			for i := 1; i <= tc.made; i++ {
				title := fmt.Sprintf("Piscine %d", i)
				if byTitle[title] != 1 {
					t.Errorf("%q produced %d notifications, want 1: its log row took the reused id %d, "+
						"at or below the cursor stranded at %d by the deletion",
						title, byTitle[title], reused[i-1], stranded)
				}
			}
			// And repairing the cursor must not re-announce what the family has already been
			// told: a dropped notification and a duplicated one are both failures here.
			if byTitle["Dentiste"] != 1 {
				t.Errorf("the change announced before the deletion is queued %d times, want 1", byTitle["Dentiste"])
			}
		})
	}
}

// moveOneOccurrence moves a single lesson and touches nobody's reminders, which is
// what the app does when the reminder section of the editor was not changed: the
// occurrence is edited, leaving a standalone copy of the event behind, and no reminder
// list is saved against it.
func moveOneOccurrence(t *testing.T, e *env, cal domain.Calendar, series domain.Event,
	userID int64, occDate domain.Date, newStart time.Time) domain.Event {
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
	return copyEvent
}

// editOneOccurrence is the sequence the app performs when somebody moves a single
// lesson *and* changes its reminders: the reminder list is filed against the id the
// edit answered with — the copy's — which is what says "these, for this one occasion".
func editOneOccurrence(t *testing.T, e *env, cal domain.Calendar, series domain.Event,
	userID int64, occDate domain.Date, newStart time.Time, reminders []domain.Reminder) domain.Event {
	t.Helper()
	copyEvent := moveOneOccurrence(t, e, cal, series, userID, occDate, newStart)
	e.setReminders(copyEvent, userID, reminders)
	return copyEvent
}

// piscine is the series the reminder tests are about: swimming at 17:00 every Tuesday
// from 1 June 2027.
func piscine(e *env, cal domain.Calendar, userID int64) domain.Event {
	return e.timedEvent(cal, userID, "Piscine", 2027, time.June, 1, 17, 0, time.Hour, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
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

// An occurrence somebody had edited used to stop hearing about anything the series
// learned afterwards. The copy left behind by the edit took a copy of the reminders as
// they stood at that moment, and the planner read nothing else for that date — so a
// reminder added to the series later reached every lesson except the one that had been
// moved, permanently, with nothing on screen to say so. This is the first of the two
// ways a family runs into that, and the reason an occurrence inherits its series'
// reminders until somebody changes them on the occurrence itself.
func TestASeriesReminderAddedAfterAnEditReachesTheEditedOccurrence(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 8, 6, 0, 0, 0, time.UTC))
	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	series := piscine(e, cal, alice.ID)

	// Tuesday's lesson is moved to the evening. Nobody has any reminders yet, so
	// there is no reminder list to save with the edit.
	moved := date(2027, 6, 8)
	moveOneOccurrence(t, e, cal, series, alice.ID, moved, time.Date(2027, 6, 8, 18, 0, 0, 0, paris))

	// Only afterwards does she ask to be reminded about swimming.
	e.reminderMinutes(series, alice.ID, 30)

	e.plan()
	if got := rowsForOccurrence(e, series.ID, moved); len(got) != 1 {
		t.Fatalf("%d reminders queued for the moved lesson, want 1: a reminder added to the series"+
			" after an occurrence was edited must still reach that occurrence", len(got))
	} else if w := wall(got[0].DueAt); w != "2027-06-08 17:30 CEST" {
		t.Errorf("the moved lesson's reminder is at %s, want 2027-06-08 17:30 CEST"+
			" (half an hour before where it was moved to)", w)
	}

	e.clk.Set(time.Date(2027, 6, 8, 17, 30, 0, 0, paris))
	e.dispatch()
	if got := len(e.push.received()); got != 1 {
		t.Errorf("%d pushes for the moved lesson, want 1", got)
	}
}

// The second way, and the one nobody would ever suspect: the member is not the person
// who made the edit and was not even here when it happened. Everything they do is
// ordinary — join the calendar, ask to be reminded about swimming — and one lesson,
// the one somebody moved before they arrived, silently never reminds them.
func TestAMemberWhoJoinsAfterAnEditIsRemindedAboutTheEditedOccurrence(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 8, 6, 0, 0, 0, time.UTC))
	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	series := piscine(e, cal, alice.ID)

	moved := date(2027, 6, 8)
	moveOneOccurrence(t, e, cal, series, alice.ID, moved, time.Date(2027, 6, 8, 18, 0, 0, 0, paris))

	// Bob arrives after the lesson was moved and sets his first reminder.
	bob := e.user("bob")
	e.join(cal.ID, bob.ID)
	e.subscribe(bob.ID, "pixel")
	e.noDigests()
	e.reminderMinutes(series, bob.ID, 30)

	e.plan()
	rows := rowsForOccurrence(e, series.ID, moved)
	if len(rows) != 1 || rows[0].UserID != bob.ID {
		t.Fatalf("reminders queued for the moved lesson = %+v, want one for Bob (%d):"+
			" a member who joins after an occurrence was edited must be reminded about it too",
			rows, bob.ID)
	}
	if w := wall(rows[0].DueAt); w != "2027-06-08 17:30 CEST" {
		t.Errorf("Bob's reminder for the moved lesson is at %s, want 2027-06-08 17:30 CEST", w)
	}

	e.clk.Set(time.Date(2027, 6, 8, 17, 30, 0, 0, paris))
	e.dispatch()
	if got := len(e.push.received()); got != 1 {
		t.Errorf("%d pushes for the moved lesson, want 1 (Bob's)", got)
	}
}

// Setting a different reminder on one occurrence replaces the series' for that date
// rather than adding to it — the same rule as removing one, said with a non-empty list.
func TestAReminderSetOnOneOccurrenceReplacesTheSeriesOne(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 8, 6, 0, 0, 0, time.UTC))
	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	series := piscine(e, cal, alice.ID)
	e.reminderMinutes(series, alice.ID, 30)

	// This one is across town, so she wants two hours' warning for it alone.
	moved := date(2027, 6, 8)
	editOneOccurrence(t, e, cal, series, alice.ID, moved,
		time.Date(2027, 6, 8, 18, 0, 0, 0, paris),
		[]domain.Reminder{{OffsetMinutes: ptrInt(120)}})

	e.plan()
	rows := rowsForOccurrence(e, series.ID, moved)
	if len(rows) != 1 {
		t.Fatalf("%d reminders queued for the edited occurrence, want 1: the reminder set on it"+
			" replaces the series' for that date rather than joining it", len(rows))
	}
	if w := wall(rows[0].DueAt); w != "2027-06-08 16:00 CEST" {
		t.Errorf("the edited occurrence's reminder is at %s, want 2027-06-08 16:00 CEST"+
			" (two hours before, not the series' half hour)", w)
	}

	// And the series is untouched: next Tuesday is still half an hour before 17:00.
	e.clk.Set(time.Date(2027, 6, 15, 6, 0, 0, 0, time.UTC))
	e.plan()
	next := rowsForOccurrence(e, series.ID, date(2027, 6, 15))
	if len(next) != 1 {
		t.Fatalf("%d reminders queued for next week's lesson, want 1", len(next))
	}
	if w := wall(next[0].DueAt); w != "2027-06-15 16:30 CEST" {
		t.Errorf("next week's reminder is at %s, want 2027-06-15 16:30 CEST", w)
	}
}

// Inheritance is per series, which is the sort of claim a query gets wrong in a way no
// other test notices: with one series in the fixture, a lookup that answers "every
// series reminder in the database" is indistinguishable from one that answers "this
// occurrence's series". So there are two series here, deliberately on the same evening
// and with different offsets, and the edited occurrence of one must inherit only its
// own — a stray reminder from the other would show up as a second row, at 15:00.
func TestASeriesRemindersDoNotReachAnotherSeriesEditedOccurrence(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 8, 6, 0, 0, 0, time.UTC))
	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	swimming := piscine(e, cal, alice.ID)
	e.reminderMinutes(swimming, alice.ID, 30)

	judo := e.timedEvent(cal, alice.ID, "Judo", 2027, time.June, 1, 19, 0, time.Hour, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	e.reminderMinutes(judo, alice.ID, 180)

	moved := date(2027, 6, 8)
	moveOneOccurrence(t, e, cal, swimming, alice.ID, moved, time.Date(2027, 6, 8, 18, 0, 0, 0, paris))

	e.plan()
	rows := rowsForOccurrence(e, swimming.ID, moved)
	if len(rows) != 1 {
		t.Fatalf("%d reminders queued for the moved swimming lesson, want exactly 1:"+
			" it inherits its own series' reminders and no other series'", len(rows))
	}
	if w := wall(rows[0].DueAt); w != "2027-06-08 17:30 CEST" {
		t.Errorf("the moved lesson's reminder is at %s, want 2027-06-08 17:30 CEST"+
			" (swimming's half hour, not judo's three)", w)
	}

	// Judo, which nobody edited, is unaffected in both directions.
	judoRows := rowsForOccurrence(e, judo.ID, moved)
	if len(judoRows) != 1 {
		t.Fatalf("%d reminders queued for judo, want 1", len(judoRows))
	}
	if w := wall(judoRows[0].DueAt); w != "2027-06-08 16:00 CEST" {
		t.Errorf("judo's reminder is at %s, want 2027-06-08 16:00 CEST", w)
	}
}

func ptrInt(v int) *int { return &v }

// The same fault with the clock standing still, which is not a contrived case: dev
// mode runs on a clock that moves only when it is told to, so every entry in the log
// can share one instant. A cursor vouched for by its instant alone is blind exactly
// there — the reused id looks like the row it replaced — which is why the calendar is
// in the witness beside it.
func TestActivityCursorSurvivesAReusedIDOnAStoppedClock(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	ctx := context.Background()
	e.noDigests()

	actor := e.user("alice")
	watcher := e.user("bruno")
	family := e.calendar("Famille", actor.ID)
	// The calendar about to go is the actor's alone, so its change is announced to
	// nobody and the outbox holds nothing under the id that is about to be reused.
	trip := e.calendar("Vacances", actor.ID)
	e.join(family.ID, watcher.ID)

	e.plan() // the first pass only takes the high-water mark

	e.timedEvent(family, actor.ID, "Dentiste", 2027, time.June, 2, 16, 30, time.Hour, nil)
	e.timedEvent(trip, actor.ID, "Ferry", 2027, time.June, 3, 9, 0, time.Hour, nil)
	e.plan()

	if err := e.st.DeleteCalendar(ctx, trip.ID); err != nil {
		t.Fatalf("delete the calendar holding the newest change: %v", err)
	}
	stranded := e.activityCursor()
	e.timedEvent(family, actor.ID, "Piscine", 2027, time.June, 4, 17, 0, time.Hour, nil)

	newest, err := e.st.ListActivity(ctx, []int64{family.ID}, 1, 0)
	if err != nil || len(newest) == 0 {
		t.Fatalf("read the newest activity row: %v", err)
	}
	if newest[0].ID > stranded {
		t.Fatalf("the new log row took id %d, above the stored cursor %d: this SQLite is not "+
			"reusing the ids of deleted rows, so this test no longer reproduces the fault",
			newest[0].ID, stranded)
	}

	e.plan()

	byTitle := map[string]int{}
	for _, row := range e.queueOfKind(domain.KindActivity) {
		byTitle[e.payloadOf(row).Title]++
	}
	if byTitle["Piscine"] != 1 {
		t.Errorf("the change made after a calendar was deleted produced %d notifications, want 1: "+
			"its log row took the reused id %d with the same instant as the row it replaced",
			byTitle["Piscine"], newest[0].ID)
	}
	if byTitle["Dentiste"] != 1 {
		t.Errorf("the change announced before the deletion is queued %d times, want 1", byTitle["Dentiste"])
	}
}

// The half of a reused id the test above deliberately steps around, by keeping the
// doomed calendar to the actor alone: what happens when the outbox *does* hold a
// notification under the id that is about to be handed out again.
//
// The planner finds the new change — that is the fix above — and then the outbox threw
// it away. A queued activity notification was identified by (user, activity id,
// instant), so the announcement of a change that took a reused id in the same second as
// the row it replaced looked to INSERT OR IGNORE like the announcement already made,
// and was dropped. Nothing looked wrong afterwards: the feed listed the change, the
// notification about it had never existed, and the row absorbing it names an event in a
// calendar that no longer exists.
//
// The clock stands still here for the same reason as above — dev mode runs on a stopped
// one, where every entry shares an instant and reuse is therefore always a collision.
// In a household it needs the tidying-up and the change that follows it to fall inside
// the one second the last announced change was made in.
func TestAChangeTakingAReusedActivityIDIsStillAnnounced(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	ctx := context.Background()

	actor := e.user("alice")
	watcher := e.user("bruno")
	family := e.calendar("Famille", actor.ID)
	trip := e.calendar("Vacances", actor.ID)
	e.join(family.ID, watcher.ID)
	// This is the difference: the watcher is in the calendar that is about to be
	// deleted, so its change is announced to them and that notification is still in
	// the outbox under the id the next change will take.
	e.join(trip.ID, watcher.ID)
	e.noDigests()

	newestIn := func(calendarID int64) domain.Activity {
		t.Helper()
		rows, err := e.st.ListActivity(ctx, []int64{calendarID}, 1, 0)
		if err != nil || len(rows) == 0 {
			t.Fatalf("read the newest change in calendar %d: %v", calendarID, err)
		}
		return rows[0]
	}

	e.plan() // the first pass only takes the high-water mark

	e.timedEvent(trip, actor.ID, "Ferry", 2027, time.June, 3, 9, 0, time.Hour, nil)
	ferry := newestIn(trip.ID)
	e.plan()
	if n := len(e.queueOfKind(domain.KindActivity)); n != 1 {
		t.Fatalf("the change in the calendar about to be deleted produced %d notifications, want 1", n)
	}

	// The announcement goes out before the calendar does, which is what leaves it in
	// the outbox to be collided with. Deleting a calendar prunes the notifications it
	// has not delivered yet and keeps the ones it has, because a delivered row is the
	// record that the family was told — and a row that keeps its slot in the UNIQUE
	// index for good is what makes the reused id below a collision rather than a near
	// miss.
	for _, row := range e.queueOfKind(domain.KindActivity) {
		if err := e.st.MarkSent(ctx, row.ID, e.clk.Now()); err != nil {
			t.Fatalf("deliver the announcement made before the deletion: %v", err)
		}
	}
	if err := e.st.DeleteCalendar(ctx, trip.ID); err != nil {
		t.Fatalf("delete the calendar holding the newest change: %v", err)
	}
	e.timedEvent(family, actor.ID, "Piscine", 2027, time.June, 4, 17, 0, time.Hour, nil)

	piscine := newestIn(family.ID)
	if piscine.ID != ferry.ID {
		t.Fatalf("the replacement change took id %d and the row it replaced had %d: this SQLite is "+
			"not reusing the ids of deleted rows, so this test no longer reproduces the fault",
			piscine.ID, ferry.ID)
	}
	if !piscine.At.Equal(ferry.At) {
		t.Fatalf("the replacement change is at %s and the row it replaced at %s: the clock has moved, "+
			"and this test no longer reproduces the fault", piscine.At, ferry.At)
	}

	e.plan()

	byTitle := map[string]int{}
	for _, row := range e.queueOfKind(domain.KindActivity) {
		byTitle[e.payloadOf(row).Title]++
	}
	if byTitle["Piscine"] != 1 {
		t.Errorf("the change that took the reused id %d is queued %d times, want 1: it was made in the "+
			"same second as the row it replaced, whose notification is still in the outbox",
			piscine.ID, byTitle["Piscine"])
	}
	// The notification about the deleted calendar's change stays exactly as it is:
	// it was delivered once and it is not delivered again.
	if byTitle["Ferry"] != 1 {
		t.Errorf("the change announced before the deletion is queued %d times, want 1", byTitle["Ferry"])
	}
}

// The other side of naming the changes: what becomes of the notifications a database
// already holds, filed under the spelling that carried the id alone.
//
// They are left exactly where they are, and the rows they announce are deliberately not
// given names by the migration, so that the two go on agreeing. The alternative — a
// migration that rewrites either half — would have to decide what an old reference's id
// means today, and after a deletion it may already mean a different change; getting
// that wrong files a stale announcement against a change nobody has been told about
// yet, which is the fault being fixed here, reintroduced by the fix. So the old rows
// keep the old spelling for as long as they exist, and the repair pass that walks the
// last day of the log recognises its own work rather than sending it again.
func TestAChangeLoggedBeforeTheUpgradeIsNotAnnouncedTwice(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	ctx := context.Background()

	actor := e.user("alice")
	watcher := e.user("bruno")
	family := e.calendar("Famille", actor.ID)
	e.join(family.ID, watcher.ID)
	e.noDigests()

	e.plan() // the first pass only takes the high-water mark
	e.timedEvent(family, actor.ID, "Dentiste", 2027, time.June, 2, 16, 30, time.Hour, nil)

	// What the previous release left in the log: entries with no name of their own.
	// This reaches past the store API through Store.DB, which exists for exactly the
	// states no supported call can produce.
	if _, err := e.st.DB().ExecContext(ctx, `UPDATE activity_log SET change_uid = ''`); err != nil {
		t.Fatalf("take the names off the changes already logged: %v", err)
	}
	e.plan()

	rows := e.queueOfKind(domain.KindActivity)
	if len(rows) != 1 {
		t.Fatalf("the change produced %d notifications, want 1", len(rows))
	}
	if want := fmt.Sprintf("activity:%d", e.payloadOf(rows[0]).ActivityID); rows[0].SourceRef != want {
		t.Fatalf("it is queued as %q, want the old spelling %q: a change from before the upgrade "+
			"must keep the reference its notification is already filed under", rows[0].SourceRef, want)
	}

	// A database from that release has a cursor with nothing to vouch for it, so the
	// first pass after the upgrade walks the last day of the log again.
	for _, key := range []string{MetaActivityCursorAt, MetaActivityCursorCalendar} {
		if err := e.st.SetMeta(ctx, key, ""); err != nil {
			t.Fatalf("clear %s: %v", key, err)
		}
	}
	e.plan()

	if n := len(e.queueOfKind(domain.KindActivity)); n != 1 {
		t.Errorf("the re-walk left %d notifications about one change, want 1: the family is told "+
			"a second time about something it has already been told about", n)
	}
}

// A database written by the release before this one has a cursor but no instant beside
// it, so nothing can vouch for the number — least of all the possibility that it is
// already stranded, which is the state this bug leaves behind and the state an upgrade
// most needs to heal. The first pass after the upgrade therefore repairs without being
// asked: it walks the last day of the log again, the outbox absorbs everything already
// announced, and the pair is recorded so no later pass has to do it again.
func TestAnActivityCursorWithoutAnInstantIsRepairedOnce(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	ctx := context.Background()
	e.noDigests()

	actor := e.user("alice")
	watcher := e.user("bruno")
	family := e.calendar("Famille", actor.ID)
	e.join(family.ID, watcher.ID)

	e.plan() // the first pass only takes the high-water mark
	e.timedEvent(family, actor.ID, "Dentiste", 2027, time.June, 2, 16, 30, time.Hour, nil)
	e.plan()

	// What the older release left behind: a number, no witness, and — since this is
	// the fault it shipped with — a number standing above the end of the log.
	if err := e.st.SetMeta(ctx, MetaActivityCursor, strconv.FormatInt(e.activityCursor()+50, 10)); err != nil {
		t.Fatalf("strand the cursor: %v", err)
	}
	for _, key := range []string{MetaActivityCursorAt, MetaActivityCursorCalendar} {
		if err := e.st.SetMeta(ctx, key, ""); err != nil {
			t.Fatalf("clear %s: %v", key, err)
		}
	}

	e.clk.Advance(time.Minute)
	e.timedEvent(family, actor.ID, "Piscine", 2027, time.June, 4, 17, 0, time.Hour, nil)
	e.plan()

	byTitle := map[string]int{}
	for _, row := range e.queueOfKind(domain.KindActivity) {
		byTitle[e.payloadOf(row).Title]++
	}
	if byTitle["Piscine"] != 1 {
		t.Errorf("the change made after the upgrade produced %d notifications, want 1", byTitle["Piscine"])
	}
	if byTitle["Dentiste"] != 1 {
		t.Errorf("the change announced before the upgrade is queued %d times, want 1", byTitle["Dentiste"])
	}

	// And the repair is a one-off: the witness is on record, so the next pass has
	// something to check the cursor against and leaves it where it is.
	c, started, err := e.n.readActivityCursor(ctx)
	if err != nil || !started || !c.vouched() {
		t.Fatalf("activity cursor after the repair = %+v, %v, %v; want one that carries its witness", c, started, err)
	}
	cursor := e.activityCursor()
	e.plan()
	if got := e.activityCursor(); got != cursor {
		t.Errorf("a second pass moved the cursor from %d to %d: it repaired a cursor it could vouch for", cursor, got)
	}
	if n := len(e.queueOfKind(domain.KindActivity)); n != 2 {
		t.Errorf("a second pass left %d activity notifications, want 2", n)
	}
}

// Repairing the cursor walks the last day of the log again, and a re-read row is
// fanned out to the calendar's members as they stand *now* — so somebody who joined
// during that day used to be handed the whole window at once: six pushes about
// things that happened before they were a member, to say nothing of what they reveal
// about a calendar they could not see at the time. A member hears about what changed
// after they joined, on a repair pass as on any other.
func TestARepairDoesNotBackfillAMemberWhoJustJoined(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	ctx := context.Background()
	e.noDigests()

	actor := e.user("alice")
	watcher := e.user("bruno")
	newcomer := e.user("chloe")
	family := e.calendar("Famille", actor.ID)
	trip := e.calendar("Vacances", actor.ID)
	e.join(family.ID, watcher.ID)
	e.join(trip.ID, watcher.ID)

	e.plan() // the first pass only takes the high-water mark

	// A day's changes, spread across it, all inside the repair's lookback.
	for i := 1; i <= 5; i++ {
		e.timedEvent(family, actor.ID, fmt.Sprintf("Course %d", i), 2027, time.June, 2, 9+i, 0, time.Hour, nil)
		e.clk.Advance(time.Hour)
	}
	// One change in the calendar that is about to go, so that deleting it strands
	// the cursor above every id the log will hand out next.
	e.timedEvent(trip, actor.ID, "Ferry", 2027, time.June, 3, 9, 0, time.Hour, nil)
	e.plan()

	if err := e.st.DeleteCalendar(ctx, trip.ID); err != nil {
		t.Fatalf("delete the calendar holding the newest change: %v", err)
	}

	// chloe arrives after everything above, and one change is made once she is in.
	e.clk.Advance(time.Minute)
	e.join(family.ID, newcomer.ID)
	e.clk.Advance(time.Minute)
	e.timedEvent(family, actor.ID, "Piscine", 2027, time.June, 4, 17, 0, time.Hour, nil)

	e.plan()

	var told []string
	for _, row := range e.queueOfKind(domain.KindActivity) {
		if row.UserID == newcomer.ID {
			told = append(told, e.payloadOf(row).Title)
		}
	}
	if len(told) != 1 || told[0] != "Piscine" {
		t.Errorf("the member who joined during the lookback was told about %v, want only [Piscine]: "+
			"the repair walks the last day of the log again and fanned every row of it out to her", told)
	}

	// And the member who was there all along still hears each change exactly once.
	byTitle := map[string]int{}
	for _, row := range e.queueOfKind(domain.KindActivity) {
		if row.UserID == watcher.ID {
			byTitle[e.payloadOf(row).Title]++
		}
	}
	for i := 1; i <= 5; i++ {
		if title := fmt.Sprintf("Course %d", i); byTitle[title] != 1 {
			t.Errorf("%q is queued %d times for the member who was there all along, want 1", title, byTitle[title])
		}
	}
	if byTitle["Piscine"] != 1 {
		t.Errorf("the change made after the deletion is queued %d times, want 1", byTitle["Piscine"])
	}
}

// The same fault as the two above, on the reference the other half of the outbox is
// keyed by — and this one loses a reminder rather than an announcement.
//
// A reminder was identified by (user, event id, occurrence date, reminder id, instant),
// and every one of those ids is handed out again once the row holding it has gone. The
// outbox prunes a deleted event's undelivered rows and deliberately keeps its delivered
// ones — they are the record that the family was told, which is what stops a catch-up
// pass sending them a second time — and nothing prunes the outbox by age, so a delivered
// row holds its slot in that UNIQUE index for good. Delete the appointment that held the
// highest ids, make it again on the same date with the same reminder, and INSERT OR
// IGNORE reads the new reminder as the one already sent and drops it. Nobody is warned
// about the appointment the family actually has, and nothing looks wrong: the event is
// in the calendar and the outbox holds one row that did its job.
//
// "Delete the thing I just made and make it again properly" is the shape that produces
// it, which is why the replacement here is made the same evening rather than contrived.
func TestAnAppointmentTakingAReusedEventIDIsStillReminded(t *testing.T) {
	e := newEnv(t, time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC)) // 08:00 in Paris
	ctx := context.Background()

	claire := e.user("claire")
	cal := e.calendar("Famille", claire.ID)
	e.subscribe(claire.ID, "iphone")
	e.noDigests()

	dentiste := e.timedEvent(cal, claire.ID, "Dentiste", 2026, time.June, 2, 16, 30, time.Hour, nil)
	e.reminderAt(dentiste, claire.ID, 1, "09:00") // the day before, at 09:00

	e.plan()
	e.clk.Set(time.Date(2026, 6, 1, 9, 0, 0, 0, paris))
	e.dispatch()
	if got := len(e.push.received()); got != 1 {
		t.Fatalf("the reminder for the original appointment produced %d pushes, want 1", got)
	}

	// That evening it turns out to be the orthodontist, so the entry is deleted and
	// made again. Deleting it prunes the outbox of everything still waiting for it;
	// the row already delivered stays, which is the whole of the setup.
	e.clk.Set(time.Date(2026, 6, 1, 20, 0, 0, 0, paris))
	if err := e.ev.Delete(ctx, claire.ID, dentiste.ID, domain.ScopeAll, domain.Date{}); err != nil {
		t.Fatalf("delete the appointment: %v", err)
	}
	ortho := e.timedEvent(cal, claire.ID, "Orthodontiste", 2026, time.June, 2, 16, 30, time.Hour, nil)
	e.reminderAt(ortho, claire.ID, 1, "09:00")

	if ortho.ID != dentiste.ID {
		t.Fatalf("the replacement took event id %d and the one it replaced had %d: this SQLite is not "+
			"reusing the ids of deleted rows, so this test no longer reproduces the fault",
			ortho.ID, dentiste.ID)
	}
	if ids := reminderIDs(t, e); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("the replacement's reminders are %v, want the reused id [1]: this test no longer "+
			"reproduces the fault", ids)
	}

	e.plan()
	e.dispatch()

	byTitle := map[string]int{}
	for _, row := range e.queueOfKind(domain.KindReminder) {
		byTitle[e.payloadOf(row).Title]++
	}
	if byTitle["Orthodontiste"] != 1 {
		t.Errorf("the appointment that took the reused ids is queued %d times, want 1: its reminder "+
			"falls on the instant the deleted appointment's did, and that row is still in the outbox",
			byTitle["Orthodontiste"])
	}
	if got := len(e.push.received()); got != 2 {
		t.Errorf("%d pushes went out in all, want 2: the family has an appointment tomorrow and has "+
			"been warned about it once", got)
	}
	// The delivered row is left exactly where it is: it is the record that the first
	// warning went out, and deleting it is what would let a catch-up pass send it again.
	if byTitle["Dentiste"] != 1 {
		t.Errorf("the warning already sent is in the outbox %d times, want 1", byTitle["Dentiste"])
	}
}

// The other side of naming the events: a reminder's row in the outbox has to stay put
// across passes. The planner recomputes the whole window on every tick and relies on
// INSERT OR IGNORE doing nothing for a row it has already queued, and on reconcile
// deleting the rows this pass would no longer produce — reminders are reconcilable,
// unlike activity. A name that changed between two passes over one unchanged reminder
// would therefore file a second row on every tick and push it, which is a worse fault
// than the one being fixed: instead of one appointment going unannounced, every
// appointment in the house is announced over and over.
//
// The three shapes are here because the name is read off a different row in each: a
// plain event's own, a series occurrence's template (the expansion carries the
// template's fields), and, for an occurrence somebody has edited, the copy standing in
// for that date.
func TestReplanningAnUnchangedReminderQueuesItOnce(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()

	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	e.subscribe(u.ID, "iphone")

	dentiste := e.timedEvent(cal, u.ID, "Dentiste", 2027, time.June, 2, 16, 30, time.Hour, nil)
	e.reminderMinutes(dentiste, u.ID, 30)
	piscine := e.timedEvent(cal, u.ID, "Piscine", 2027, time.June, 1, 17, 0, time.Hour, &domain.Recurrence{
		Freq: domain.FreqDaily, Interval: 1,
	})
	e.reminderMinutes(piscine, u.ID, 30)

	moved := date(2027, 6, 2)
	newStart := time.Date(2027, 6, 2, 19, 0, 0, 0, paris)
	if _, err := e.ev.Update(e.ctx, u.ID, piscine.ID, domain.ScopeThis, moved, events.Input{
		CalendarID: cal.ID, Title: "Piscine (retardée)",
		StartsAt: newStart.UTC(), EndsAt: newStart.Add(time.Hour).UTC(),
		LabelID: piscine.LabelID, Participants: []int64{u.ID},
	}); err != nil {
		t.Fatalf("move one occurrence: %v", err)
	}

	// Four passes, spaced as the scheduler's ticks are. Nothing changes in between,
	// so every pass after the first must be a no-op.
	var first []string
	for pass := range 4 {
		e.plan()
		var refs []string
		for _, row := range e.queueOfKind(domain.KindReminder) {
			refs = append(refs, row.SourceRef)
		}
		sort.Strings(refs)
		switch pass {
		case 0:
			first = refs
			if len(refs) == 0 {
				t.Fatal("the first pass queued no reminders at all")
			}
		default:
			if !slices.Equal(refs, first) {
				t.Fatalf("pass %d left the outbox holding\n\t%v\nand the first pass left\n\t%v\n"+
					"a reference that moves between two passes over one unchanged reminder is a "+
					"second push on every tick", pass+1, refs, first)
			}
		}
		e.clk.Advance(30 * time.Second)
	}

	// And the family is warned once per occurrence rather than once per pass.
	e.clk.Set(time.Date(2027, 6, 3, 12, 0, 0, 0, paris))
	e.dispatch()
	byTitle := map[string]int{}
	for _, row := range e.queueOfKind(domain.KindReminder) {
		byTitle[e.payloadOf(row).Title]++
	}
	for title, want := range map[string]int{"Dentiste": 1, "Piscine (retardée)": 1} {
		if byTitle[title] != want {
			t.Errorf("%q is queued %d times, want %d", title, byTitle[title], want)
		}
	}
}

// The reproduction from #65, driven through a real store and the real planner. A
// reminder is filed in the outbox under its own id among other things, and
// ReplaceReminders used to delete every row in the list and insert it again — so the id
// moved on every save unless the deleted rows happened to be the highest in the table.
// The already-delivered row then no longer absorbed the re-plan, and the second copy was
// not merely queued but sent: a reminder whose slot has passed is planned while its
// event is still ahead, and staleness lets a late warning through on purpose.
//
// The action that triggers it is opening the reminder editor and pressing save without
// changing anything, which is why "low-moderate" is as low as it goes: a household that
// does it twice gets three copies.
func TestReSavingAnUnchangedReminderListDoesNotSendItAgain(t *testing.T) {
	e := newEnv(t, time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC)) // 08:00 in Paris

	claire := e.user("claire")
	cal := e.calendar("Famille", claire.ID)
	e.subscribe(claire.ID, "iphone")
	e.noDigests()

	dentiste := e.timedEvent(cal, claire.ID, "Dentiste", 2026, time.June, 2, 16, 30, time.Hour, nil)
	e.reminderAt(dentiste, claire.ID, 1, "09:00") // the day before, at 09:00

	// A reminder set afterwards, on something far enough off that no pass in this test
	// plans it. It is here only to hold a row number above the one under test, which is
	// what stops the re-insert landing back on the id it just freed.
	piscine := e.timedEvent(cal, claire.ID, "Piscine", 2026, time.June, 20, 17, 0, time.Hour, nil)
	e.reminderMinutes(piscine, claire.ID, 30)

	own := func() domain.Reminder {
		t.Helper()
		id := dentiste.ID
		rs, err := e.st.ListReminders(e.ctx, &id, nil, claire.ID)
		if err != nil || len(rs) != 1 {
			t.Fatalf("Claire's reminders for the appointment = %+v, %v; want exactly one", rs, err)
		}
		return rs[0]
	}
	before := own()
	if all := reminderIDs(t, e); before.ID >= slices.Max(all) {
		t.Fatalf("the appointment's reminder is row %d of %v, the highest there is: deleting it "+
			"would hand the id straight back, so this test no longer reproduces the fault",
			before.ID, all)
	}

	// 09:00, and the family is warned about tomorrow's appointment.
	e.plan()
	e.clk.Set(time.Date(2026, 6, 1, 9, 0, 0, 0, paris))
	e.dispatch()
	if got := len(e.push.received()); got != 1 {
		t.Fatalf("the reminder produced %d pushes, want 1", got)
	}

	// An hour later somebody opens the reminder editor and presses save. The list is
	// the one that is already there.
	e.clk.Set(time.Date(2026, 6, 1, 10, 0, 0, 0, paris))
	e.reminderAt(dentiste, claire.ID, 1, "09:00")

	if after := own(); after.ID != before.ID {
		t.Errorf("the reminder was row %d before the save and is row %d after (all of them: %v): "+
			"the outbox files it under that number, so it is now a reminder the family has not "+
			"been told about", before.ID, after.ID, reminderIDs(t, e))
	}

	e.plan()
	e.dispatch()

	queued := 0
	for _, row := range e.queueOfKind(domain.KindReminder) {
		if e.payloadOf(row).Title == "Dentiste" {
			queued++
		}
	}
	if queued != 1 {
		t.Errorf("the appointment is in the outbox %d times, want 1: saving the same list again "+
			"is not a second reminder", queued)
	}
	if got := len(e.push.received()); got != 1 {
		t.Errorf("%d pushes went out in all, want 1: the warning went out at 09:00 and nothing "+
			"has changed since", got)
	}
}

// The other half of the same fix, and the case it must not buy the first with. Moving a
// reminder is a different warning at a different instant, so the row already queued for
// the old one has to go — a save that stopped the duplicate but left the reminder firing
// at the time it used to would be worse than the fault it closed.
//
// Nothing prunes on this path: PUT /events/{id}/reminders writes the rows and does not
// touch the outbox. What removes the old row is the planner recomputing the window and
// dropping the undelivered rows it would no longer produce, which is where every edit
// that invalidates the outbox is answered at once.
func TestMovingAReminderMovesTheRowQueuedForIt(t *testing.T) {
	e := newEnv(t, time.Date(2026, 6, 1, 6, 0, 0, 0, time.UTC)) // 08:00 in Paris

	claire := e.user("claire")
	cal := e.calendar("Famille", claire.ID)
	e.subscribe(claire.ID, "iphone")
	e.noDigests()

	dentiste := e.timedEvent(cal, claire.ID, "Dentiste", 2026, time.June, 1, 18, 0, time.Hour, nil)
	e.reminderMinutes(dentiste, claire.ID, 60)
	e.plan()

	// An hour beforehand turns out to be too soon to be useful; half an hour it is.
	e.clk.Set(time.Date(2026, 6, 1, 9, 0, 0, 0, paris))
	e.reminderMinutes(dentiste, claire.ID, 30)
	e.plan()

	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 {
		var when []string
		for _, row := range rows {
			when = append(when, wall(row.DueAt))
		}
		t.Fatalf("%d reminders are queued for one appointment after moving 60 minutes to 30 (%v), want 1", len(rows), when)
	}
	if want := time.Date(2026, 6, 1, 17, 30, 0, 0, paris); !rows[0].DueAt.Equal(want) {
		t.Fatalf("the queued reminder is due %s, want %s: the row left behind is the old warning",
			wall(rows[0].DueAt), wall(want))
	}

	// 17:00 comes and goes in silence, and the warning arrives at 17:30.
	e.clk.Set(time.Date(2026, 6, 1, 17, 5, 0, 0, paris))
	e.dispatch()
	if got := len(e.push.received()); got != 0 {
		t.Errorf("%d pushes went out at 17:05, want 0: that is the reminder the family moved", got)
	}
	e.clk.Set(time.Date(2026, 6, 1, 17, 31, 0, 0, paris))
	e.dispatch()
	if got := len(e.push.received()); got != 1 {
		t.Errorf("%d pushes went out in all, want 1", got)
	}
}

// reminderIDs is every reminder row in the database, in the order the planner walks
// them. A test that means to reproduce a reused id has to say which ids it got.
func reminderIDs(t *testing.T, e *env) []int64 {
	t.Helper()
	all, err := e.st.ListAllReminders(e.ctx)
	if err != nil {
		t.Fatalf("list reminders: %v", err)
	}
	ids := make([]int64, 0, len(all))
	for _, r := range all {
		ids = append(ids, r.ID)
	}
	return ids
}

// The constraint the name had to be fitted around. A reference is a prune key before it
// is an identity: internal/events deletes "everything queued for this occurrence" when
// one moves and "everything queued for this event" when a series changes shape or goes,
// both by prefix, and a reference the prefixes no longer reach is a reminder that goes
// on firing for an appointment nobody has any more.
//
// TestSourceRefPrefixesNest holds the arrangement on the strings. This holds it on the
// rows the planner actually writes, where the names are the ones the store minted rather
// than any a test chose.
func TestBothPrunesStillReachAReferenceCarryingAName(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)

	series := e.timedEvent(cal, u.ID, "Piscine", 2027, 6, 1, 17, 0, time.Hour, &domain.Recurrence{
		Freq: domain.FreqDaily, Interval: 1,
	})
	e.reminderMinutes(series, u.ID, 30)
	e.plan()

	pending := func() []string {
		t.Helper()
		rows, err := e.st.ListUnsentBefore(e.ctx, e.clk.Now().Add(e.n.Horizon()))
		if err != nil {
			t.Fatalf("list unsent: %v", err)
		}
		var refs []string
		for _, r := range rows {
			if r.Kind == domain.KindReminder {
				refs = append(refs, r.SourceRef)
			}
		}
		sort.Strings(refs)
		return refs
	}
	before := pending()
	if len(before) < 2 {
		t.Fatalf("the series queued %v, want at least two occurrences to tell the two prunes apart", before)
	}
	named := 0
	for _, ref := range before {
		if strings.Count(ref, ":") == 4 {
			named++
		}
	}
	if named != len(before) {
		t.Fatalf("%d of %d queued references carry a name (%v); this test proves nothing about the "+
			"prunes reaching the named spelling if the planner is not writing it", named, len(before), before)
	}

	// The occurrence prune: moving one lesson takes that date's rows and leaves the rest.
	moved := date(2027, 6, 2)
	newStart := time.Date(2027, 6, 2, 19, 0, 0, 0, paris)
	if _, err := e.ev.Update(e.ctx, u.ID, series.ID, domain.ScopeThis, moved, events.Input{
		CalendarID: cal.ID, Title: "Piscine (retardée)",
		StartsAt: newStart.UTC(), EndsAt: newStart.Add(time.Hour).UTC(),
		LabelID: series.LabelID, Participants: []int64{u.ID},
	}); err != nil {
		t.Fatalf("move one occurrence: %v", err)
	}
	prefix := events.OccurrenceSourcePrefix(series.ID, moved)
	after := pending()
	for _, ref := range after {
		if strings.HasPrefix(ref, prefix) {
			t.Errorf("%q survived the move of %s: the occurrence prune no longer reaches a reference "+
				"carrying a name, so the lesson is still announced at the time it left", ref, moved)
		}
	}
	if len(after) != len(before)-1 {
		t.Errorf("the occurrence prune left %v and the queue held %v: it took more than the one "+
			"occurrence that moved", after, before)
	}

	// The event prune: the whole series goes, and so does everything queued for it.
	if err := e.ev.Delete(e.ctx, u.ID, series.ID, domain.ScopeAll, domain.Date{}); err != nil {
		t.Fatalf("delete the series: %v", err)
	}
	if refs := pending(); len(refs) != 0 {
		t.Errorf("%v survived the deletion of the series: the event prune no longer reaches a "+
			"reference carrying a name, so a deleted series goes on reminding the family", refs)
	}
}

// Deleting a calendar prunes the outbox of the notifications it was about to produce,
// which is what stops a reminder for an appointment nobody has any more going off two
// days later. It only ever pruned half of them: the query matched reminder references
// alone, and an activity reference names the change rather than the calendar, so it
// matched nothing. Every announcement of a change made in the calendar stayed queued and
// went out — telling somebody about a calendar they no longer have, with a click-through
// to an event that no longer exists.
//
// The window is a day at most, since delivery drops an activity notification more than
// maxActivityLateness behind its slot, and nothing is lost by it. It is here because the
// comment above the query said it was already doing this.
func TestDeletingACalendarTakesItsQueuedAnnouncementsWithIt(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	ctx := context.Background()

	actor := e.user("alice")
	watcher := e.user("bruno")
	family := e.calendar("Famille", actor.ID)
	trip := e.calendar("Vacances", actor.ID)
	e.join(family.ID, watcher.ID)
	e.join(trip.ID, watcher.ID)
	e.subscribe(watcher.ID, "iphone")
	e.noDigests()

	e.plan() // the first pass only takes the high-water mark

	e.timedEvent(trip, actor.ID, "Ferry", 2027, time.June, 3, 9, 0, time.Hour, nil)
	e.clk.Advance(time.Second)
	e.timedEvent(family, actor.ID, "Dentiste", 2027, time.June, 2, 16, 30, time.Hour, nil)
	e.plan()
	if n := len(e.queueOfKind(domain.KindActivity)); n != 2 {
		t.Fatalf("two changes in two calendars produced %d announcements, want 2", n)
	}

	if err := e.st.DeleteCalendar(ctx, trip.ID); err != nil {
		t.Fatalf("delete the calendar: %v", err)
	}
	e.dispatch()

	var sent []string
	for _, row := range e.queueOfKind(domain.KindActivity) {
		if !row.SentAt.IsZero() {
			sent = append(sent, e.payloadOf(row).Title)
		}
	}
	if slices.Contains(sent, "Ferry") {
		t.Errorf("the change in the deleted calendar was announced anyway (delivered: %v): the family "+
			"is told about a calendar it no longer has, and the notification opens an event that has "+
			"gone with it", sent)
	}
	// The other calendar's announcement is not collateral: the prune is scoped.
	if !slices.Contains(sent, "Dentiste") {
		t.Errorf("the surviving calendar's change was not announced (delivered: %v): deleting one "+
			"calendar has emptied the outbox of another's", sent)
	}
	if got := len(e.push.received()); got != 1 {
		t.Errorf("%d pushes went out, want 1 — the surviving calendar's", got)
	}
}
