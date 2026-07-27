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
