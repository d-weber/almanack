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
