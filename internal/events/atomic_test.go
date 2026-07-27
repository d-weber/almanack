package events

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"
)

// A scoped edit is several writes, and every one of them runs on the request's context.
// A phone that drops its connection half-way through therefore genuinely stops the
// sequence — this is not only a crash window — and until those writes shared a
// transaction the calendar was left in a state nobody asked for: "this and following"
// capped the old series and never created its replacement, and any flow could commit
// its edit and then lose the activity row the change notification is planned from.
//
// The tests below interrupt an edit at each point it can be interrupted and assert the
// only two outcomes a family can make sense of: the edit happened, or it did not.

// interruptClock cancels a context on the nth read of the clock, which is how these
// tests stand in for a dropped connection without needing one. Every write path reads
// the clock — the service once per edit, the store once for each row it stamps — so
// counting reads walks the interruption through the sequence a step at a time.
//
// It is deliberately not safe for concurrent use: the tests that arm it drive one edit
// from one goroutine, and a mutex here would only hide it if that ever stopped being
// true.
type interruptClock struct {
	fake   *clock.Fake
	at     int
	reads  int
	cancel context.CancelFunc
}

func (c *interruptClock) Now() time.Time {
	c.reads++
	if c.reads == c.at {
		c.cancel()
	}
	return c.fake.Now()
}

// interruptAt arms the clock so that the nth read from now on cancels ctx. The reads a
// test makes while setting itself up do not count.
func (c *interruptClock) interruptAt(n int, cancel context.CancelFunc) {
	c.reads, c.at, c.cancel = 0, n, cancel
}

func newInterruptibleFixture(t *testing.T) (*fixture, *interruptClock) {
	t.Helper()
	var ic *interruptClock
	f := newFixtureClock(t, func(fake *clock.Fake) clock.Clock {
		ic = &interruptClock{fake: fake}
		return ic
	})
	return f, ic
}

// interruptionPoints is how far the sweep goes. The longest flow here reads the clock
// four times; the runs past the end are the control, where nothing is interrupted and
// the edit has to land in full.
const interruptionPoints = 6

// dates renders occurrences as "YYYY-MM-DD title", which is the calendar as the family
// reads it and the shape a half-finished edit shows up in.
func dates(occ []domain.Occurrence) []string {
	out := make([]string, 0, len(occ))
	for _, o := range occ {
		out = append(out, o.OccurrenceDate.String()+" "+o.Title)
	}
	slices.Sort(out)
	return out
}

func (f *fixture) state(t *testing.T) []string {
	t.Helper()
	return dates(f.occurrences(t, "2026-04-01", "2026-05-31"))
}

func (f *fixture) activityCount(t *testing.T) int {
	t.Helper()
	rows, err := f.st.ListActivity(context.Background(), []int64{f.cal}, 100, 0)
	if err != nil {
		t.Fatalf("list activity: %v", err)
	}
	return len(rows)
}

func (f *fixture) reminderCount(t *testing.T) int {
	t.Helper()
	rs, err := f.st.ListAllReminders(context.Background())
	if err != nil {
		t.Fatalf("list reminders: %v", err)
	}
	return len(rs)
}

// TestAnInterruptedSplitLeavesTheSeriesAsItWas is the regression for the one flow that
// lost data rather than leaving junk behind. "This and following" ends the old series
// the day before the split and then creates its replacement; interrupted between the
// two, it used to leave the family with half a series and no error they could act on —
// the swimming lessons after 21 April simply stopped existing.
func TestAnInterruptedSplitLeavesTheSeriesAsItWas(t *testing.T) {
	for n := 1; n <= interruptionPoints; n++ {
		t.Run(fmt.Sprintf("interrupted at clock read %d", n), func(t *testing.T) {
			f, ic := newInterruptibleFixture(t)
			series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
				Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
			})
			// A reminder on the series, because the split has to copy it across and a
			// half of a series that has quietly stopped reminding anyone is the
			// symptom nobody notices until the lesson is missed.
			offset := 30
			if err := f.st.ReplaceReminders(context.Background(), nil, series.RecurrenceID, f.maman,
				[]domain.Reminder{{OffsetMinutes: &offset}}); err != nil {
				t.Fatalf("add reminder: %v", err)
			}

			intact := f.state(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ic.interruptAt(n, cancel)

			// From 21 April swimming moves to Wednesday the 22nd.
			_, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, domain.MustParseDate("2026-04-21"), Input{
				Title: "Piscine", StartsAt: f.at("2026-04-22", 18, 0), EndsAt: f.at("2026-04-22", 19, 0),
				LabelID: f.labels[0].ID, Participants: []int64{f.maman},
			})

			got := f.state(t)
			reminders := f.reminderCount(t)
			if err != nil {
				// Interrupted: the calendar must be exactly as it was.
				if !slices.Equal(got, intact) {
					t.Errorf("an interrupted split changed the calendar.\n got: %v\nwant: %v", got, intact)
				}
				if reminders != 1 {
					t.Errorf("an interrupted split left %d reminders, want the original 1", reminders)
				}
				return
			}
			// Not interrupted: the split must have landed whole.
			split := []string{
				"2026-04-07 Piscine", "2026-04-14 Piscine",
				"2026-04-22 Piscine", "2026-04-29 Piscine",
				"2026-05-06 Piscine", "2026-05-13 Piscine",
				"2026-05-20 Piscine", "2026-05-27 Piscine",
			}
			if !slices.Equal(got, split) {
				t.Errorf("the split reported success but the calendar is half-done.\n got: %v\nwant: %v", got, split)
			}
			if reminders != 2 {
				t.Errorf("the split reported success with %d reminders, want one on each half", reminders)
			}
		})
	}
}

// TestAnInterruptedEditNeverLosesItsActivityRow pins what internal/notify's planActivity
// already claims: "the edit and its log row land in the same transaction, so a crash
// between the edit and the notification loses nothing". They did not. logActivity was a
// separate statement after every edit, so an interruption between the two committed the
// change and silently dropped the notification of it — precisely the failure the comment
// says is impossible.
//
// The assertion is the invariant rather than a sequence: the calendar changed if and
// only if something was written to the log.
func TestAnInterruptedEditNeverLosesItsActivityRow(t *testing.T) {
	the21st := domain.MustParseDate("2026-04-21")

	cases := []struct {
		name string
		edit func(ctx context.Context, f *fixture, series domain.Event) error
	}{
		{"editing one occurrence", func(ctx context.Context, f *fixture, series domain.Event) error {
			_, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, the21st, Input{
				Title: "Piscine + goûter", StartsAt: f.at("2026-04-21", 19, 0), EndsAt: f.at("2026-04-21", 20, 0),
				LabelID: f.labels[0].ID, Participants: []int64{f.maman},
			})
			return err
		}},
		{"this and following", func(ctx context.Context, f *fixture, series domain.Event) error {
			_, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeUpcoming, the21st, Input{
				Title: "Piscine", StartsAt: f.at("2026-04-22", 18, 0), EndsAt: f.at("2026-04-22", 19, 0),
				LabelID: f.labels[0].ID, Participants: []int64{f.maman},
			})
			return err
		}},
		{"cancelling one occurrence", func(ctx context.Context, f *fixture, series domain.Event) error {
			return f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeThis, the21st)
		}},
		{"deleting this and following", func(ctx context.Context, f *fixture, series domain.Event) error {
			return f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeUpcoming, the21st)
		}},
		{"deleting the whole series", func(ctx context.Context, f *fixture, series domain.Event) error {
			return f.svc.Delete(ctx, f.maman, series.ID, domain.ScopeAll, domain.Date{})
		}},
	}

	for _, tc := range cases {
		for n := 1; n <= interruptionPoints; n++ {
			t.Run(fmt.Sprintf("%s, interrupted at clock read %d", tc.name, n), func(t *testing.T) {
				f, ic := newInterruptibleFixture(t)
				series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
					Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
				})
				// An occurrence someone had already edited, so that the flows which
				// have to clean up after one are exercised too.
				if _, err := f.svc.Update(context.Background(), f.maman, series.ID, domain.ScopeThis,
					domain.MustParseDate("2026-04-28"), Input{
						Title: "Piscine (retard)", StartsAt: f.at("2026-04-28", 18, 30), EndsAt: f.at("2026-04-28", 19, 30),
						LabelID: f.labels[0].ID, Participants: []int64{f.maman},
					}); err != nil {
					t.Fatalf("edit an occurrence: %v", err)
				}

				before, logged := f.state(t), f.activityCount(t)

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ic.interruptAt(n, cancel)
				_ = tc.edit(ctx, f, series)

				changed := !slices.Equal(before, f.state(t))
				recorded := f.activityCount(t) > logged
				switch {
				case changed && !recorded:
					t.Errorf("the calendar changed and nothing was written to the activity log, "+
						"so no one is told about it.\nbefore: %v\n after: %v", before, f.state(t))
				case recorded && !changed:
					t.Error("an activity row survived an edit that did not happen, so the family " +
						"is told about a change that was rolled back")
				}
			})
		}
	}
}
