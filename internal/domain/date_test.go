package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDaysUntil(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"same day", "2026-07-26", "2026-07-26", 0},
		{"one day", "2026-07-26", "2026-07-27", 1},
		{"backwards", "2026-07-27", "2026-07-26", -1},
		{"across a month", "2026-01-31", "2026-02-01", 1},
		{"across a leap day", "2024-02-28", "2024-03-01", 2},
		{"across a common February", "2026-02-28", "2026-03-01", 1},
		{"a full common year", "2026-01-01", "2027-01-01", 365},
		{"a full leap year", "2024-01-01", "2025-01-01", 366},
		// A time.Duration saturates at ~292 years, so a Duration-based
		// implementation silently returns a wrong answer here. The recurrence
		// engine measures spans like this when it jumps to a query window.
		// 1000 years = 365000 days + 243 leap days (250 divisible by 4, less 10
		// centuries, plus 3 four-hundreds).
		{"a millennium", "1026-07-26", "2026-07-26", 365243},
		{"four centuries", "1626-07-26", "2026-07-26", 146097},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := MustParseDate(tt.a), MustParseDate(tt.b)
			if got := a.DaysUntil(b); got != tt.want {
				t.Errorf("%s.DaysUntil(%s) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestDaysUntilMatchesAddDays cross-checks the two independent implementations of
// date arithmetic against each other across a long span.
func TestDaysUntilMatchesAddDays(t *testing.T) {
	start := MustParseDate("1900-01-01")
	for n := 0; n < 60000; n += 37 {
		if got := start.DaysUntil(start.AddDays(n)); got != n {
			t.Fatalf("AddDays(%d) then DaysUntil = %d", n, got)
		}
	}
}

func TestAddMonthsRefusesToGuess(t *testing.T) {
	// AddMonths deliberately reports failure rather than choosing between the
	// skip and clamp policies, which differ by intent (a monthly bill skips, a
	// birthday clamps). See internal/recur.
	if _, ok := MustParseDate("2026-01-31").AddMonths(1); ok {
		t.Error("31 January + 1 month should report failure, not silently become 28 February or 3 March")
	}
	got, ok := MustParseDate("2026-01-31").AddMonths(2)
	if !ok || got.String() != "2026-03-31" {
		t.Errorf("31 January + 2 months = %v, %v; want 2026-03-31, true", got, ok)
	}
	if got, ok := MustParseDate("2026-11-30").AddMonths(2); !ok || got.String() != "2027-01-30" {
		t.Errorf("crossing a year boundary: got %v, %v", got, ok)
	}
	if got, ok := MustParseDate("2026-03-15").AddMonths(-4); !ok || got.String() != "2025-11-15" {
		t.Errorf("negative months: got %v, %v", got, ok)
	}
}

func TestNthWeekdayOfMonth(t *testing.T) {
	tests := []struct {
		name    string
		y       int
		m       time.Month
		wd      time.Weekday
		n       int
		want    string
		wantErr bool
	}{
		{name: "2nd Tuesday of August 2026", y: 2026, m: time.August, wd: time.Tuesday, n: 2, want: "2026-08-11"},
		{name: "1st Saturday of August 2026", y: 2026, m: time.August, wd: time.Saturday, n: 1, want: "2026-08-01"},
		{name: "last Friday of August 2026", y: 2026, m: time.August, wd: time.Friday, n: -1, want: "2026-08-28"},
		{name: "last day-of-week equals 5th when it exists", y: 2026, m: time.August, wd: time.Monday, n: 5, want: "2026-08-31"},
		{name: "no 5th Tuesday in August 2026", y: 2026, m: time.August, wd: time.Tuesday, n: 5, wantErr: true},
		{name: "ordinal zero is meaningless", y: 2026, m: time.August, wd: time.Tuesday, n: 0, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NthWeekdayOfMonth(tt.y, tt.m, tt.wd, tt.n)
			if tt.wantErr {
				if ok {
					t.Fatalf("expected no such date, got %s", got)
				}
				return
			}
			if !ok {
				t.Fatalf("expected %s, got no date", tt.want)
			}
			if got.String() != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
			if got.Weekday() != tt.wd {
				t.Errorf("%s is a %s, want %s", got, got.Weekday(), tt.wd)
			}
		})
	}
}

func TestLastDayAndDaysInMonth(t *testing.T) {
	if d := DaysInMonth(2024, time.February); d != 29 {
		t.Errorf("February 2024 has %d days, want 29", d)
	}
	if d := DaysInMonth(2026, time.February); d != 28 {
		t.Errorf("February 2026 has %d days, want 28", d)
	}
	if d := DaysInMonth(2000, time.February); d != 29 {
		t.Errorf("February 2000 (a 400-year leap) has %d days, want 29", d)
	}
	if d := DaysInMonth(1900, time.February); d != 28 {
		t.Errorf("February 1900 (a century, not a leap year) has %d days, want 28", d)
	}
	if got := LastDayOfMonth(2026, time.February); got.String() != "2026-02-28" {
		t.Errorf("LastDayOfMonth = %s", got)
	}
}

// TestAllDayHasNoTimezone is the regression guard for the off-by-one-day bug: a
// Date must round-trip through JSON and SQL as a plain calendar date, never as an
// instant that a UTC conversion could shift backwards.
func TestAllDayHasNoTimezone(t *testing.T) {
	d := MustParseDate("2026-07-15")

	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `"2026-07-15"` {
		t.Errorf("JSON = %s, want \"2026-07-15\"", encoded)
	}

	var back Date
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Equal(d) {
		t.Errorf("round-tripped to %s", back)
	}

	value, err := d.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if value != "2026-07-15" {
		t.Errorf("SQL value = %v, want the date as TEXT", value)
	}

	var scanned Date
	if err := scanned.Scan("2026-07-15"); err != nil || !scanned.Equal(d) {
		t.Errorf("Scan = %s, %v", scanned, err)
	}

	// Midnight in Paris on the 15th is 22:00 UTC on the 14th. Reading the date
	// back in the family zone must still say the 15th.
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	if got := DateIn(d.In(paris), paris); !got.Equal(d) {
		t.Errorf("midnight Paris on %s read back as %s", d, got)
	}
}

// TestWallClockResolvesOntoTheDayItWasAskedFor pins what a Date does with a wall time
// that a daylight-saving change broke, and it is two assertions at once.
//
// The first is that the existing rule is untouched. Which instant a skipped hour
// resolves to follows the zone rather than a policy — forward in Europe/Paris, backward
// in America/New_York — and both of those rows are pinned in internal/events and in a
// real browser as well. A change here that moved either of them would be a change to a
// documented promise, so they are restated in the two rows below that say "unchanged".
//
// The second is the rule this one is subordinate to: whatever the offsets do, the answer
// is on the date that was asked for. A zone that jumps at midnight has no 00:30 on the
// day it jumps, and until #57 the correction handed back 23:30 the previous evening —
// a date every bucket in the application then disagreed with, having no way to know it
// had been asked for a different one.
func TestWallClockResolvesOntoTheDayItWasAskedFor(t *testing.T) {
	cases := []struct {
		name            string
		zone, date      string
		hour, min       int
		wantUTC, wantHM string
		note            string
	}{
		{
			name: "an ordinary time is the ordinary answer",
			zone: "Europe/Paris", date: "2026-03-31", hour: 16, min: 30,
			wantUTC: "2026-03-31T14:30:00Z", wantHM: "16:30",
			note: "the summer offset applied to a time that exists",
		},
		{
			name: "Paris moves a skipped hour forward, unchanged",
			zone: "Europe/Paris", date: "2026-03-29", hour: 2, min: 30,
			wantUTC: "2026-03-29T01:30:00Z", wantHM: "03:30",
			note: "02:30 never happens on this date; the day does not change, so neither does the answer",
		},
		{
			name: "New York moves a skipped hour backward, unchanged",
			zone: "America/New_York", date: "2026-03-08", hour: 2, min: 30,
			wantUTC: "2026-03-08T06:30:00Z", wantHM: "01:30",
			note: "the opposite direction from Paris, and still the 8th, so it stands",
		},
		{
			name: "the repeated hour is untouched in either zone",
			zone: "Europe/Paris", date: "2026-10-25", hour: 2, min: 30,
			wantUTC: "2026-10-25T01:30:00Z", wantHM: "02:30",
			note: "an hour that happens twice names two instants, not none, so nothing here applies to it",
		},
		{
			name: "Santiago has no 00:30 on the day it jumps at midnight",
			zone: "America/Santiago", date: "2026-09-06", hour: 0, min: 30,
			wantUTC: "2026-09-06T04:30:00Z", wantHM: "01:30",
			note: "the reading this replaces is 2026-09-06T03:30:00Z, which is 23:30 on the 5th",
		},
		{
			name: "and no midnight either, so the day starts at 01:00",
			zone: "America/Santiago", date: "2026-09-06", hour: 0, min: 0,
			wantUTC: "2026-09-06T04:00:00Z", wantHM: "01:00",
			note: "Date.In is this case, and it is a day boundary in two range queries",
		},
		{
			name: "the week either side of it is ordinary",
			zone: "America/Santiago", date: "2026-09-13", hour: 0, min: 30,
			wantUTC: "2026-09-13T03:30:00Z", wantHM: "00:30",
			note: "00:30 exists on this date, and the summer offset is the one that says so",
		},
		{
			name: "Havana, the other zone with a household in it",
			zone: "America/Havana", date: "2026-03-08", hour: 0, min: 30,
			wantUTC: "2026-03-08T05:30:00Z", wantHM: "01:30",
			note: "the reading this replaces is 23:30 on the 7th",
		},
		{
			name: "the Azores, which is an hour from Lisbon and does the same thing",
			zone: "Atlantic/Azores", date: "2026-03-29", hour: 0, min: 30,
			wantUTC: "2026-03-29T01:30:00Z", wantHM: "01:30",
			note: "the reading this replaces is 23:30 on the 28th",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Skipf("no tzdata: %v", err)
			}
			d := MustParseDate(tc.date)
			got := d.At(tc.hour, tc.min, loc)
			if s := got.UTC().Format(time.RFC3339); s != tc.wantUTC {
				t.Errorf("%s %02d:%02d in %s = %s, want %s (%s)", tc.date, tc.hour, tc.min, tc.zone, s, tc.wantUTC, tc.note)
			}
			if s := got.In(loc).Format("15:04"); s != tc.wantHM {
				t.Errorf("%s %02d:%02d in %s reads back as %s, want %s", tc.date, tc.hour, tc.min, tc.zone, s, tc.wantHM)
			}
			// The one that matters to every caller: the instant is on the date.
			if bucket := DateIn(got, loc); !bucket.Equal(d) {
				t.Errorf("%s %02d:%02d in %s lands on %s, which is not the day it was asked for", tc.date, tc.hour, tc.min, tc.zone, bucket)
			}

			// The midnight rows belong to Date.In as much as to Date.At — it is the one
			// that is a day boundary in Store.EventsInRange and in the activity count
			// behind a summary — and asking only its neighbour leaves it free to go back
			// to bare time.Date without a single test noticing. It is not a synonym for
			// At(0, 0): a day with two midnights is In's business and not At's, which is
			// why the answer is restated here rather than compared to the row above.
			if tc.hour != 0 || tc.min != 0 {
				return
			}
			first := d.In(loc)
			if s := first.UTC().Format(time.RFC3339); s != tc.wantUTC {
				t.Errorf("%s.In(%s) = %s, want %s (%s)", tc.date, tc.zone, s, tc.wantUTC, tc.note)
			}
			if bucket := DateIn(first, loc); !bucket.Equal(d) {
				t.Errorf("%s.In(%s) lands on %s, which is not the day it was asked for", tc.date, tc.zone, bucket)
			}
		})
	}
}

// TestNoWallTimeInAWholeYearLeavesItsDay is the same promise made exhaustively rather
// than by example, because the examples above are the cases somebody thought of. Every
// minute of every day of a year, in the zones that break one, must resolve onto the day
// it names. Nothing else in this application would notice if one did not: an occurrence
// carries no memory of the date it was asked for once it is an instant, which is exactly
// why #57 was invisible until somebody queried the day and got nothing back.
func TestNoWallTimeInAWholeYearLeavesItsDay(t *testing.T) {
	// Every kind of zone that has ever been a problem: the two the household might use,
	// the negative-offset one whose skipped hour moves the other way, and four that jump
	// at midnight, one of them (Scoresbysund) having only just started to.
	zones := []string{
		"Europe/Paris", "America/New_York", "America/Santiago", "America/Havana",
		"Atlantic/Azores", "America/Scoresbysund", "Australia/Lord_Howe", "Pacific/Chatham",
	}
	first, last := MustParseDate("2026-01-01"), MustParseDate("2026-12-31")
	for _, zone := range zones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skipf("no tzdata: %v", err)
		}
		for d := first; !d.After(last); d = d.AddDays(1) {
			for mins := 0; mins < 24*60; mins++ {
				got := d.At(mins/60, mins%60, loc)
				if bucket := DateIn(got, loc); !bucket.Equal(d) {
					t.Fatalf("%s %02d:%02d in %s resolved to %s, which is %s",
						d, mins/60, mins%60, zone, got.In(loc).Format(time.RFC3339), bucket)
				}
			}
		}
	}
}

// TestADayWithTwoMidnightsBeginsAtTheFirstOne is the other half of #57, and it arrives
// at the same place from the opposite direction.
//
// A day fails to begin at midnight in two ways, not one. It can have no midnight — the
// clocks going forward at 00:00, which is the case #57 was reported for and which
// Date.at answers with 01:00. Or it can have two: the clocks going back onto or across
// 00:00, so the wall clock reads midnight, runs on to the jump and reads midnight again
// an hour or half an hour later. time.Date hands back the second of those, and every
// half-open window built from it then opens an hour into the day it named — which is
// exactly #57's symptom, at the other end of the window, and Store.EventsInRange and the
// activity count behind a summary are both such windows.
//
// The rows below are the shapes this takes rather than a tour of the zones: an hour, a
// half hour, three hours, and a zone that jumps at midnight the other way and must not
// move. Every one of them is historical — nothing in the database needs this after 2023
// — but Europe/Paris is on the list, which is the answer to "no household would ever
// see it".
func TestADayWithTwoMidnightsBeginsAtTheFirstOne(t *testing.T) {
	cases := []struct {
		name       string
		zone, date string
		wantUTC    string
		note       string
	}{
		{
			name: "an ordinary day begins at its only midnight",
			zone: "Europe/Paris", date: "2026-07-15",
			wantUTC: "2026-07-14T22:00:00Z",
			note:    "the summer offset, and nothing here applies",
		},
		{
			name: "a day with no midnight still begins at 01:00",
			zone: "America/Santiago", date: "2026-09-06",
			wantUTC: "2026-09-06T04:00:00Z",
			note:    "the #57 case, which this must leave exactly where it is",
		},
		{
			name: "Amman's clocks went back onto midnight, so the day has two",
			zone: "Asia/Amman", date: "2021-10-29",
			wantUTC: "2021-10-28T21:00:00Z",
			note:    "time.Date answers 22:00Z, an hour of the day later than the day starts",
		},
		{
			name: "and so did Paris, once, which is the zone this app defaults to",
			zone: "Europe/Paris", date: "1976-09-26",
			wantUTC: "1976-09-25T22:00:00Z",
			note:    "France moved its clocks at 01:00 that year; the reading time.Date gives is 23:00Z",
		},
		{
			name: "Colombo's was half an hour, so the gap need not be a whole one",
			zone: "Asia/Colombo", date: "2006-04-15",
			wantUTC: "2006-04-14T18:00:00Z",
			note:    "+06:00 to +05:30 at 00:30; assuming an hour would answer 18:30Z",
		},
		{
			name: "Casey's was three, so it need not be a small one either",
			zone: "Antarctica/Casey", date: "2023-03-09",
			wantUTC: "2023-03-08T13:00:00Z",
			note:    "+11:00 to +08:00; time.Date answers 16:00Z, a quarter of the day in",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Skipf("no tzdata: %v", err)
			}
			d := MustParseDate(tc.date)
			got := d.In(loc)
			if s := got.UTC().Format(time.RFC3339); s != tc.wantUTC {
				t.Errorf("%s.In(%s) = %s, want %s (%s)", tc.date, tc.zone, s, tc.wantUTC, tc.note)
			}
			if bucket := DateIn(got, loc); !bucket.Equal(d) {
				t.Errorf("%s.In(%s) lands on %s, which is not the day it was asked for", tc.date, tc.zone, bucket)
			}
			// "First" said as itself rather than as a number: the instant before the answer
			// belongs to an earlier day. On a day that runs from one end to the other — every
			// day but the three a zone deleted on crossing the date line — that is the whole
			// of the promise.
			if bucket := DateIn(got.Add(-time.Second), loc); bucket.Equal(d) {
				t.Errorf("%s.In(%s) = %s, but a second earlier is still %s",
					tc.date, tc.zone, got.UTC().Format(time.RFC3339), bucket)
			}
		})
	}

	// And exhaustively, over the years the case is thickest on the ground, because the
	// rows above are the ones somebody thought of. Amman moved its clocks at midnight
	// every autumn for a quarter of a century.
	loc, err := time.LoadLocation("Asia/Amman")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	first, last := MustParseDate("1995-01-01"), MustParseDate("2022-12-31")
	for d := first; !d.After(last); d = d.AddDays(1) {
		got := d.In(loc)
		if bucket := DateIn(got, loc); !bucket.Equal(d) {
			t.Fatalf("%s.In(Asia/Amman) = %s, which is %s", d, got.UTC().Format(time.RFC3339), bucket)
		}
		if bucket := DateIn(got.Add(-time.Second), loc); bucket.Equal(d) {
			t.Fatalf("%s.In(Asia/Amman) = %s, but a second earlier is still %s",
				d, got.UTC().Format(time.RFC3339), bucket)
		}
	}
}

func TestZeroDateIsUnset(t *testing.T) {
	var zero Date
	if !zero.IsZero() {
		t.Error("zero value should report IsZero")
	}
	encoded, _ := json.Marshal(zero)
	if string(encoded) != "null" {
		t.Errorf("unset date encodes as %s, want null", encoded)
	}
	v, _ := zero.Value()
	if v != nil {
		t.Errorf("unset date stores as %v, want NULL", v)
	}
}
