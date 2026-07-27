package recur

import (
	"errors"
	"strings"
	"testing"
	"time"

	"almanack/internal/domain"
)

// Every expected date in this file was derived from the calendar, not from the code
// under test: a case that only agrees with the implementation proves nothing.

func day(s string) domain.Date { return domain.MustParseDate(s) }

func at(s string) *domain.Date {
	d := domain.MustParseDate(s)
	return &d
}

func num(n int) *int { return &n }

func strs(ds []domain.Date) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.String()
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// daily/weekly/monthly/yearly rule builders keep the tables readable.

func daily(start string, interval int) domain.Recurrence {
	return domain.Recurrence{Freq: domain.FreqDaily, Interval: interval, DTStart: day(start)}
}

func weekly(start string, interval int, wds ...time.Weekday) domain.Recurrence {
	return domain.Recurrence{Freq: domain.FreqWeekly, Interval: interval, DTStart: day(start), ByWeekday: wds}
}

func monthlyDay(start string, interval, monthday int) domain.Recurrence {
	return domain.Recurrence{Freq: domain.FreqMonthly, Interval: interval, DTStart: day(start), ByMonthday: num(monthday)}
}

func monthlyNth(start string, interval int, wd time.Weekday, ordinal int) domain.Recurrence {
	return domain.Recurrence{
		Freq: domain.FreqMonthly, Interval: interval, DTStart: day(start),
		ByWeekday: []time.Weekday{wd}, WeekOrdinal: num(ordinal),
	}
}

func monthlyLast(start string, interval int) domain.Recurrence {
	return domain.Recurrence{Freq: domain.FreqMonthly, Interval: interval, DTStart: day(start), MonthLastDay: true}
}

func yearly(start string, interval int) domain.Recurrence {
	return domain.Recurrence{Freq: domain.FreqYearly, Interval: interval, DTStart: day(start)}
}

func withUntil(r domain.Recurrence, until string) domain.Recurrence {
	r.Until = at(until)
	return r
}

func TestExpand(t *testing.T) {
	tests := []struct {
		name     string
		r        domain.Recurrence
		from, to string
		want     []string
	}{
		// ---- daily: every `interval` days from DTStart ----
		{
			name: "daily/every day",
			r:    daily("2026-03-01", 1), from: "2026-03-01", to: "2026-03-05",
			want: []string{"2026-03-01", "2026-03-02", "2026-03-03", "2026-03-04", "2026-03-05"},
		},
		{
			name: "daily/interval 3 anchored on dtstart",
			r:    daily("2026-01-01", 3), from: "2026-01-01", to: "2026-01-15",
			want: []string{"2026-01-01", "2026-01-04", "2026-01-07", "2026-01-10", "2026-01-13"},
		},
		{
			// Jan 31 is the 11th occurrence (day 1 + 30), so the parity carries into
			// February; a window that starts mid-series must not re-anchor.
			name: "daily/window starting mid-series keeps the dtstart anchor",
			r:    daily("2026-01-01", 3), from: "2026-02-01", to: "2026-02-10",
			want: []string{"2026-02-03", "2026-02-06", "2026-02-09"},
		},

		// ---- weekly ----
		{
			name: "weekly/empty by_weekday defaults to the dtstart weekday",
			r:    weekly("2026-01-06", 1), from: "2026-01-01", to: "2026-02-01", // Tuesday
			want: []string{"2026-01-06", "2026-01-13", "2026-01-20", "2026-01-27"},
		},
		{
			name: "weekly/multiple weekdays in every qualifying week",
			r:    weekly("2026-01-06", 1, time.Tuesday, time.Thursday), from: "2026-01-01", to: "2026-01-20",
			want: []string{"2026-01-06", "2026-01-08", "2026-01-13", "2026-01-15", "2026-01-20"},
		},
		{
			// Tuesday 6 Jan sits in DTStart's own week but before DTStart itself.
			name: "weekly/never emits before dtstart inside dtstart's own week",
			r:    weekly("2026-01-08", 1, time.Tuesday, time.Thursday), from: "2026-01-01", to: "2026-01-20",
			want: []string{"2026-01-08", "2026-01-13", "2026-01-15", "2026-01-20"},
		},
		{
			// The Thursday of the first week falls in the next calendar year; parity is
			// a property of the Monday week, not of the year.
			name: "weekly/every 2 weeks on tue+thu across a year boundary",
			r:    weekly("2025-12-30", 2, time.Tuesday, time.Thursday), from: "2025-12-01", to: "2026-03-15",
			want: []string{
				"2025-12-30", "2026-01-01",
				"2026-01-13", "2026-01-15",
				"2026-01-27", "2026-01-29",
				"2026-02-10", "2026-02-12",
				"2026-02-24", "2026-02-26",
				"2026-03-10", "2026-03-12",
			},
		},
		{
			name: "weekly/every 2 weeks parity does not drift in a later window",
			r:    weekly("2025-12-30", 2, time.Tuesday, time.Thursday), from: "2026-02-01", to: "2026-03-15",
			want: []string{"2026-02-10", "2026-02-12", "2026-02-24", "2026-02-26", "2026-03-10", "2026-03-12"},
		},
		{
			// DTStart is a Sunday, so a Sunday-started week would put Monday 5 Jan in
			// the first week and shift every later pair. WKST is Monday: 4 Jan belongs
			// to the week of Monday 29 Dec, and the next qualifying week is 12 Jan.
			name: "weekly/wkst is monday and not the user's week_start",
			r:    weekly("2026-01-04", 2, time.Sunday, time.Monday), from: "2026-01-01", to: "2026-02-05",
			want: []string{"2026-01-04", "2026-01-12", "2026-01-18", "2026-01-26", "2026-02-01"},
		},
		{
			name: "weekly/duplicate weekdays do not duplicate occurrences",
			r:    weekly("2026-01-06", 1, time.Tuesday, time.Tuesday), from: "2026-01-01", to: "2026-01-20",
			want: []string{"2026-01-06", "2026-01-13", "2026-01-20"},
		},

		// ---- monthly by monthday: short months are SKIPPED, never clamped ----
		{
			name: "monthly/on the 31st across a full year skips short months",
			r:    monthlyDay("2026-01-31", 1, 31), from: "2026-01-01", to: "2026-12-31",
			want: []string{
				"2026-01-31", "2026-03-31", "2026-05-31", "2026-07-31",
				"2026-08-31", "2026-10-31", "2026-12-31",
			},
		},
		{
			name: "monthly/on the 30th skips february only",
			r:    monthlyDay("2026-01-30", 1, 30), from: "2026-01-01", to: "2026-06-30",
			want: []string{"2026-01-30", "2026-03-30", "2026-04-30", "2026-05-30", "2026-06-30"},
		},
		{
			name: "monthly/on the 29th skips february in a common year",
			r:    monthlyDay("2026-01-29", 1, 29), from: "2026-01-01", to: "2026-04-30",
			want: []string{"2026-01-29", "2026-03-29", "2026-04-29"},
		},
		{
			name: "monthly/on the 29th keeps february in a leap year",
			r:    monthlyDay("2024-01-29", 1, 29), from: "2024-01-01", to: "2024-04-30",
			want: []string{"2024-01-29", "2024-02-29", "2024-03-29", "2024-04-29"},
		},
		{
			// The interval counts months, including the ones the rule cannot satisfy:
			// Jan, Mar, May, Jul, Sep, Nov — and September and November have 30 days.
			name: "monthly/interval counts skipped months too",
			r:    monthlyDay("2026-01-31", 2, 31), from: "2026-01-01", to: "2026-12-31",
			want: []string{"2026-01-31", "2026-03-31", "2026-05-31", "2026-07-31"},
		},
		{
			name: "monthly/interval 3 anchored on dtstart",
			r:    monthlyDay("2026-01-15", 3, 15), from: "2026-01-01", to: "2026-12-31",
			want: []string{"2026-01-15", "2026-04-15", "2026-07-15", "2026-10-15"},
		},
		{
			name: "monthly/window starting mid-series",
			r:    monthlyDay("2026-01-31", 1, 31), from: "2026-06-01", to: "2026-09-30",
			want: []string{"2026-07-31", "2026-08-31"},
		},
		{
			name: "monthly/monthday earlier than dtstart's day starts next month",
			r:    monthlyDay("2026-01-20", 1, 10), from: "2026-01-01", to: "2026-04-30",
			want: []string{"2026-02-10", "2026-03-10", "2026-04-10"},
		},

		// ---- monthly last calendar day ----
		{
			name: "monthly/last day across february in a leap year",
			r:    monthlyLast("2024-01-31", 1), from: "2024-01-01", to: "2024-05-31",
			want: []string{"2024-01-31", "2024-02-29", "2024-03-31", "2024-04-30", "2024-05-31"},
		},
		{
			name: "monthly/last day across february in a common year",
			r:    monthlyLast("2026-01-31", 1), from: "2026-01-01", to: "2026-05-31",
			want: []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30", "2026-05-31"},
		},
		{
			name: "monthly/last day across a year boundary",
			r:    monthlyLast("2026-11-30", 1), from: "2026-11-01", to: "2027-02-28",
			want: []string{"2026-11-30", "2026-12-31", "2027-01-31", "2027-02-28"},
		},

		// ---- monthly nth weekday ----
		{
			name: "monthly/2nd tuesday",
			r:    monthlyNth("2026-01-13", 1, time.Tuesday, 2), from: "2026-01-01", to: "2026-06-30",
			want: []string{"2026-01-13", "2026-02-10", "2026-03-10", "2026-04-14", "2026-05-12", "2026-06-09"},
		},
		{
			name: "monthly/last friday",
			r:    monthlyNth("2026-01-30", 1, time.Friday, -1), from: "2026-01-01", to: "2026-06-30",
			want: []string{"2026-01-30", "2026-02-27", "2026-03-27", "2026-04-24", "2026-05-29", "2026-06-26"},
		},
		{
			// Only March, June, September and December 2026 have a fifth Tuesday; the
			// other eight months produce nothing rather than falling back to the fourth.
			name: "monthly/5th tuesday skips the months that have none",
			r:    monthlyNth("2026-01-01", 1, time.Tuesday, 5), from: "2026-01-01", to: "2026-12-31",
			want: []string{"2026-03-31", "2026-06-30", "2026-09-29", "2026-12-29"},
		},
		{
			name: "monthly/1st monday every 2 months",
			r:    monthlyNth("2026-01-05", 2, time.Monday, 1), from: "2026-01-01", to: "2026-08-31",
			want: []string{"2026-01-05", "2026-03-02", "2026-05-04", "2026-07-06"},
		},

		// ---- yearly: 29 February CLAMPS (the opposite of the monthly rule) ----
		{
			name: "yearly/29 february clamps to the 28th in common years and is exact in leap years",
			r:    yearly("2024-02-29", 1), from: "2024-01-01", to: "2028-12-31",
			want: []string{"2024-02-29", "2025-02-28", "2026-02-28", "2027-02-28", "2028-02-29"},
		},
		{
			name: "yearly/29 february in a window starting mid-series",
			r:    yearly("2024-02-29", 1), from: "2026-03-01", to: "2029-03-01",
			want: []string{"2027-02-28", "2028-02-29", "2029-02-28"},
		},
		{
			name: "yearly/ordinary date each year",
			r:    yearly("2026-07-14", 1), from: "2026-01-01", to: "2029-12-31",
			want: []string{"2026-07-14", "2027-07-14", "2028-07-14", "2029-07-14"},
		},
		{
			name: "yearly/interval 2 anchored on dtstart",
			r:    yearly("2025-05-01", 2), from: "2025-01-01", to: "2031-12-31",
			want: []string{"2025-05-01", "2027-05-01", "2029-05-01", "2031-05-01"},
		},
		{
			name: "yearly/30 january is never affected by month length",
			r:    yearly("2026-01-30", 1), from: "2026-01-01", to: "2028-12-31",
			want: []string{"2026-01-30", "2027-01-30", "2028-01-30"},
		},

		// ---- until is inclusive ----
		{
			name: "until/an occurrence exactly on until is emitted",
			r:    withUntil(daily("2026-03-01", 2), "2026-03-07"), from: "2026-03-01", to: "2026-03-31",
			want: []string{"2026-03-01", "2026-03-03", "2026-03-05", "2026-03-07"},
		},
		{
			name: "until/the occurrence after until is not emitted",
			r:    withUntil(daily("2026-03-01", 2), "2026-03-06"), from: "2026-03-01", to: "2026-03-31",
			want: []string{"2026-03-01", "2026-03-03", "2026-03-05"},
		},
		{
			name: "until/equal to dtstart yields exactly one occurrence",
			r:    withUntil(daily("2026-03-01", 1), "2026-03-01"), from: "2026-01-01", to: "2026-12-31",
			want: []string{"2026-03-01"},
		},
		{
			name: "until/clips a weekly series mid-week",
			r:    withUntil(weekly("2026-01-06", 1, time.Tuesday, time.Thursday), "2026-01-13"), from: "2026-01-01", to: "2026-12-31",
			want: []string{"2026-01-06", "2026-01-08", "2026-01-13"},
		},

		// ---- window edges ----
		{
			name: "window/entirely before the series",
			r:    daily("2026-03-01", 1), from: "2026-01-01", to: "2026-02-01",
			want: nil,
		},
		{
			name: "window/entirely after until",
			r:    withUntil(daily("2026-03-01", 1), "2026-03-10"), from: "2026-04-01", to: "2026-05-01",
			want: nil,
		},
		{
			name: "window/starts before dtstart and is clipped to it",
			r:    daily("2026-03-01", 1), from: "2026-01-01", to: "2026-03-03",
			want: []string{"2026-03-01", "2026-03-02", "2026-03-03"},
		},
		{
			name: "window/from after to is empty",
			r:    daily("2026-03-01", 1), from: "2026-03-10", to: "2026-03-01",
			want: nil,
		},
		{
			name: "window/single day that is an occurrence",
			r:    weekly("2026-01-06", 1, time.Tuesday), from: "2026-01-13", to: "2026-01-13",
			want: []string{"2026-01-13"},
		},
		{
			name: "window/single day that is not an occurrence",
			r:    weekly("2026-01-06", 1, time.Tuesday), from: "2026-01-14", to: "2026-01-14",
			want: nil,
		},

		// ---- malformed rules expand to nothing rather than to a guess ----
		{
			name: "invalid/interval zero expands to nothing",
			r:    daily("2026-03-01", 0), from: "2026-01-01", to: "2026-12-31",
			want: nil,
		},
		{
			name: "invalid/monthly with two modes expands to nothing",
			r: domain.Recurrence{
				Freq: domain.FreqMonthly, Interval: 1, DTStart: day("2026-01-31"),
				ByMonthday: num(31), MonthLastDay: true,
			},
			from: "2026-01-01", to: "2026-12-31",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := strs(Expand(tc.r, day(tc.from), day(tc.to)))
			if !equal(got, tc.want) {
				t.Errorf("Expand(%s..%s)\n got: %v\nwant: %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestNext(t *testing.T) {
	tests := []struct {
		name   string
		r      domain.Recurrence
		after  string
		want   string // "" means no next occurrence
		wantOK bool
	}{
		{
			name: "next/before dtstart returns dtstart",
			r:    daily("2026-03-01", 2), after: "2026-01-01", want: "2026-03-01", wantOK: true,
		},
		{
			name: "next/exactly on dtstart is strict and returns the second occurrence",
			r:    daily("2026-03-01", 2), after: "2026-03-01", want: "2026-03-03", wantOK: true,
		},
		{
			name: "next/on a non-occurrence day returns the following occurrence",
			r:    daily("2026-03-01", 2), after: "2026-03-02", want: "2026-03-03", wantOK: true,
		},
		{
			name: "next/just before until returns the until occurrence",
			r:    withUntil(daily("2026-03-01", 2), "2026-03-07"), after: "2026-03-06", want: "2026-03-07", wantOK: true,
		},
		{
			name: "next/exactly on until returns nothing",
			r:    withUntil(daily("2026-03-01", 2), "2026-03-07"), after: "2026-03-07", wantOK: false,
		},
		{
			name: "next/after until returns nothing",
			r:    withUntil(daily("2026-03-01", 2), "2026-03-07"), after: "2026-06-01", wantOK: false,
		},
		{
			name: "next/weekly biweekly keeps the dtstart parity",
			r:    weekly("2025-12-30", 2, time.Tuesday, time.Thursday), after: "2026-01-01", want: "2026-01-13", wantOK: true,
		},
		{
			name: "next/monthly on the 31st skips february",
			r:    monthlyDay("2026-01-31", 1, 31), after: "2026-01-31", want: "2026-03-31", wantOK: true,
		},
		{
			name: "next/monthly 5th tuesday jumps three months",
			r:    monthlyNth("2026-01-01", 1, time.Tuesday, 5), after: "2026-03-31", want: "2026-06-30", wantOK: true,
		},
		{
			name: "next/monthly last day across february",
			r:    monthlyLast("2026-01-31", 1), after: "2026-01-31", want: "2026-02-28", wantOK: true,
		},
		{
			name: "next/yearly 29 february clamps",
			r:    yearly("2024-02-29", 1), after: "2024-02-29", want: "2025-02-28", wantOK: true,
		},
		{
			name: "next/yearly from just after a clamped occurrence",
			r:    yearly("2024-02-29", 1), after: "2025-02-28", want: "2026-02-28", wantOK: true,
		},
		{
			name: "next/far future query on an endless daily series",
			r:    daily("2015-01-01", 1), after: "2040-05-31", want: "2040-06-01", wantOK: true,
		},
		{
			// Every period lands in February, which never has a 30th: the rule has no
			// occurrence at all, and Next must say so instead of searching forever.
			name: "next/rule that can never occur returns nothing",
			r:    monthlyDay("2026-02-15", 12, 30), after: "2026-01-01", wantOK: false,
		},
		{
			name: "next/invalid rule returns nothing",
			r:    daily("2026-03-01", 0), after: "2026-01-01", wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Next(tc.r, day(tc.after))
			if ok != tc.wantOK {
				t.Fatalf("Next(after=%s) ok = %v, want %v (got %s)", tc.after, ok, tc.wantOK, got)
			}
			if ok && got.String() != tc.want {
				t.Errorf("Next(after=%s) = %s, want %s", tc.after, got, tc.want)
			}
		})
	}
}

func TestOccurs(t *testing.T) {
	tests := []struct {
		name string
		r    domain.Recurrence
		date string
		want bool
	}{
		{name: "occurs/daily on an interval day", r: daily("2026-03-01", 2), date: "2026-03-05", want: true},
		{name: "occurs/daily off an interval day", r: daily("2026-03-01", 2), date: "2026-03-04", want: false},
		{name: "occurs/dtstart itself", r: daily("2026-03-01", 2), date: "2026-03-01", want: true},
		{name: "occurs/before dtstart", r: daily("2026-03-01", 2), date: "2026-02-27", want: false},
		{name: "occurs/exactly on until", r: withUntil(daily("2026-03-01", 2), "2026-03-07"), date: "2026-03-07", want: true},
		{name: "occurs/after until", r: withUntil(daily("2026-03-01", 2), "2026-03-07"), date: "2026-03-09", want: false},
		{name: "occurs/weekly right weekday right week", r: weekly("2025-12-30", 2, time.Tuesday, time.Thursday), date: "2026-01-15", want: true},
		{name: "occurs/weekly right weekday wrong week", r: weekly("2025-12-30", 2, time.Tuesday, time.Thursday), date: "2026-01-08", want: false},
		{name: "occurs/weekly wrong weekday right week", r: weekly("2025-12-30", 2, time.Tuesday, time.Thursday), date: "2026-01-14", want: false},
		{name: "occurs/monthly 31st in a month that has one", r: monthlyDay("2026-01-31", 1, 31), date: "2026-03-31", want: true},
		{name: "occurs/monthly 31st in february is never clamped", r: monthlyDay("2026-01-31", 1, 31), date: "2026-02-28", want: false},
		{name: "occurs/monthly last day in a common february", r: monthlyLast("2026-01-31", 1), date: "2026-02-28", want: true},
		{name: "occurs/monthly last day is not the day before", r: monthlyLast("2026-01-31", 1), date: "2026-02-27", want: false},
		{name: "occurs/monthly 2nd tuesday", r: monthlyNth("2026-01-13", 1, time.Tuesday, 2), date: "2026-04-14", want: true},
		{name: "occurs/monthly 2nd tuesday is not the 1st tuesday", r: monthlyNth("2026-01-13", 1, time.Tuesday, 2), date: "2026-04-07", want: false},
		{name: "occurs/yearly clamped 29 february", r: yearly("2024-02-29", 1), date: "2025-02-28", want: true},
		{name: "occurs/yearly clamped day does not leak to 1 march", r: yearly("2024-02-29", 1), date: "2025-03-01", want: false},
		{name: "occurs/yearly exact in a leap year", r: yearly("2024-02-29", 1), date: "2028-02-29", want: true},
		{name: "occurs/yearly off-interval year", r: yearly("2025-05-01", 2), date: "2026-05-01", want: false},
		{name: "occurs/invalid rule", r: daily("2026-03-01", 0), date: "2026-03-01", want: false},
		{name: "occurs/zero date", r: daily("2026-03-01", 1), date: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d domain.Date
			if tc.date != "" {
				d = day(tc.date)
			}
			if got := Occurs(tc.r, d); got != tc.want {
				t.Errorf("Occurs(%s) = %v, want %v", d, got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		r    domain.Recurrence
		want string // substring of the expected message; "" means the rule is valid
	}{
		{name: "valid/daily", r: daily("2026-03-01", 1)},
		{name: "valid/weekly with weekdays", r: weekly("2026-01-06", 2, time.Tuesday, time.Thursday)},
		{name: "valid/weekly with no weekdays", r: weekly("2026-01-06", 1)},
		{name: "valid/monthly by monthday", r: monthlyDay("2026-01-31", 1, 31)},
		{name: "valid/monthly nth weekday", r: monthlyNth("2026-01-13", 1, time.Tuesday, 2)},
		{name: "valid/monthly last weekday", r: monthlyNth("2026-01-30", 1, time.Friday, -1)},
		{name: "valid/monthly last day", r: monthlyLast("2026-01-31", 1)},
		{name: "valid/yearly", r: yearly("2024-02-29", 1)},
		{name: "valid/until equal to dtstart", r: withUntil(daily("2026-03-01", 1), "2026-03-01")},
		{name: "valid/sunday and saturday are in range", r: weekly("2026-01-06", 1, time.Sunday, time.Saturday)},

		{name: "invalid/unknown freq", r: domain.Recurrence{Freq: "hourly", Interval: 1, DTStart: day("2026-03-01")}, want: "unknown frequency"},
		{name: "invalid/empty freq", r: domain.Recurrence{Interval: 1, DTStart: day("2026-03-01")}, want: "unknown frequency"},
		{name: "invalid/interval zero", r: daily("2026-03-01", 0), want: "interval must be at least 1"},
		{name: "invalid/interval negative", r: daily("2026-03-01", -2), want: "interval must be at least 1"},
		{name: "invalid/missing dtstart", r: domain.Recurrence{Freq: domain.FreqDaily, Interval: 1}, want: "dtstart is required"},
		{name: "invalid/weekday above saturday", r: weekly("2026-01-06", 1, time.Weekday(7)), want: "weekday 7 out of range"},
		{name: "invalid/weekday below sunday", r: weekly("2026-01-06", 1, time.Weekday(-1)), want: "weekday -1 out of range"},
		{name: "invalid/until before dtstart", r: withUntil(daily("2026-03-01", 1), "2026-02-28"), want: "before dtstart"},
		{
			name: "invalid/monthly with no mode",
			r:    domain.Recurrence{Freq: domain.FreqMonthly, Interval: 1, DTStart: day("2026-01-31")},
			want: "exactly one of",
		},
		{
			name: "invalid/monthly with two modes",
			r: domain.Recurrence{Freq: domain.FreqMonthly, Interval: 1, DTStart: day("2026-01-31"),
				ByMonthday: num(31), MonthLastDay: true},
			want: "exactly one of",
		},
		{
			name: "invalid/monthly with three modes",
			r: domain.Recurrence{Freq: domain.FreqMonthly, Interval: 1, DTStart: day("2026-01-31"),
				ByMonthday: num(31), MonthLastDay: true, ByWeekday: []time.Weekday{time.Tuesday}, WeekOrdinal: num(2)},
			want: "exactly one of",
		},
		{
			name: "invalid/monthly week_ordinal without a weekday",
			r: domain.Recurrence{Freq: domain.FreqMonthly, Interval: 1, DTStart: day("2026-01-13"),
				WeekOrdinal: num(2)},
			want: "week_ordinal needs a by_weekday",
		},
		{
			name: "invalid/monthly weekday without a week_ordinal",
			r: domain.Recurrence{Freq: domain.FreqMonthly, Interval: 1, DTStart: day("2026-01-13"),
				ByWeekday: []time.Weekday{time.Tuesday}},
			want: "needs a week_ordinal",
		},
		{name: "invalid/monthly monthday zero", r: monthlyDay("2026-01-31", 1, 0), want: "by_monthday 0 out of range"},
		{name: "invalid/monthly monthday 32", r: monthlyDay("2026-01-31", 1, 32), want: "by_monthday 32 out of range"},
		{name: "invalid/monthly monthday negative", r: monthlyDay("2026-01-31", 1, -1), want: "by_monthday -1 out of range"},
		{name: "invalid/monthly week_ordinal zero", r: monthlyNth("2026-01-13", 1, time.Tuesday, 0), want: "week_ordinal 0 must be"},
		{name: "invalid/monthly week_ordinal six", r: monthlyNth("2026-01-13", 1, time.Tuesday, 6), want: "week_ordinal 6 must be"},
		{name: "invalid/monthly week_ordinal minus two", r: monthlyNth("2026-01-13", 1, time.Tuesday, -2), want: "week_ordinal -2 must be"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.r)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.want)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("Validate() = %v, want it to wrap domain.ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestExpandJumpsToTheWindow is the performance guard: expansion must compute where the
// window starts, not walk to it.
//
// It is written as a ratio rather than a stopwatch reading. An absolute budget either
// flakes on a loaded machine or is loose enough that a naive walk slips under it — a
// correct-but-walking implementation of this package expands one month of a 2015 daily
// series in about a microsecond, which no honest budget can distinguish from the right
// answer. What it cannot fake is the *shape* of the cost: walking makes a 25-year-old
// series roughly 300 times dearer than a one-month-old one, while jumping makes them
// cost the same. The two rule sets below produce identical occurrence counts over the
// same window and differ only in how old the series is.
func TestExpandJumpsToTheWindow(t *testing.T) {
	from, to := day("2040-06-01"), day("2040-06-30")

	type pair struct {
		name       string
		old, young domain.Recurrence
		want       int
	}
	pairs := []pair{
		{"daily", daily("2015-01-01", 1), daily("2040-05-01", 1), 30},
		{"weekly mon+fri", weekly("2015-01-02", 1, time.Monday, time.Friday), weekly("2040-05-01", 1, time.Monday, time.Friday), 9},
		{"monthly 15th", monthlyDay("2015-01-15", 1, 15), monthlyDay("2040-05-15", 1, 15), 1},
		{"yearly 15 june", yearly("2015-06-15", 1), yearly("2039-06-15", 1), 1},
	}

	// Both series must genuinely produce the same occurrences, or the comparison below
	// would be measuring different amounts of output rather than different amounts of
	// searching.
	for _, p := range pairs {
		gotOld, gotYoung := strs(Expand(p.old, from, to)), strs(Expand(p.young, from, to))
		if !equal(gotOld, gotYoung) {
			t.Fatalf("%s: old series gave %v, young series gave %v", p.name, gotOld, gotYoung)
		}
		if len(gotOld) != p.want {
			t.Fatalf("%s: got %d occurrences, want %d", p.name, len(gotOld), p.want)
		}
	}

	const repeats = 5000
	run := func(pick func(pair) domain.Recurrence) time.Duration {
		start := time.Now()
		for i := 0; i < repeats; i++ {
			for _, p := range pairs {
				if n := len(Expand(pick(p), from, to)); n != p.want {
					t.Fatalf("%s: got %d occurrences, want %d", p.name, n, p.want)
				}
			}
		}
		return time.Since(start)
	}
	young := run(func(p pair) domain.Recurrence { return p.young })
	old := run(func(p pair) domain.Recurrence { return p.old })

	// Generous: the honest ratio is ~1, a walking implementation is in the hundreds, and
	// the additive slack keeps a scheduling hiccup on a sub-millisecond measurement from
	// failing the build.
	if limit := 10*young + 50*time.Millisecond; old > limit {
		t.Errorf("expanding series that started in 2015 took %s vs %s for series that started weeks earlier (limit %s):\n"+
			"cost is scaling with the age of the series, so expansion is walking to the window instead of jumping to it", old, young, limit)
	}

	// Fast must not be bought with wrong dates.
	got := strs(Expand(daily("2015-01-01", 1), from, to))
	if got[0] != "2040-06-01" || got[len(got)-1] != "2040-06-30" {
		t.Errorf("daily window = %s..%s, want 2040-06-01..2040-06-30", got[0], got[len(got)-1])
	}
	// Every 3 days from 1 January 2015: 2040-06-01 is 9 283 days later and 9 283 is not
	// a multiple of 3, so the June occurrences start on the 3rd — the anchor parity has
	// to survive the jump.
	if got := strs(Expand(daily("2015-01-01", 3), from, to)); got[0] != "2040-06-03" || len(got) != 10 {
		t.Errorf("every-3-days window = %v, want 10 dates starting 2040-06-03", got)
	}
}

// TestExpandNextOccursAgree checks the three entry points against each other on a
// spread of rules: whatever Expand emits, Occurs must confirm and Next must reach, in
// the same order and with nothing in between.
func TestExpandNextOccursAgree(t *testing.T) {
	rules := []struct {
		name string
		r    domain.Recurrence
	}{
		{"daily interval 4", daily("2026-01-01", 4)},
		{"weekly biweekly tue+thu", weekly("2025-12-30", 2, time.Tuesday, time.Thursday)},
		{"weekly triweekly sun+mon", weekly("2026-01-04", 3, time.Sunday, time.Monday)},
		{"monthly 31st", monthlyDay("2026-01-31", 1, 31)},
		{"monthly last day", monthlyLast("2026-01-31", 1)},
		{"monthly 5th tuesday", monthlyNth("2026-01-01", 1, time.Tuesday, 5)},
		{"monthly last friday every 2 months", monthlyNth("2026-01-30", 2, time.Friday, -1)},
		{"yearly leap day", yearly("2024-02-29", 1)},
		{"daily with until", withUntil(daily("2026-01-01", 3), "2026-02-14")},
	}

	from, to := day("2024-01-01"), day("2030-12-31")
	for _, rule := range rules {
		t.Run(rule.name, func(t *testing.T) {
			got := Expand(rule.r, from, to)
			if len(got) == 0 {
				t.Fatal("expected at least one occurrence in the window")
			}
			for i, d := range got {
				if i > 0 && !got[i-1].Before(d) {
					t.Fatalf("occurrences are not strictly ascending: %s then %s", got[i-1], d)
				}
				if !Occurs(rule.r, d) {
					t.Errorf("Expand emitted %s but Occurs says it is not an occurrence", d)
				}
			}
			// Walking with Next must reproduce the same list exactly.
			cursor := from.AddDays(-1)
			for i, want := range got {
				n, ok := Next(rule.r, cursor)
				if !ok {
					t.Fatalf("Next stopped after %d of %d occurrences", i, len(got))
				}
				if !n.Equal(want) {
					t.Fatalf("Next gave %s, want %s", n, want)
				}
				cursor = n
			}
			// And nothing Expand skipped may be an occurrence: sample the gaps.
			for d := got[0]; !d.After(got[len(got)-1]); d = d.AddDays(1) {
				want := false
				for _, o := range got {
					if o.Equal(d) {
						want = true
						break
					}
				}
				if Occurs(rule.r, d) != want {
					t.Fatalf("Occurs(%s) = %v, want %v", d, !want, want)
				}
			}
		})
	}
}

// TestDaysBetween pins the civil-date arithmetic this package uses instead of
// Date.DaysUntil, which saturates its time.Duration past ~292 years.
func TestDaysBetween(t *testing.T) {
	if got := dayNumber(day("1970-01-01")); got != 0 {
		t.Errorf("dayNumber(1970-01-01) = %d, want 0", got)
	}
	// Agrees with the domain helper everywhere the helper is trustworthy.
	for _, pair := range [][2]string{
		{"1970-01-01", "1970-01-02"},
		{"2026-01-01", "2026-12-31"},
		{"1900-01-01", "2100-01-01"},
		{"2024-02-28", "2024-03-01"},
		{"2026-02-28", "2026-03-01"},
		{"2000-02-28", "2000-03-01"},
		{"1900-02-28", "1900-03-01"},
		{"2040-06-30", "2015-01-01"}, // negative
	} {
		a, b := day(pair[0]), day(pair[1])
		if got, want := daysBetween(a, b), a.DaysUntil(b); got != want {
			t.Errorf("daysBetween(%s, %s) = %d, want %d", a, b, got, want)
		}
	}
	// And keeps working past the Duration limit, which is the reason it exists.
	if got := daysBetween(day("1600-01-01"), day("2400-01-01")); got != 292194 {
		t.Errorf("daysBetween(1600-01-01, 2400-01-01) = %d, want 292194", got)
	}
}
