package notify

import (
	"testing"
	"time"

	"agenda/internal/domain"
	"agenda/internal/events"
)

// TestPlanIsIdempotent is the property the whole design rests on: the planner may
// recompute its window on every 30-second tick because UNIQUE(user_id, kind,
// source_ref, due_at) plus INSERT OR IGNORE makes a repeat pass free. If this ever
// fails, every reminder is delivered once per tick.
func TestPlanIsIdempotent(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 2, 16, 30, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)

	e.plan()
	first := e.queue()
	if len(first) == 0 {
		t.Fatal("the first pass planned nothing")
	}

	for i := 0; i < 5; i++ {
		e.plan()
	}
	second := e.queue()
	if len(second) != len(first) {
		t.Fatalf("re-planning grew the queue from %d to %d rows", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID || first[i].SourceRef != second[i].SourceRef {
			t.Fatalf("row %d changed identity: %+v then %+v", i, first[i], second[i])
		}
	}

	// And it stays idempotent once rows have been delivered: a sent row must not
	// be re-created by the next pass, or every reminder would go out again on the
	// following tick.
	e.clk.Set(time.Date(2027, 6, 2, 14, 0, 0, 0, time.UTC)) // the reminder's slot
	e.dispatch()
	e.plan()
	seen := map[string]int{}
	sent := 0
	for _, r := range e.queue() {
		seen[string(r.Kind)+"|"+r.SourceRef+"|"+r.DueAt.Format(time.RFC3339)]++
		if !r.SentAt.IsZero() {
			sent++
		}
	}
	if sent == 0 {
		t.Fatal("nothing was delivered, so the re-planning check proves nothing")
	}
	for key, n := range seen {
		if n != 1 {
			t.Errorf("re-planning after delivery produced %d rows for %s, want 1", n, key)
		}
	}
}

// TestTimedReminderDueTime pins the arithmetic for timed events, including both
// Europe/Paris daylight-saving transitions.
//
// The claim being tested is precise: a reminder set 30 minutes before a 16:30
// event fires at 16:00 *local* on every occurrence, whatever the UTC offset is
// that week. A weekly series is used rather than four separate events because
// that is the case where a naive implementation drifts — one stored instant,
// four occurrences, two offsets.
func TestTimedReminderDueTime(t *testing.T) {
	e := newEnv(t, time.Date(2027, 3, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)

	// Every Sunday at 16:30, starting three weeks before the spring transition.
	ev := e.timedEvent(cal, u.ID, "Piscine", 2027, 3, 21, 16, 30, time.Hour, &domain.Recurrence{
		Freq:      domain.FreqWeekly,
		Interval:  1,
		ByWeekday: []time.Weekday{time.Sunday},
	})
	e.reminderMinutes(ev, u.ID, 30)

	tests := []struct {
		name    string
		occDate domain.Date
		wantUTC string // the reminder instant
		wantLoc string // ... and what it reads as on a Paris wall clock
	}{
		{"winter, before the spring forward", date(2027, 3, 21), "2027-03-21T15:00:00Z", "2027-03-21 16:00 CET"},
		{"summer, on the spring forward day", date(2027, 3, 28), "2027-03-28T14:00:00Z", "2027-03-28 16:00 CEST"},
		{"summer, before the fall back", date(2027, 10, 24), "2027-10-24T14:00:00Z", "2027-10-24 16:00 CEST"},
		{"winter, on the fall back day", date(2027, 10, 31), "2027-10-31T15:00:00Z", "2027-10-31 16:00 CET"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Stand a day before the occurrence so it falls inside the horizon.
			e.clk.Set(tc.occDate.AddDays(-1).At(12, 0, paris))
			e.plan()

			ref := events.ReminderSourceRef(ev.ID, tc.occDate, reminderIDOf(t, e, u.ID))
			row, ok := findRow(e.queue(), ref)
			if !ok {
				t.Fatalf("no queued reminder for %s (source %q)", tc.occDate, ref)
			}
			if got := row.DueAt.Format(time.RFC3339); got != tc.wantUTC {
				t.Errorf("due_at = %s, want %s", got, tc.wantUTC)
			}
			if got := wall(row.DueAt); got != tc.wantLoc {
				t.Errorf("due_at in Paris = %s, want %s (a 16:30 event must keep its reminder 30 minutes before *local* 16:30)", got, tc.wantLoc)
			}
		})
	}
}

// TestTimedReminderOffsetIsAbsoluteAcrossTheGap pins the other half of the DST
// contract, which is easy to get wrong in the opposite direction: "two hours
// before" is two hours of real time. Across the spring-forward gap that lands on
// a wall clock two hours *and one skipped hour* earlier, and across the fall-back
// ambiguity it lands in the first pass of the repeated hour.
func TestTimedReminderOffsetIsAbsoluteAcrossTheGap(t *testing.T) {
	tests := []struct {
		name             string
		y                int
		mo               time.Month
		d, h, min        int
		offset           int
		wantUTC, wantLoc string
	}{
		{
			name: "spring forward: 03:30 CEST minus two real hours is 00:30 CET",
			y:    2027, mo: 3, d: 28, h: 3, min: 30, offset: 120,
			wantUTC: "2027-03-27T23:30:00Z", wantLoc: "2027-03-28 00:30 CET",
		},
		{
			name: "fall back: 03:30 CET minus two real hours is 02:30 CEST, the first pass of the repeated hour",
			y:    2027, mo: 10, d: 31, h: 3, min: 30, offset: 120,
			wantUTC: "2027-10-31T00:30:00Z", wantLoc: "2027-10-31 02:30 CEST",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, time.Date(tc.y, tc.mo, tc.d-1, 6, 0, 0, 0, time.UTC))
			e.noDigests()
			u := e.user("alice")
			cal := e.calendar("Famille", u.ID)
			ev := e.timedEvent(cal, u.ID, "Vol", tc.y, tc.mo, tc.d, tc.h, tc.min, time.Hour, nil)
			e.reminderMinutes(ev, u.ID, tc.offset)
			e.plan()

			rows := e.queueOfKind(domain.KindReminder)
			if len(rows) != 1 {
				t.Fatalf("got %d reminder rows, want 1", len(rows))
			}
			if got := rows[0].DueAt.Format(time.RFC3339); got != tc.wantUTC {
				t.Errorf("due_at = %s, want %s", got, tc.wantUTC)
			}
			if got := wall(rows[0].DueAt); got != tc.wantLoc {
				t.Errorf("due_at in Paris = %s, want %s", got, tc.wantLoc)
			}
		})
	}
}

// TestAllDayReminderDueTime covers why all-day reminders are stored as
// days_before + at_time_local rather than as an offset: "09:00 on the day" is not
// a duration before midnight, and neither form survives a DST change if it is
// computed in UTC.
func TestAllDayReminderDueTime(t *testing.T) {
	tests := []struct {
		name             string
		day              domain.Date
		daysBefore       int
		at               string
		wantUTC, wantLoc string
	}{
		{
			name: "09:00 on the day", day: date(2027, 6, 10), daysBefore: 0, at: "09:00",
			wantUTC: "2027-06-10T07:00:00Z", wantLoc: "2027-06-10 09:00 CEST",
		},
		{
			name: "09:00 the day before", day: date(2027, 6, 10), daysBefore: 1, at: "09:00",
			wantUTC: "2027-06-09T07:00:00Z", wantLoc: "2027-06-09 09:00 CEST",
		},
		{
			name: "09:00 on the day, in winter: same wall clock, different offset",
			day:  date(2027, 12, 10), daysBefore: 0, at: "09:00",
			wantUTC: "2027-12-10T08:00:00Z", wantLoc: "2027-12-10 09:00 CET",
		},
		{
			name: "09:00 the day before, across the spring forward",
			day:  date(2027, 3, 28), daysBefore: 1, at: "09:00",
			wantUTC: "2027-03-27T08:00:00Z", wantLoc: "2027-03-27 09:00 CET",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Stand far enough back that the slot is inside the horizon.
			e := newEnv(t, tc.day.AddDays(-tc.daysBefore).At(6, 0, paris).Add(-time.Hour))
			e.noDigests()
			u := e.user("alice")
			cal := e.calendar("Famille", u.ID)
			ev := e.allDayEvent(cal, u.ID, "Sortie scolaire", tc.day)
			e.reminderAt(ev, u.ID, tc.daysBefore, tc.at)
			e.plan()

			rows := e.queueOfKind(domain.KindReminder)
			if len(rows) != 1 {
				t.Fatalf("got %d reminder rows, want 1", len(rows))
			}
			if got := rows[0].DueAt.Format(time.RFC3339); got != tc.wantUTC {
				t.Errorf("due_at = %s, want %s", got, tc.wantUTC)
			}
			if got := wall(rows[0].DueAt); got != tc.wantLoc {
				t.Errorf("due_at in Paris = %s, want %s", got, tc.wantLoc)
			}
			p := e.payloadOf(rows[0])
			if !p.AllDay || p.EventDate != tc.day {
				t.Errorf("payload = %+v, want an all-day payload dated %s", p, tc.day)
			}
		})
	}
}

// TestDigestAcrossDSTTransitions: 07:30 is 07:30 all year. A digest computed by
// adding a fixed number of hours to midnight UTC would arrive at 08:30 for half
// the year, which is exactly the bug the family-timezone rule exists to prevent.
func TestDigestAcrossDSTTransitions(t *testing.T) {
	tests := []struct {
		name             string
		day              domain.Date
		wantUTC, wantLoc string
	}{
		{"the day before the spring forward", date(2027, 3, 27), "2027-03-27T06:30:00Z", "2027-03-27 07:30 CET"},
		{"the spring forward day", date(2027, 3, 28), "2027-03-28T05:30:00Z", "2027-03-28 07:30 CEST"},
		{"the day before the fall back", date(2027, 10, 30), "2027-10-30T05:30:00Z", "2027-10-30 07:30 CEST"},
		{"the fall back day", date(2027, 10, 31), "2027-10-31T06:30:00Z", "2027-10-31 07:30 CET"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.day.AddDays(-1).At(20, 0, paris))
			u := e.user("alice")
			cal := e.calendar("Famille", u.ID)
			e.timedEvent(cal, u.ID, "Marché", tc.day.Year, tc.day.Month, tc.day.Day, 10, 0, time.Hour, nil)
			e.setPrefs(domain.NotificationPrefs{
				UserID: u.ID, DigestEnabled: true, DigestTime: "07:30", SummaryTime: "20:00",
				EmailReminders: true,
			})
			e.plan()

			ref := events.DigestSourceRef(tc.day)
			row, ok := findRow(e.queueOfKind(domain.KindDigest), ref)
			if !ok {
				t.Fatalf("no digest queued for %s", tc.day)
			}
			if got := row.DueAt.Format(time.RFC3339); got != tc.wantUTC {
				t.Errorf("digest due_at = %s, want %s", got, tc.wantUTC)
			}
			if got := wall(row.DueAt); got != tc.wantLoc {
				t.Errorf("digest due_at in Paris = %s, want %s", got, tc.wantLoc)
			}
			if p := e.payloadOf(row); p.Total != 1 || p.Day != tc.day {
				t.Errorf("digest payload = %+v, want one event on %s", p, tc.day)
			}
		})
	}
}

// TestDigestOnEmpty: the setting decides whether a quiet day is worth a push.
func TestDigestOnEmpty(t *testing.T) {
	for _, onEmpty := range []bool{false, true} {
		name := "off"
		if onEmpty {
			name = "on"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, time.Date(2027, 6, 1, 4, 0, 0, 0, time.UTC))
			u := e.user("alice")
			e.calendar("Famille", u.ID)
			e.setPrefs(domain.NotificationPrefs{
				UserID: u.ID, DigestEnabled: true, DigestTime: "07:30",
				DigestOnEmpty: onEmpty, SummaryTime: "20:00", EmailReminders: true,
			})
			e.plan()

			got := len(e.queueOfKind(domain.KindDigest))
			if onEmpty && got == 0 {
				t.Error("digest_on_empty is set but no digest was queued for an empty day")
			}
			if !onEmpty && got != 0 {
				t.Errorf("digest_on_empty is off but %d digests were queued for empty days", got)
			}
		})
	}
}

// TestMutedAndParticipatingOnlyExcluded: the two per-calendar settings that stop
// a reminder before it is ever materialized. Both are enforced by
// events.UserOccurrences so that no notification kind reimplements them; this
// test proves the planner actually goes through it.
func TestMutedAndParticipatingOnlyExcluded(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()

	owner := e.user("alice")
	muted := e.user("bruno")
	partOnly := e.user("chloe")
	normal := e.user("david")
	cal := e.calendar("Famille", owner.ID)
	for _, u := range []domain.User{muted, partOnly, normal} {
		e.join(cal.ID, u.ID)
	}
	e.membership(cal.ID, muted.ID, true, false)
	e.membership(cal.ID, partOnly.ID, false, true)

	// The event concerns alice only; chloe is not a participant.
	ev := e.timedEvent(cal, owner.ID, "Dentiste", 2027, 6, 2, 16, 30, time.Hour, nil)
	for _, u := range []domain.User{muted, partOnly, normal} {
		e.reminderMinutes(ev, u.ID, 30)
	}
	e.plan()

	got := map[int64]int{}
	for _, r := range e.queueOfKind(domain.KindReminder) {
		got[r.UserID]++
	}
	if got[muted.ID] != 0 {
		t.Errorf("a muted calendar still produced %d reminders", got[muted.ID])
	}
	if got[partOnly.ID] != 0 {
		t.Errorf("participating_only produced %d reminders for a non-participant", got[partOnly.ID])
	}
	if got[normal.ID] != 1 {
		t.Errorf("an ordinary member got %d reminders, want 1", got[normal.ID])
	}

	// ... and participating_only lets a reminder through once the person is on
	// the event, which is the half of the rule that would go unnoticed if it broke.
	if _, err := e.ev.Update(e.ctx, owner.ID, ev.ID, "", domain.Date{}, events.Input{
		CalendarID: cal.ID, Title: ev.Title, StartsAt: ev.StartsAt, EndsAt: ev.EndsAt,
		LabelID: ev.LabelID, Participants: []int64{owner.ID, partOnly.ID},
	}); err != nil {
		t.Fatalf("add participant: %v", err)
	}
	e.plan()
	got = map[int64]int{}
	for _, r := range e.queueOfKind(domain.KindReminder) {
		got[r.UserID]++
	}
	if got[partOnly.ID] != 1 {
		t.Errorf("participating_only member is now a participant but got %d reminders, want 1", got[partOnly.ID])
	}
}

// TestEditingAnEventPrunesQueuedRows drives the reconciliation half of
// the notification rules in docs/architecture.md through the real editor: a moved event must not fire its old
// reminder. internal/events prunes by source-reference prefix and the planner
// re-materializes; neither half is any use without the other.
func TestEditingAnEventPrunesQueuedRows(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	ev := e.timedEvent(cal, u.ID, "Dentiste", 2027, 6, 2, 16, 30, time.Hour, nil)
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()

	rows := e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 {
		t.Fatalf("got %d reminder rows, want 1", len(rows))
	}
	stale := rows[0]
	if got := wall(stale.DueAt); got != "2027-06-02 16:00 CEST" {
		t.Fatalf("reminder due at %s, want 2027-06-02 16:00 CEST", got)
	}

	// Move the appointment two hours later.
	newStart := time.Date(2027, 6, 2, 18, 30, 0, 0, paris)
	if _, err := e.ev.Update(e.ctx, u.ID, ev.ID, "", domain.Date{}, events.Input{
		CalendarID: cal.ID, Title: ev.Title,
		StartsAt: newStart.UTC(), EndsAt: newStart.Add(time.Hour).UTC(),
		LabelID: ev.LabelID, Participants: []int64{u.ID},
	}); err != nil {
		t.Fatalf("move event: %v", err)
	}

	for _, r := range e.queue() {
		if r.ID == stale.ID {
			t.Fatalf("the row queued for the old time survived the edit: %+v", r)
		}
	}

	e.plan()
	rows = e.queueOfKind(domain.KindReminder)
	if len(rows) != 1 {
		t.Fatalf("after re-planning got %d reminder rows, want 1", len(rows))
	}
	if got := wall(rows[0].DueAt); got != "2027-06-02 18:00 CEST" {
		t.Errorf("reminder for the moved event is at %s, want 2027-06-02 18:00 CEST", got)
	}
}

// TestCancelledOccurrenceProducesNoReminder: the same pruning path, for one
// occurrence of a series. A cancelled dentist appointment must go quiet.
func TestCancelledOccurrenceProducesNoReminder(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	ev := e.timedEvent(cal, u.ID, "Piscine", 2027, 6, 1, 17, 0, time.Hour, &domain.Recurrence{
		Freq: domain.FreqDaily, Interval: 1,
	})
	e.reminderMinutes(ev, u.ID, 30)
	e.plan()
	if n := len(e.queueOfKind(domain.KindReminder)); n < 2 {
		t.Fatalf("a daily series over 48 h queued %d reminders, want at least 2", n)
	}

	cancelled := date(2027, 6, 2)
	if err := e.ev.Delete(e.ctx, u.ID, ev.ID, domain.ScopeThis, cancelled); err != nil {
		t.Fatalf("cancel occurrence: %v", err)
	}
	e.plan()

	for _, r := range e.queueOfKind(domain.KindReminder) {
		if p := e.payloadOf(r); p.OccDate == cancelled {
			t.Fatalf("a cancelled occurrence still has a queued reminder: %+v", r)
		}
	}
}

// TestSeriesReminderFollowsAnOverride is the recurrence policy table in docs/architecture.md's normative
// inheritance rule: a series-level reminder applies to an edited occurrence, and
// is computed from *that occurrence's* start time. Moving one swimming lesson
// must move its reminder with it, and the queued row must still be keyed on the
// series event, because that is the prefix internal/events prunes by.
func TestSeriesReminderFollowsAnOverride(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	u := e.user("alice")
	cal := e.calendar("Famille", u.ID)
	series := e.timedEvent(cal, u.ID, "Piscine", 2027, 6, 1, 17, 0, time.Hour, &domain.Recurrence{
		Freq: domain.FreqDaily, Interval: 1,
	})
	e.reminderMinutes(series, u.ID, 30)

	moved := date(2027, 6, 2)
	newStart := time.Date(2027, 6, 2, 19, 0, 0, 0, paris)
	if _, err := e.ev.Update(e.ctx, u.ID, series.ID, domain.ScopeThis, moved, events.Input{
		CalendarID: cal.ID, Title: "Piscine (retardée)",
		StartsAt: newStart.UTC(), EndsAt: newStart.Add(time.Hour).UTC(),
		LabelID: series.LabelID, Participants: []int64{u.ID},
	}); err != nil {
		t.Fatalf("move one occurrence: %v", err)
	}
	e.plan()

	ref := events.ReminderSourceRef(series.ID, moved, reminderIDOf(t, e, u.ID))
	row, ok := findRow(e.queueOfKind(domain.KindReminder), ref)
	if !ok {
		t.Fatalf("no reminder queued for the edited occurrence (source %q); a series reminder must survive an override", ref)
	}
	if got := wall(row.DueAt); got != "2027-06-02 18:30 CEST" {
		t.Errorf("reminder for the moved occurrence is at %s, want 2027-06-02 18:30 CEST", got)
	}
	if p := e.payloadOf(row); p.Title != "Piscine (retardée)" {
		t.Errorf("payload title = %q, want the override's title", p.Title)
	}

	// The untouched occurrence keeps its original slot.
	untouched := events.ReminderSourceRef(series.ID, date(2027, 6, 1), reminderIDOf(t, e, u.ID))
	if row, ok := findRow(e.queueOfKind(domain.KindReminder), untouched); !ok {
		t.Error("the unedited occurrence lost its reminder")
	} else if got := wall(row.DueAt); got != "2027-06-01 16:30 CEST" {
		t.Errorf("unedited occurrence reminder is at %s, want 2027-06-01 16:30 CEST", got)
	}
}

// TestActivityNotifications: fan-out from the log, with the actor, muted members
// and summary-mode users excluded.
func TestActivityNotifications(t *testing.T) {
	e := newEnv(t, time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC))
	e.noDigests()
	actor := e.user("alice")
	watcher := e.user("bruno")
	muted := e.user("chloe")
	batched := e.user("david")
	cal := e.calendar("Famille", actor.ID)
	for _, u := range []domain.User{watcher, muted, batched} {
		e.join(cal.ID, u.ID)
	}
	e.membership(cal.ID, muted.ID, true, false)
	e.setPrefs(domain.NotificationPrefs{
		UserID: batched.ID, DigestTime: "07:30", DailySummaryMode: true,
		SummaryTime: "20:00", EmailReminders: true, ActivityPush: true,
	})

	// The first pass only sets the high-water mark: a fresh install must not
	// replay history into everyone's tray.
	e.plan()
	if n := len(e.queueOfKind(domain.KindActivity)); n != 0 {
		t.Fatalf("the first pass queued %d activity notifications, want 0", n)
	}

	e.timedEvent(cal, actor.ID, "Dentiste", 2027, 6, 2, 16, 30, time.Hour, nil)
	e.plan()

	got := map[int64]int{}
	for _, r := range e.queueOfKind(domain.KindActivity) {
		got[r.UserID]++
	}
	if got[actor.ID] != 0 {
		t.Errorf("the actor was notified about their own change %d times", got[actor.ID])
	}
	if got[muted.ID] != 0 {
		t.Errorf("a muted member was notified %d times", got[muted.ID])
	}
	if got[batched.ID] != 0 {
		t.Errorf("a summary-mode member got %d individual activity pushes, want 0", got[batched.ID])
	}
	if got[watcher.ID] != 1 {
		t.Errorf("the watching member got %d activity notifications, want 1", got[watcher.ID])
	}

	// Re-planning must not duplicate: the cursor is the guard.
	e.plan()
	if n := len(e.queueOfKind(domain.KindActivity)); n != 1 {
		t.Errorf("re-planning produced %d activity rows, want 1", n)
	}
}

// TestPlannerRecordsItsHorizon: without this marker CatchUp cannot know how big
// the hole an outage left is.
func TestPlannerRecordsItsHorizon(t *testing.T) {
	now := time.Date(2027, 6, 1, 6, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	e.plan()
	got, err := e.n.plannedThrough(e.ctx)
	if err != nil {
		t.Fatalf("planned through: %v", err)
	}
	if want := now.Add(DefaultHorizon); !got.Equal(want) {
		t.Errorf("planned through %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// ---------------------------------------------------------------------------

func findRow(rows []queueRow, sourceRef string) (queueRow, bool) {
	for _, r := range rows {
		if r.SourceRef == sourceRef {
			return r, true
		}
	}
	return queueRow{}, false
}

// reminderIDOf returns the single reminder a test user owns, so source references
// can be reconstructed exactly as the planner builds them.
func reminderIDOf(t *testing.T, e *env, userID int64) int64 {
	t.Helper()
	all, err := e.st.ListAllReminders(e.ctx)
	if err != nil {
		t.Fatalf("list reminders: %v", err)
	}
	for _, r := range all {
		if r.UserID == userID {
			return r.ID
		}
	}
	t.Fatalf("user %d has no reminders", userID)
	return 0
}
