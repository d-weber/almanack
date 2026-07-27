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
