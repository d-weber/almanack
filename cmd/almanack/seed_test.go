package main

import (
	"testing"
	"time"

	"almanack/internal/domain"
)

// The demo is a screen before it is a fixture: `make seed && make dev` has to open on a
// calendar with the interesting cases in it, and the weekly series is the most
// interesting of them — one occurrence moved, a later one cancelled, the rest plain.
// Anchored to the next Tuesday after the seeded day, none of that was reliably on the
// month the app opens on, and on a Tuesday none of it was on it at all: the series
// began seven days out, which a five-row grid can end before. The browser smoke test
// that asserts the series renders therefore went red on whichever days the calendar
// happened to fall badly (#62), and the demo was empty of a series on the same days.
//
// The dates are pure arithmetic over the month, so this asks about every day in a
// decade rather than a few interesting ones — including both February lengths, every
// weekday the seed can be run on, and the months whose first Tuesday is the 7th, which
// is the tightest case.
func TestTheDemoSeriesLandsOnTheMonthTheAppOpensOn(t *testing.T) {
	last := domain.NewDate(2033, time.December, 31)
	for day := domain.NewDate(2024, time.January, 1); !day.After(last); day = day.AddDays(1) {
		start, moved, cancelled := swimmingSeries(day)

		// The second plain occurrence is named as well as the first, because the browser
		// test counts them: one chip on the grid would also be what a series that had
		// stopped repeating after its first occurrence looked like.
		for _, c := range []struct {
			what string
			date domain.Date
		}{
			{"the series starts", start},
			{"the second plain occurrence is", start.AddDays(14)},
			{"the moved occurrence is", moved},
			{"the cancelled occurrence is", cancelled},
		} {
			if c.date.Weekday() != time.Tuesday {
				t.Fatalf("seeded on %s (%s): %s %s, a %s", day, day.Weekday(), c.what, c.date, c.date.Weekday())
			}
			// Every day of the month is on that month's grid and nothing else is
			// guaranteed to be, so this is exactly "visible on the screen it opens on".
			if c.date.Year != day.Year || c.date.Month != day.Month {
				t.Fatalf("seeded on %s: %s %s, outside %s %d", day, c.what, c.date, day.Month, day.Year)
			}
		}
	}
}

// The fortnightly guitar lesson is the same case as the swimming series and was left in
// the same state (#67): anchored to the next Wednesday after the seeded day, which is up
// to seven days out and so sometimes past the end of the grid. A series needs two
// occurrences on the month to be a series at all — "every second Wednesday" is the one
// thing this one is in the seed to show, and one chip shows nothing about an interval —
// so both are asked for here.
func TestTheDemoFortnightlySeriesShowsTwiceOnTheMonthTheAppOpensOn(t *testing.T) {
	last := domain.NewDate(2033, time.December, 31)
	for day := domain.NewDate(2024, time.January, 1); !day.After(last); day = day.AddDays(1) {
		first := guitarSeries(day)
		for _, c := range []struct {
			what string
			date domain.Date
		}{
			{"the series starts", first},
			{"its second occurrence is", first.AddDays(14)},
		} {
			if c.date.Weekday() != time.Wednesday {
				t.Fatalf("seeded on %s (%s): %s %s, a %s", day, day.Weekday(), c.what, c.date, c.date.Weekday())
			}
			if c.date.Year != day.Year || c.date.Month != day.Month {
				t.Fatalf("seeded on %s: %s %s, outside %s %d", day, c.what, c.date, day.Month, day.Year)
			}
		}
	}
}

// The cinema is the one seeded event this rule is deliberately *not* applied to, and both
// halves of that are pinned here.
//
// It is a one-off evening out, so it has to stay near the day the demo was made: pinning
// it to a fixed Saturday of the month, the fix the two series got, would leave the demo
// advertising an outing three weeks ago. And it still has to be on the month the app
// opens on, which is the only set of days every user's grid contains — the trailing week
// of a five-row grid is not, and that is where the next Saturday after today could land
// (#67). Assert one and the other invites being optimised away.
func TestTheDemoEveningOutStaysNearTodayAndOnItsMonth(t *testing.T) {
	last := domain.NewDate(2033, time.December, 31)
	for day := domain.NewDate(2024, time.January, 1); !day.After(last); day = day.AddDays(1) {
		night := cinemaNight(day)
		if night.Weekday() != time.Saturday {
			t.Fatalf("seeded on %s (%s): the cinema is %s, a %s", day, day.Weekday(), night, night.Weekday())
		}
		if night.Year != day.Year || night.Month != day.Month {
			t.Fatalf("seeded on %s: the cinema is %s, outside %s %d", day, night, day.Month, day.Year)
		}
		// Six either way: forwards it is the coming Saturday, and backwards it is the one
		// before it, which is where the last days of a month send it.
		if days := day.DaysUntil(night); days < -6 || days > 6 {
			t.Fatalf("seeded on %s: the cinema is %s, %d days away", day, night, days)
		}
	}
}

// The seaside holiday is the largest of these by frequency and was missed by both of the
// issues that fixed the others (#72). It ran from today + 10 to today + 16, so the whole
// week was in the following month on a third of the days the seed could be run, and off
// the end of a Monday-start grid entirely on 164 of the 730 days of 2026 and 2027 —
// while the seeder's summary went on advertising a multi-day holiday.
//
// A span is asked about at both of its ends, which is the part the earlier two fixes had
// no occasion to think about: a bar is wholly on the screen only if the day it begins and
// the day it finishes are both on it, and seven days need seven consecutive days of the
// month. The tightest month is a 28-day February, which allows a start no later than the
// 22nd.
func TestTheDemoHolidayLandsWhollyOnTheMonthTheAppOpensOn(t *testing.T) {
	last := domain.NewDate(2033, time.December, 31)
	for day := domain.NewDate(2024, time.January, 1); !day.After(last); day = day.AddDays(1) {
		start, end := seasideHoliday(day)

		for _, c := range []struct {
			what string
			date domain.Date
		}{
			{"the holiday starts", start},
			{"the holiday ends", end},
		} {
			if c.date.Year != day.Year || c.date.Month != day.Month {
				t.Fatalf("seeded on %s: %s %s, outside %s %d", day, c.what, c.date, day.Month, day.Year)
			}
		}

		// Still a week. Shortening the span is the easy way to satisfy everything above
		// and leave the demo without the multi-day case it is in the seed for.
		if days := start.DaysUntil(end); days != 6 {
			t.Fatalf("seeded on %s: the holiday runs %s to %s, %d days long", day, start, end, days+1)
		}

		// Saturday, so the bar is drawn in two week rows for both of the week starts the
		// settings screen offers. A seven-day span sits in one row exactly when it begins
		// on the reader's first day of the week, and one row is not the case the lane
		// layout in web/js/views/month.js exists for.
		if start.Weekday() != time.Saturday {
			t.Fatalf("seeded on %s: the holiday starts %s, a %s", day, start, start.Weekday())
		}
	}
}

// The parents' evening was the last seeded date still counted blindly from the day the
// seed ran, and the mildest case of the same fault: tomorrow leaves the seeded month on
// the last day of one, and leaves the grid as well when that day ends a week.
//
// Both halves are asked for, as they are for the cinema. Beside today is what the
// appointment is doing in the seed, and on the month is what makes it visible to every
// reader; either assertion alone invites the other to be optimised away.
func TestTheDemoParentsEveningStaysBesideTodayAndOnItsMonth(t *testing.T) {
	last := domain.NewDate(2033, time.December, 31)
	for day := domain.NewDate(2024, time.January, 1); !day.After(last); day = day.AddDays(1) {
		evening := parentsEvening(day)
		if days := day.DaysUntil(evening); days != 1 && days != -1 {
			t.Fatalf("seeded on %s: the parents' evening is %s, %d days away", day, evening, days)
		}
		if evening.Year != day.Year || evening.Month != day.Month {
			t.Fatalf("seeded on %s: the parents' evening is %s, outside %s %d", day, evening, day.Month, day.Year)
		}
	}
}
