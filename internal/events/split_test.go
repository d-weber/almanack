package events

import (
	"context"
	"testing"
	"time"

	"almanack/internal/domain"
)

// Regressions from an adversarial review of the split logic. Each of these was a
// way "this and following" quietly did the wrong thing to a family's calendar.

// Moving an occurrence to another weekday used to keep the old pattern: the series
// carried on at the old day and the occurrence the user had just moved did not
// exist at all.
func TestSplitFollowsTheDayTheOccurrenceMovedTo(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})

	// From 21 April swimming moves to Wednesday the 22nd. The client deliberately
	// sends no recurrence for this scope — the server owns the split.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-04-21"), Input{
		Title: "Piscine", StartsAt: f.at("2026-04-22", 18, 0), EndsAt: f.at("2026-04-22", 19, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}

	occ := f.occurrences(t, "2026-04-01", "2026-05-10")
	got := map[string]bool{}
	for _, o := range occ {
		got[o.OccurrenceDate.String()] = true
	}

	// The moved occurrence must exist...
	if !got["2026-04-22"] {
		t.Error("the occurrence the user moved to Wednesday 22 April does not exist")
	}
	// ...and the series must follow it to Wednesdays.
	for _, want := range []string{"2026-04-29", "2026-05-06"} {
		if !got[want] {
			t.Errorf("the series did not follow the move to Wednesdays: %s is missing", want)
		}
	}
	// The old Tuesdays before the split survive; the ones after it must not.
	if !got["2026-04-07"] || !got["2026-04-14"] {
		t.Error("occurrences before the split were lost")
	}
	if got["2026-04-28"] {
		t.Error("the old Tuesday series is still producing occurrences after the split")
	}
}

// The monthly equivalent: the rent moves from the 15th to the 20th.
func TestSplitFollowsTheDayOfMonth(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	day := 15
	series := f.timed(t, "Loyer", "2026-01-15", 9, 0, &domain.Recurrence{
		Freq: domain.FreqMonthly, Interval: 1, ByMonthday: &day,
	})

	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-05-15"), Input{
		Title: "Loyer", StartsAt: f.at("2026-05-20", 9, 0), EndsAt: f.at("2026-05-20", 10, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}

	occ := f.occurrences(t, "2026-05-01", "2026-07-31")
	got := map[string]bool{}
	for _, o := range occ {
		got[o.OccurrenceDate.String()] = true
	}
	for _, want := range []string{"2026-05-20", "2026-06-20", "2026-07-20"} {
		if !got[want] {
			t.Errorf("%s is missing: the rent did not follow the move to the 20th", want)
		}
	}
	if got["2026-06-15"] {
		t.Error("the series is still producing occurrences on the 15th after the move")
	}
}

// A rejected split used to leave the original series truncated: the user saw an
// error and lost every remaining occurrence.
func TestRejectedSplitLeavesTheSeriesIntact(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	until := domain.MustParseDate("2026-04-28")
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday}, Until: &until,
	})
	before := len(f.occurrences(t, "2026-04-01", "2026-05-31"))
	if before != 4 {
		t.Fatalf("expected four Tuesdays in April, got %d", before)
	}

	// Move an occurrence past the end of the series. Whether this is accepted or
	// refused, what must not happen is losing the occurrences before it.
	_, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-04-21"), Input{
		Title: "Piscine", StartsAt: f.at("2026-05-05", 17, 30), EndsAt: f.at("2026-05-05", 18, 30),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	})

	after := f.occurrences(t, "2026-04-01", "2026-05-31")
	if err != nil {
		// Refused: the calendar must be exactly as it was.
		if len(after) != before {
			t.Errorf("a rejected split destroyed occurrences: %d before, %d after", before, len(after))
		}
		return
	}
	// Accepted: the two occurrences before the split survive, and the moved one exists.
	got := map[string]bool{}
	for _, o := range after {
		got[o.OccurrenceDate.String()] = true
	}
	for _, want := range []string{"2026-04-07", "2026-04-14", "2026-05-05"} {
		if !got[want] {
			t.Errorf("%s is missing after the split", want)
		}
	}
}

// When the pattern moves, an occurrence someone had already edited used to become a
// row nothing could reach — invisible to the series and hidden from the plain-event
// query. It should stay visible.
func TestSplitKeepsAnEditedOccurrenceReachable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	// Someone adds a note to the lesson of 21 April.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, domain.MustParseDate("2026-04-21"), Input{
		Title: "Piscine + goûter", StartsAt: f.at("2026-04-21", 19, 0), EndsAt: f.at("2026-04-21", 20, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("override: %v", err)
	}
	// Then swimming moves to Wednesdays from 14 April, so 21 April is no longer a
	// date the pattern produces.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-04-14"), Input{
		Title: "Piscine", StartsAt: f.at("2026-04-15", 17, 30), EndsAt: f.at("2026-04-15", 18, 30),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}

	for _, o := range f.occurrences(t, "2026-04-01", "2026-04-30") {
		if o.Title == "Piscine + goûter" {
			return // still visible, which is all that is required
		}
	}
	t.Error("the edited occurrence vanished from the calendar when the pattern moved")
}

// An occurrence a re-patterned split leaves behind stops being an occurrence of its
// series, so it stops inheriting the series' reminders — and an ordinary event with no
// reminders of its own is announced by nothing at all. It takes what it was inheriting
// with it, per member, or the family simply stops hearing about the one lesson the
// split stranded, which is the failure mode this application exists to prevent.
func TestASplitLeavesAStrandedOccurrenceItsReminders(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	thirty := 30
	if err := f.st.ReplaceReminders(ctx, nil, series.RecurrenceID, f.maman,
		[]domain.Reminder{{OffsetMinutes: &thirty}}); err != nil {
		t.Fatalf("reminders for Maman: %v", err)
	}
	// Papa wants nothing for the lesson of 21 April specifically, and says so.
	stranded, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, domain.MustParseDate("2026-04-21"), Input{
		Title: "Piscine + goûter", StartsAt: f.at("2026-04-21", 19, 0), EndsAt: f.at("2026-04-21", 20, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if err := f.st.ReplaceReminders(ctx, &stranded.ID, nil, f.papa, nil); err != nil {
		t.Fatalf("clear Papa's reminders on the occurrence: %v", err)
	}

	// Swimming moves to Wednesdays from 14 April, so 21 April is no longer a date the
	// pattern produces and the copy is detached from the series.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-04-14"), Input{
		Title: "Piscine", StartsAt: f.at("2026-04-15", 17, 30), EndsAt: f.at("2026-04-15", 18, 30),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}

	mamans, err := f.st.ListReminders(ctx, &stranded.ID, nil, f.maman)
	if err != nil {
		t.Fatalf("list Maman's reminders on the stranded occurrence: %v", err)
	}
	if len(mamans) != 1 || mamans[0].OffsetMinutes == nil || *mamans[0].OffsetMinutes != 30 {
		t.Errorf("Maman's reminders on the stranded occurrence = %+v, want the half hour she was"+
			" inheriting: it has no series left to inherit from", mamans)
	}
	if papas, err := f.st.ListReminders(ctx, &stranded.ID, nil, f.papa); err != nil {
		t.Fatalf("list Papa's reminders on the stranded occurrence: %v", err)
	} else if len(papas) != 0 {
		t.Errorf("Papa has %d reminders on the occurrence he had silenced; a split must not"+
			" undo that", len(papas))
	}
}

// Cancelling a date that is not an occurrence used to write a junk exception that
// would spring to life if the pattern ever changed to include that date.
func TestCancellingANonOccurrenceIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	// 8 April 2026 is a Wednesday.
	if err := f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeThis, domain.MustParseDate("2026-04-08")); err == nil {
		t.Error("cancelling a Wednesday on a Tuesday series was accepted")
	}
}

// "This and following" at the *first* occurrence takes a different branch from every
// test above: splitting there would leave an empty first half, so it is an edit of the
// whole series instead. That shortcut used to skip the re-anchoring the split does, so
// the pattern stayed on Tuesdays while DTStart moved to a Wednesday — and since a series
// is only ever read through its rule, the moved occurrence did not exist and neither did
// the one it came from. The edit answered 200 with both gone.
func TestFollowingFromTheFirstOccurrenceMovesTheWholePattern(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})

	// The very first lesson moves to Wednesday the 8th. As everywhere else in this
	// scope, the client sends no recurrence: the server owns the split.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-04-07"), Input{
		Title: "Piscine", StartsAt: f.at("2026-04-08", 18, 0), EndsAt: f.at("2026-04-08", 19, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("move the first occurrence: %v", err)
	}

	got := map[string]bool{}
	for _, o := range f.occurrences(t, "2026-04-01", "2026-05-10") {
		got[o.OccurrenceDate.String()] = true
	}
	if !got["2026-04-08"] {
		t.Error("the occurrence the user moved to Wednesday 8 April does not exist")
	}
	for _, want := range []string{"2026-04-15", "2026-04-22"} {
		if !got[want] {
			t.Errorf("the series did not follow the move to Wednesdays: %s is missing", want)
		}
	}
	if got["2026-04-14"] || got["2026-04-21"] {
		t.Error("the series is still producing Tuesdays after its first occurrence moved to a Wednesday")
	}
}

// The monthly equivalent, which fails the same way: the first rent payment moves from
// the 15th to the 20th and the rule has to move with it.
func TestFollowingFromTheFirstOccurrenceMovesTheDayOfMonth(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	day := 15
	series := f.timed(t, "Loyer", "2026-01-15", 9, 0, &domain.Recurrence{
		Freq: domain.FreqMonthly, Interval: 1, ByMonthday: &day,
	})

	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-01-15"), Input{
		Title: "Loyer", StartsAt: f.at("2026-01-20", 9, 0), EndsAt: f.at("2026-01-20", 10, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("move the first occurrence: %v", err)
	}

	got := map[string]bool{}
	for _, o := range f.occurrences(t, "2026-01-01", "2026-03-31") {
		got[o.OccurrenceDate.String()] = true
	}
	for _, want := range []string{"2026-01-20", "2026-02-20", "2026-03-20"} {
		if !got[want] {
			t.Errorf("%s is missing: the rent did not follow the move to the 20th", want)
		}
	}
	if got["2026-02-15"] {
		t.Error("the series is still producing occurrences on the 15th after the move")
	}
}

// A whole-series edit that does carry a pattern must keep the recur package's documented
// freedom for DTStart to sit outside the rule it anchors — a weekly series anchored on a
// Monday with by_weekday of Tuesday starts the day after. Re-anchoring the first-
// occurrence branch must not tighten that into a rejection.
func TestAWholeSeriesEditMayStartBeforeItsFirstOccurrence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	// 6 April 2026 is a Monday; the rule still says Tuesdays.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeAll, domain.Date{}, Input{
		Title: "Piscine", StartsAt: f.at("2026-04-06", 17, 30), EndsAt: f.at("2026-04-06", 18, 30),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
		Recurrence: &domain.Recurrence{
			Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
		},
	}); err != nil {
		t.Fatalf("whole-series edit anchored the day before its first occurrence: %v", err)
	}

	got := map[string]bool{}
	for _, o := range f.occurrences(t, "2026-04-01", "2026-04-30") {
		got[o.OccurrenceDate.String()] = true
	}
	if !got["2026-04-07"] || !got["2026-04-14"] {
		t.Error("the series stopped producing its Tuesdays")
	}
}

// "Delete this and following" from a date past the end of a series used to *extend* it:
// the until date was written as the split minus a day whatever it was, so occurrences
// between the real end and the new one came back from the dead. Deleting must only ever
// bring a series' end forward.
func TestDeletingFollowingPastTheEndCannotResurrectOccurrences(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	until := domain.MustParseDate("2026-04-14")
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday}, Until: &until,
	})
	before := len(f.occurrences(t, "2026-04-01", "2026-05-31"))
	if before != 2 {
		t.Fatalf("expected the two Tuesdays the series runs for, got %d", before)
	}

	// 12 May is well past the series' own end, so there is nothing there to delete.
	if err := f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeUpcoming,
		domain.MustParseDate("2026-05-12")); err != nil {
		t.Fatalf("delete this and following past the end: %v", err)
	}

	got := map[string]bool{}
	for _, o := range f.occurrences(t, "2026-04-01", "2026-05-31") {
		got[o.OccurrenceDate.String()] = true
	}
	for _, gone := range []string{"2026-04-21", "2026-04-28", "2026-05-05"} {
		if got[gone] {
			t.Errorf("%s came back from the dead: deleting extended the series instead of ending it", gone)
		}
	}
	if !got["2026-04-07"] || !got["2026-04-14"] {
		t.Error("the occurrences the series really had were lost")
	}
}

// The same unclamped until on the split path: "edit this and following" at a date past
// the series' end extended the half being closed, so occurrences that never existed
// appeared behind the split.
func TestSplittingPastTheEndCannotResurrectOccurrences(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	until := domain.MustParseDate("2026-04-14")
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday}, Until: &until,
	})

	// Split at 12 May, past the end, moving the (non-existent) occurrence to the 13th.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-05-12"), Input{
		Title: "Piscine", StartsAt: f.at("2026-05-13", 18, 0), EndsAt: f.at("2026-05-13", 19, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("split past the end: %v", err)
	}

	got := map[string]bool{}
	for _, o := range f.occurrences(t, "2026-04-01", "2026-05-31") {
		got[o.OccurrenceDate.String()] = true
	}
	for _, gone := range []string{"2026-04-21", "2026-04-28", "2026-05-05"} {
		if got[gone] {
			t.Errorf("%s came back from the dead: the closed half of the split was extended", gone)
		}
	}
	if !got["2026-04-07"] || !got["2026-04-14"] {
		t.Error("the occurrences the series really had were lost")
	}
}
