package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"
	"almanack/internal/store"

	_ "modernc.org/sqlite"
)

// paris is the family timezone in every test here. Recurrence is expanded in local
// wall-clock and stored in UTC, so the DST cases below are the ones that matter.
func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}
	return loc
}

type fixture struct {
	svc    *Service
	st     *store.Store
	loc    *time.Location
	clk    *clock.Fake
	maman  int64
	papa   int64
	cal    int64
	other  int64 // a calendar maman is not a member of
	labels []domain.Label
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureClock(t, nil)
}

// newFixtureClock builds a fixture whose store and service read time through wrap(fake)
// rather than through the fake itself. Everything that writes reads the clock, which is
// what lets the interruption tests in atomic_test.go fail an edit part-way through
// without a seam of their own.
func newFixtureClock(t *testing.T, wrap func(*clock.Fake) clock.Clock) *fixture {
	t.Helper()
	loc := paris(t)
	fake := clock.NewFake(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	var clk clock.Clock = fake
	if wrap != nil {
		clk = wrap(fake)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), loc, clk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	maman, err := st.CreateUser(ctx, domain.User{Email: "maman@example.org", DisplayName: "Maman", Color: "#c0392b", Lang: domain.LangFR, TimeFormat: "24h"}, "x")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	papa, err := st.CreateUser(ctx, domain.User{Email: "papa@example.org", DisplayName: "Papa", Color: "#2980b9", Lang: domain.LangFR, TimeFormat: "24h"}, "x")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cal, err := st.CreateCalendar(ctx, domain.Calendar{Name: "Famille", Color: "#3b7ddd", CreatorID: maman.ID})
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}
	if err := st.AddMember(ctx, cal.ID, papa.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	other, err := st.CreateCalendar(ctx, domain.Calendar{Name: "Ailleurs", Color: "#888888", CreatorID: papa.ID})
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}
	labels, err := st.ListLabels(ctx, cal.ID)
	if err != nil {
		t.Fatalf("labels: %v", err)
	}

	return &fixture{
		svc: New(st, loc, clk), st: st, loc: loc, clk: fake,
		maman: maman.ID, papa: papa.ID, cal: cal.ID, other: other.ID, labels: labels,
	}
}

func (f *fixture) at(d string, hour, min int) time.Time {
	return domain.MustParseDate(d).At(hour, min, f.loc).UTC()
}

func (f *fixture) timed(t *testing.T, title, day string, hour, min int, rec *domain.Recurrence, participants ...int64) domain.Event {
	t.Helper()
	if participants == nil {
		participants = []int64{f.maman}
	}
	e, err := f.svc.Create(context.Background(), f.maman, Input{
		CalendarID: f.cal, Title: title,
		StartsAt: f.at(day, hour, min), EndsAt: f.at(day, hour+1, min),
		LabelID: f.labels[0].ID, Participants: participants, Recurrence: rec,
	})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return e
}

// allDay creates an all-day event covering [start, end] inclusive, so that a test can
// have an occurrence that is longer than the day it starts on.
func (f *fixture) allDay(t *testing.T, title, start, end string, rec *domain.Recurrence, participants ...int64) domain.Event {
	t.Helper()
	if participants == nil {
		participants = []int64{f.maman}
	}
	e, err := f.svc.Create(context.Background(), f.maman, Input{
		CalendarID: f.cal, Title: title, AllDay: true,
		StartDate: domain.MustParseDate(start), EndDate: domain.MustParseDate(end),
		LabelID: f.labels[0].ID, Participants: participants, Recurrence: rec,
	})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return e
}

func (f *fixture) occurrences(t *testing.T, from, to string) []domain.Occurrence {
	t.Helper()
	occ, err := f.svc.Occurrences(context.Background(), []int64{f.cal}, domain.MustParseDate(from), domain.MustParseDate(to))
	if err != nil {
		t.Fatalf("occurrences %s..%s: %v", from, to, err)
	}
	return occ
}

func titlesOn(occ []domain.Occurrence, date domain.Date) []string {
	var out []string
	for _, o := range occ {
		if o.OccurrenceDate.Equal(date) {
			out = append(out, o.Title)
		}
	}
	return out
}

// TestWeeklySeriesKeepsWallClockAcrossDST is the single most important test in this
// package. Europe/Paris moves to summer time on 29 March 2026; a 16:30 event must
// still be at 16:30 for the family afterwards, which means its UTC instant shifts.
// An implementation that adds seven days to the stored UTC instant passes every other
// test in this file and fails this one.
func TestWeeklySeriesKeepsWallClockAcrossDST(t *testing.T) {
	f := newFixture(t)
	f.timed(t, "Piscine", "2026-03-24", 16, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})

	occ := f.occurrences(t, "2026-03-01", "2026-04-30")

	want := map[string]string{
		"2026-03-24": "15:30", // CET, UTC+1
		"2026-03-31": "14:30", // CEST, UTC+2 — the same 16:30 locally
		"2026-04-07": "14:30",
	}
	seen := map[string]string{}
	for _, o := range occ {
		seen[o.OccurrenceDate.String()] = o.StartsAt.UTC().Format("15:04")
	}
	for date, wantUTC := range want {
		if seen[date] != wantUTC {
			t.Errorf("%s starts at %sZ, want %sZ", date, seen[date], wantUTC)
		}
		// And the local reading, which is what the family actually sees.
		for _, o := range occ {
			if o.OccurrenceDate.String() == date {
				if got := o.StartsAt.In(f.loc).Format("15:04"); got != "16:30" {
					t.Errorf("%s displays as %s locally, want 16:30", date, got)
				}
			}
		}
	}
}

func TestOverrideMovesOneOccurrenceOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})

	moved := domain.MustParseDate("2026-04-14")
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, moved, Input{
		Title: "Piscine (exceptionnel)", StartsAt: f.at("2026-04-14", 19, 0), EndsAt: f.at("2026-04-14", 20, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("override: %v", err)
	}

	occ := f.occurrences(t, "2026-04-01", "2026-04-30")
	for _, o := range occ {
		local := o.StartsAt.In(f.loc).Format("15:04")
		switch o.OccurrenceDate.String() {
		case "2026-04-14":
			if o.Title != "Piscine (exceptionnel)" || local != "19:00" {
				t.Errorf("overridden occurrence is %q at %s, want the edited values", o.Title, local)
			}
			if !o.IsOverride {
				t.Error("overridden occurrence is not flagged as one")
			}
		default:
			if o.Title != "Piscine" || local != "17:30" {
				t.Errorf("%s was changed to %q at %s; editing one occurrence must not touch the others", o.OccurrenceDate, o.Title, local)
			}
		}
	}
	if len(occ) != 4 {
		t.Errorf("got %d occurrences in April, want 4", len(occ))
	}
}

func TestCancelledOccurrenceDisappearsAndSeriesSurvives(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	if err := f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeThis, domain.MustParseDate("2026-04-14")); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	occ := f.occurrences(t, "2026-04-01", "2026-04-30")
	if got := titlesOn(occ, domain.MustParseDate("2026-04-14")); len(got) != 0 {
		t.Errorf("cancelled occurrence still present: %v", got)
	}
	if len(occ) != 3 {
		t.Errorf("got %d occurrences, want 3 (April has four Tuesdays, one cancelled)", len(occ))
	}
}

// TestOverrideMovedIntoWindowFromOutside guards the subtle case: the original
// occurrence lies outside the queried window, but the edit moved it inside. Expanding
// the recurrence alone would never find it.
func TestOverrideMovedIntoWindowFromOutside(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})

	// The 5 May occurrence is moved back to 28 April.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, domain.MustParseDate("2026-05-05"), Input{
		Title: "Piscine (avancée)", StartsAt: f.at("2026-04-28", 17, 30), EndsAt: f.at("2026-04-28", 18, 30),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("override: %v", err)
	}

	// Query only April. The moved occurrence must appear even though its identity
	// date (5 May) is outside the window.
	occ := f.occurrences(t, "2026-04-01", "2026-04-30")
	found := false
	for _, o := range occ {
		if o.Title == "Piscine (avancée)" {
			found = true
		}
	}
	if !found {
		t.Error("an occurrence moved into the window from outside it was not returned")
	}
}

// TestOccurrenceMovedPastTheEndOfItsSeriesStaysVisible is the last lesson of term
// dragged into the following month. The series ended in June, so a window covering
// July can only reach that occurrence through a series whose own dates say it has
// nothing left to give.
func TestOccurrenceMovedPastTheEndOfItsSeriesStaysVisible(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	endOfTerm := domain.MustParseDate("2026-06-30")
	series := f.timed(t, "Piscine", "2026-06-02", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
		Until: &endOfTerm,
	})

	// The 30 June lesson is moved to 7 July, past the end of the series.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, endOfTerm, Input{
		Title: "Piscine (rattrapage)", StartsAt: f.at("2026-07-07", 17, 30), EndsAt: f.at("2026-07-07", 18, 30),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("move the last occurrence: %v", err)
	}

	occ := f.occurrences(t, "2026-07-01", "2026-07-31")
	if got := titlesOn(occ, endOfTerm); len(got) != 1 || got[0] != "Piscine (rattrapage)" {
		t.Fatalf("July shows %v; the occurrence moved past the end of its series is missing", got)
	}
	if got := occ[0].StartsAt.In(f.loc).Format("2006-01-02 15:04"); got != "2026-07-07 17:30" {
		t.Errorf("the moved occurrence lands at %s, want 2026-07-07 17:30", got)
	}
	// It must not also show up on its original date in the June window.
	if got := titlesOn(f.occurrences(t, "2026-06-01", "2026-06-30"), endOfTerm); len(got) != 0 {
		t.Errorf("30 June still shows %v; the occurrence moved away from it", got)
	}
}

// TestOccurrenceMovedBeforeTheStartOfItsSeriesStaysVisible is the same hole at the
// other end: the first occurrence of a series that has not begun yet, pulled back into
// the month you are looking at.
func TestOccurrenceMovedBeforeTheStartOfItsSeriesStaysVisible(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-08-04", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})

	// The first lesson, 4 August, is brought forward to 28 July — before dtstart.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, domain.MustParseDate("2026-08-04"), Input{
		Title: "Piscine (avancée)", StartsAt: f.at("2026-07-28", 17, 30), EndsAt: f.at("2026-07-28", 18, 30),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("move the first occurrence: %v", err)
	}

	occ := f.occurrences(t, "2026-07-01", "2026-07-31")
	if got := titlesOn(occ, domain.MustParseDate("2026-08-04")); len(got) != 1 || got[0] != "Piscine (avancée)" {
		t.Fatalf("July shows %v; the occurrence moved before the start of its series is missing", got)
	}
}

// TestMultiDayOccurrenceMovedPastTheEndOfItsSeriesStaysVisible is the multi-day version
// of the same move: an all-day trip that no longer starts anywhere near the window it
// now covers.
func TestMultiDayOccurrenceMovedPastTheEndOfItsSeriesStaysVisible(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	lastTrip := domain.MustParseDate("2026-06-29")
	series := f.allDay(t, "Papa à Lyon", "2026-06-01", "2026-06-03", &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Monday},
		Until: &lastTrip,
	})

	// The last trip, 29 June to 1 July, is pushed a week later: 6 to 8 July.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, lastTrip, Input{
		Title: "Papa à Lyon (reporté)", AllDay: true,
		StartDate: domain.MustParseDate("2026-07-06"), EndDate: domain.MustParseDate("2026-07-08"),
		LabelID: f.labels[0].ID, Participants: []int64{f.papa},
	}); err != nil {
		t.Fatalf("move the last trip: %v", err)
	}

	occ := f.occurrences(t, "2026-07-01", "2026-07-31")
	if got := titlesOn(occ, lastTrip); len(got) != 1 || got[0] != "Papa à Lyon (reporté)" {
		t.Fatalf("July shows %v; the multi-day occurrence moved past the end of its series is missing", got)
	}
	if got := occ[0].EndDate.String(); got != "2026-07-08" {
		t.Errorf("the moved trip ends on %s, want 2026-07-08", got)
	}
}

// TestMultiDayOccurrenceReachingIntoTheWindowStaysVisible has no override at all. The
// series ends on 29 June, but that last three-day trip runs into 1 July, and a window
// starting on 1 July still has to show it — exactly as it would for a one-off event
// with the same dates.
func TestMultiDayOccurrenceReachingIntoTheWindowStaysVisible(t *testing.T) {
	f := newFixture(t)
	lastTrip := domain.MustParseDate("2026-06-29")
	f.allDay(t, "Papa à Lyon", "2026-06-01", "2026-06-03", &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Monday},
		Until: &lastTrip,
	})

	occ := f.occurrences(t, "2026-07-01", "2026-07-31")
	if got := titlesOn(occ, lastTrip); len(got) != 1 || got[0] != "Papa à Lyon" {
		t.Fatalf("July shows %v; the tail of the last trip is missing", got)
	}
}

func TestSplitSeriesAtUpcoming(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})

	// From 21 April on, swimming moves to Wednesdays at 18:00.
	split := domain.MustParseDate("2026-04-21")
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, split, Input{
		Title: "Piscine", StartsAt: f.at("2026-04-22", 18, 0), EndsAt: f.at("2026-04-22", 19, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
		Recurrence: &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Wednesday}},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}

	occ := f.occurrences(t, "2026-04-01", "2026-05-10")
	var dates []string
	for _, o := range occ {
		dates = append(dates, o.OccurrenceDate.String()+" "+o.StartsAt.In(f.loc).Format("15:04"))
	}

	// Tuesdays up to the split, Wednesdays after it, and nothing on 21 April.
	wantBefore := map[string]bool{"2026-04-07 17:30": true, "2026-04-14 17:30": true}
	wantAfter := map[string]bool{"2026-04-22 18:00": true, "2026-04-29 18:00": true, "2026-05-06 18:00": true}
	got := map[string]bool{}
	for _, d := range dates {
		got[d] = true
	}
	for want := range wantBefore {
		if !got[want] {
			t.Errorf("missing pre-split occurrence %s (got %v)", want, dates)
		}
	}
	for want := range wantAfter {
		if !got[want] {
			t.Errorf("missing post-split occurrence %s (got %v)", want, dates)
		}
	}
	if got["2026-04-21 17:30"] {
		t.Error("the old series still produces an occurrence on the split date")
	}
}

// TestSplitDoesNotResurrectCancelledOccurrences is the bookkeeping trap in a series
// split: overrides dated at or after the split must move to the new series, or a
// cancellation made before the split silently comes back to life under the new one.
func TestSplitDoesNotResurrectCancelledOccurrences(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-04-01", 17, 30, &domain.Recurrence{
		Freq: domain.FreqDaily, Interval: 1,
	})

	cancelled := domain.MustParseDate("2026-04-10")
	if err := f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeThis, cancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Now split before the cancelled day.
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-04-05"), Input{
		Title: "Piscine", StartsAt: f.at("2026-04-05", 18, 0), EndsAt: f.at("2026-04-05", 19, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
		Recurrence: &domain.Recurrence{Freq: domain.FreqDaily, Interval: 1},
	}); err != nil {
		t.Fatalf("split: %v", err)
	}

	occ := f.occurrences(t, "2026-04-01", "2026-04-15")
	if got := titlesOn(occ, cancelled); len(got) != 0 {
		t.Errorf("the cancellation was lost in the split: %s came back as %v", cancelled, got)
	}
}

func TestEditAllLeavesDeliberateOverridesAlone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	overridden := domain.MustParseDate("2026-04-14")
	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, overridden, Input{
		Title: "Piscine (exceptionnel)", StartsAt: f.at("2026-04-14", 19, 0), EndsAt: f.at("2026-04-14", 20, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("override: %v", err)
	}

	if _, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeAll, domain.Date{}, Input{
		Title: "Natation", StartsAt: f.at("2026-04-07", 17, 30), EndsAt: f.at("2026-04-07", 18, 30),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
		Recurrence: &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday}},
	}); err != nil {
		t.Fatalf("edit all: %v", err)
	}

	occ := f.occurrences(t, "2026-04-01", "2026-04-30")
	for _, o := range occ {
		if o.OccurrenceDate.Equal(overridden) {
			if o.Title != "Piscine (exceptionnel)" {
				t.Errorf("the deliberately edited occurrence was overwritten by the series edit: %q", o.Title)
			}
			continue
		}
		if o.Title != "Natation" {
			t.Errorf("%s is %q, want the new series title", o.OccurrenceDate, o.Title)
		}
	}
}

func TestDeleteAllRemovesSeriesAndItsOverrideEvents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	moved, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, domain.MustParseDate("2026-04-14"), Input{
		Title: "Piscine (exceptionnel)", StartsAt: f.at("2026-04-14", 19, 0), EndsAt: f.at("2026-04-14", 20, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	})
	if err != nil {
		t.Fatalf("override: %v", err)
	}

	if err := f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeAll, domain.Date{}); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	if occ := f.occurrences(t, "2026-04-01", "2026-05-31"); len(occ) != 0 {
		t.Errorf("%d occurrences survived deleting the series", len(occ))
	}
	// The override's standalone event would otherwise be orphaned: nothing else
	// references it, so nothing else would ever clean it up.
	if _, err := f.st.EventByID(ctx, moved.ID); err == nil {
		t.Error("the override event outlived its series")
	}
}

func TestDeleteUpcomingEndsTheSeries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	if err := f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-04-21")); err != nil {
		t.Fatalf("delete upcoming: %v", err)
	}

	occ := f.occurrences(t, "2026-04-01", "2026-06-30")
	if len(occ) != 2 {
		t.Errorf("got %d occurrences, want the two before the cut", len(occ))
	}
	for _, o := range occ {
		if !o.OccurrenceDate.Before(domain.MustParseDate("2026-04-21")) {
			t.Errorf("%s survived a delete-upcoming from 2026-04-21", o.OccurrenceDate)
		}
	}
}

func TestAllDayMultiDayEventSpansTheWindowEdges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, f.maman, Input{
		CalendarID: f.cal, Title: "Vacances", AllDay: true,
		StartDate: domain.MustParseDate("2026-04-28"), EndDate: domain.MustParseDate("2026-05-04"),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Windows that touch only the start, only the end, or sit entirely inside.
	for _, w := range []struct{ from, to string }{
		{"2026-04-01", "2026-04-30"},
		{"2026-05-01", "2026-05-31"},
		{"2026-04-30", "2026-05-01"},
	} {
		if occ := f.occurrences(t, w.from, w.to); len(occ) != 1 {
			t.Errorf("window %s..%s returned %d occurrences, want the holiday", w.from, w.to, len(occ))
		}
	}
	if occ := f.occurrences(t, "2026-05-05", "2026-05-31"); len(occ) != 0 {
		t.Errorf("the holiday leaked into a window after it ended")
	}
}

// TestEventEndingAtMidnightBelongsToTheDayItStarted stops a 23:00–00:00 event from
// appearing on the following day as well.
func TestEventEndingAtMidnightBelongsToTheDayItStarted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, f.maman, Input{
		CalendarID: f.cal, Title: "Soirée",
		StartsAt: f.at("2026-04-10", 23, 0), EndsAt: f.at("2026-04-11", 0, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if occ := f.occurrences(t, "2026-04-11", "2026-04-11"); len(occ) != 0 {
		t.Errorf("an event ending at midnight showed up on the next day")
	}
	if occ := f.occurrences(t, "2026-04-10", "2026-04-10"); len(occ) != 1 {
		t.Errorf("the event is missing from its own day")
	}
}

func TestUserOccurrencesRespectsMuteAndParticipation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.timed(t, "Concerne Papa", "2026-04-10", 10, 0, nil, f.papa)
	f.timed(t, "Concerne Maman", "2026-04-10", 11, 0, nil, f.maman)

	from, to := domain.MustParseDate("2026-04-01"), domain.MustParseDate("2026-04-30")

	all, err := f.svc.UserOccurrences(ctx, f.maman, from, to)
	if err != nil {
		t.Fatalf("user occurrences: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d occurrences, want both", len(all))
	}

	// "Only my events" hides the one she is not part of.
	m, err := f.st.Membership(ctx, f.cal, f.maman)
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	m.ParticipatingOnly = true
	if err := f.st.UpdateMembership(ctx, m); err != nil {
		t.Fatalf("update membership: %v", err)
	}
	filtered, err := f.svc.UserOccurrences(ctx, f.maman, from, to)
	if err != nil {
		t.Fatalf("user occurrences: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Title != "Concerne Maman" {
		t.Errorf("participating-only returned %d occurrences, want just hers", len(filtered))
	}

	// Muting the calendar hides everything in it.
	m.Muted = true
	if err := f.st.UpdateMembership(ctx, m); err != nil {
		t.Fatalf("update membership: %v", err)
	}
	muted, err := f.svc.UserOccurrences(ctx, f.maman, from, to)
	if err != nil {
		t.Fatalf("user occurrences: %v", err)
	}
	if len(muted) != 0 {
		t.Errorf("a muted calendar still returned %d occurrences", len(muted))
	}
}

func TestNonMembersCannotWrite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.Create(ctx, f.maman, Input{
		CalendarID: f.other, Title: "Intrusion",
		StartsAt: f.at("2026-04-10", 10, 0), EndsAt: f.at("2026-04-10", 11, 0),
		LabelID: f.labels[0].ID,
	})
	if err == nil {
		t.Fatal("a non-member was allowed to create an event")
	}
}

func TestValidationRejectsTheObviousMistakes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	base := Input{
		CalendarID: f.cal, Title: "Test",
		StartsAt: f.at("2026-04-10", 10, 0), EndsAt: f.at("2026-04-10", 11, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}

	cases := map[string]func(Input) Input{
		"no title":            func(in Input) Input { in.Title = "  "; return in },
		"end before start":    func(in Input) Input { in.EndsAt = f.at("2026-04-10", 9, 0); return in },
		"all-day with no end": func(in Input) Input { in.AllDay = true; in.StartDate = domain.MustParseDate("2026-04-10"); return in },
		"label from another calendar": func(in Input) Input {
			ls, err := f.st.ListLabels(ctx, f.other)
			if err != nil || len(ls) == 0 {
				t.Fatalf("labels: %v", err)
			}
			in.LabelID = ls[0].ID
			return in
		},
		"participant who is not a member": func(in Input) Input {
			u, err := f.st.CreateUser(ctx, domain.User{Email: "stranger@example.org", DisplayName: "Stranger", Color: "#000000", Lang: domain.LangFR, TimeFormat: "24h"}, "x")
			if err != nil {
				t.Fatalf("create user: %v", err)
			}
			in.Participants = []int64{u.ID}
			return in
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.svc.Create(ctx, f.maman, mutate(base)); err == nil {
				t.Errorf("accepted an invalid event: %s", name)
			}
		})
	}
}

// TestEditingPrunesQueuedNotifications: a moved event must not still fire a reminder
// at its old time. The planner rebuilds what it needs on the next pass.
func TestEditingPrunesQueuedNotifications(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ev := f.timed(t, "Dentiste", "2026-04-10", 16, 30, nil)

	queued := domain.QueuedNotification{
		UserID:    f.maman,
		Kind:      domain.KindReminder,
		SourceRef: ReminderSourceRef(ev.ID, domain.MustParseDate("2026-04-10"), 1),
		Payload:   `{}`,
		DueAt:     f.at("2026-04-10", 15, 30),
	}
	if err := f.st.EnqueueNotification(ctx, queued); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := f.svc.Update(ctx, f.maman, ev.ID, domain.ScopeAll, domain.Date{}, Input{
		Title: "Dentiste", StartsAt: f.at("2026-04-10", 18, 0), EndsAt: f.at("2026-04-10", 19, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	pending, err := f.st.ListUnsentBefore(ctx, f.at("2026-04-11", 0, 0))
	if err != nil {
		t.Fatalf("list unsent: %v", err)
	}
	for _, p := range pending {
		if p.SourceRef == queued.SourceRef {
			t.Error("a reminder queued for the old time survived the edit")
		}
	}
}

func TestSourceRefPrefixesNest(t *testing.T) {
	d := domain.MustParseDate("2026-08-04")
	ref := ReminderSourceRef(12, d, 7)
	if got := OccurrenceSourcePrefix(12, d); len(ref) <= len(got) || ref[:len(got)] != got {
		t.Errorf("%q is not prefixed by the occurrence prefix %q", ref, got)
	}
	if got := EventSourcePrefix(12); ref[:len(got)] != got {
		t.Errorf("%q is not prefixed by the event prefix %q", ref, got)
	}
	// Event 1's prefix must not match event 12's references.
	if other := EventSourcePrefix(1); len(ref) >= len(other) && ref[:len(other)] == other {
		t.Errorf("prefix %q wrongly matches a reference for event 12", other)
	}
}
