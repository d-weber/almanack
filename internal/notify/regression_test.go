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
