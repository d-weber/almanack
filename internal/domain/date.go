package domain

import (
	"database/sql/driver"
	"fmt"
	"time"
)

// Date is a calendar date with no time-of-day and no timezone: the 14th of July is
// the 14th of July whether you are in Paris or Lisbon.
//
// All-day events and recurrence occurrence identity use this type rather than a
// time.Time. Storing an all-day event as a midnight instant is the classic calendar
// bug: midnight Paris on the 15th is 22:00 UTC on the 14th, so any code that later
// derives a date in UTC silently shifts the event a day earlier.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// NewDate returns the date y-m-d, normalizing out-of-range values the way time.Date
// does (so NewDate(2026, 1, 32) is the 1st of February).
func NewDate(y int, m time.Month, d int) Date {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return Date{t.Year(), t.Month(), t.Day()}
}

// DateIn returns the calendar date that instant t falls on in location loc.
// This is the only correct way to ask "what day is this event on for the family".
func DateIn(t time.Time, loc *time.Location) Date {
	t = t.In(loc)
	return Date{t.Year(), t.Month(), t.Day()}
}

// ParseDate parses the YYYY-MM-DD form used in storage and on the wire.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("parse date %q: %w", s, err)
	}
	return Date{t.Year(), t.Month(), t.Day()}, nil
}

// MustParseDate is ParseDate for tests and constants; it panics on bad input.
func MustParseDate(s string) Date {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// IsZero reports whether d is the zero Date, which is used to mean "unset".
func (d Date) IsZero() bool { return d == Date{} }

// In returns the first instant of d in loc. That is midnight on every day but the few
// a year when a zone that changes its clocks at midnight has no midnight to offer, and
// the day begins at 01:00 instead.
func (d Date) In(loc *time.Location) time.Time {
	return d.At(0, 0, loc)
}

// At returns the instant on d at the wall-clock time hour:min in loc.
func (d Date) At(hour, min int, loc *time.Location) time.Time {
	return d.at(hour, min, 0, loc)
}

// AtTimeOf returns the instant on d at the same wall-clock time of day that t reads in
// loc. It is how a series template is carried onto another date: the arithmetic happens
// in local wall-clock and converts afterwards, which is what keeps a 16:30 event at
// 16:30 on both sides of a daylight-saving change (CONVENTIONS §4).
func (d Date) AtTimeOf(t time.Time, loc *time.Location) time.Time {
	wall := t.In(loc)
	return d.at(wall.Hour(), wall.Minute(), wall.Second(), loc)
}

// at is the one place in this application where a wall clock in the family timezone
// becomes an instant, and it is a function of its own because one of the answers the
// standard library can give to that question is unusable here.
//
// A wall time inside the hour a spring-forward skips names no moment at all, so
// time.Date resolves it: it reads the fields as though they were UTC, applies the offset
// in force at that reading and corrects once. Which side of the jump that lands on
// follows the sign of the zone's offset rather than any policy anyone chose — 02:30
// becomes 03:30 in Europe/Paris and 01:30 in America/New_York — and that is the
// documented behaviour, pinned from the server's side and the browser's. It stays.
//
// What it may not do is answer with a different day, and it can, whenever the hour that
// went missing touches a date boundary. A zone that jumps at midnight has no 00:30 on
// the day it jumps, and correcting a negative offset backwards puts that occurrence at
// 23:30 the previous evening — so every date bucket in the application is wrong about it
// at once. The day the series named returns nothing at all, while the month grid, the
// digest and the reminder file it under a day nobody asked for. Every zone that still
// does this is west of Greenwich (America/Santiago and America/Havana are the populous
// ones, Atlantic/Azores the nearest), and Paris jumps at 02:00, which is the whole reason
// the household this was built for never saw it. The mirror case — a zone that abolishes
// the last hour of a day rather than the first — is rarer and mostly historical, and
// comes out here the same way.
//
// A broken wall time has exactly two readings, being the offsets either side of the
// jump, so where the first leaves the date behind the other one is the answer. Over
// every minute of every gap in the timezone database between 1970 and 2100 it lands on
// the date that was asked for, the sole exceptions being the six dates a zone deleted
// outright on crossing the date line (Pacific/Apia has no 30 December 2011), which no
// arithmetic can put an occurrence on because they never happened.
func (d Date) at(hour, min, sec int, loc *time.Location) time.Time {
	t := time.Date(d.Year, d.Month, d.Day, hour, min, sec, 0, loc)
	if DateIn(t, loc).Equal(d) {
		return t
	}
	// The offset in force at an answer that was corrected across the jump is the one on
	// the far side of it from the offset that produced the answer, which is what makes
	// applying it to the same fields the other reading rather than the same one again.
	_, off := t.Zone()
	alt := time.Date(d.Year, d.Month, d.Day, hour, min, sec, 0, time.UTC).
		Add(-time.Duration(off) * time.Second)
	if DateIn(alt, loc).Equal(d) {
		return alt
	}
	return t
}

// AddDays returns the date n days after d (n may be negative).
func (d Date) AddDays(n int) Date {
	t := d.In(time.UTC).AddDate(0, 0, n)
	return Date{t.Year(), t.Month(), t.Day()}
}

// AddMonths returns the date n months after d WITHOUT normalization: if the target
// month is too short the result is invalid and ok is false. Callers decide whether
// that means skip (monthly-on-the-31st) or clamp (yearly on 29 February); the two
// policies differ and both are wrong by default, so this type refuses to choose.
func (d Date) AddMonths(n int) (_ Date, ok bool) {
	total := int(d.Month) - 1 + n
	y := d.Year + floorDiv(total, 12)
	m := time.Month(floorMod(total, 12) + 1)
	if d.Day > DaysInMonth(y, m) {
		return Date{}, false
	}
	return Date{y, m, d.Day}, true
}

// Weekday returns the day of the week d falls on.
func (d Date) Weekday() time.Weekday { return d.In(time.UTC).Weekday() }

// Compare returns -1, 0 or +1 as d sorts before, with, or after o.
func (d Date) Compare(o Date) int {
	switch {
	case d.Year != o.Year:
		return sign(d.Year - o.Year)
	case d.Month != o.Month:
		return sign(int(d.Month) - int(o.Month))
	case d.Day != o.Day:
		return sign(d.Day - o.Day)
	}
	return 0
}

func (d Date) Before(o Date) bool { return d.Compare(o) < 0 }
func (d Date) After(o Date) bool  { return d.Compare(o) > 0 }
func (d Date) Equal(o Date) bool  { return d.Compare(o) == 0 }

// DaysUntil returns the number of days from d to o (negative if o is earlier).
//
// This counts civil days directly rather than subtracting two time.Times: a
// time.Duration saturates at about 292 years, and recurrence code legitimately
// measures spans longer than that when it jumps ahead to a query window.
func (d Date) DaysUntil(o Date) int {
	return o.daysFromCivil() - d.daysFromCivil()
}

// daysFromCivil returns the number of days since 1970-01-01 (Howard Hinnant's
// civil-date algorithm), valid for any year int can hold.
func (d Date) daysFromCivil() int {
	y, m := d.Year, int(d.Month)
	if m <= 2 {
		y--
	}
	era := floorDiv(y, 400)
	yoe := y - era*400 // [0, 399]
	mp := (m + 9) % 12 // March = 0
	doy := (153*mp+2)/5 + d.Day - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// MarshalJSON encodes the date as "YYYY-MM-DD", or null when unset.
func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + d.String() + `"`), nil
}

func (d *Date) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" || s == `""` {
		*d = Date{}
		return nil
	}
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return fmt.Errorf("date must be a JSON string, got %s", s)
	}
	parsed, err := ParseDate(s[1 : len(s)-1])
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Value implements driver.Valuer: dates are stored as TEXT so that a database file
// opened in 2040 is self-describing.
func (d Date) Value() (driver.Value, error) {
	if d.IsZero() {
		return nil, nil
	}
	return d.String(), nil
}

// Scan implements sql.Scanner.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
		return nil
	case string:
		parsed, err := ParseDate(v)
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	case []byte:
		parsed, err := ParseDate(string(v))
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	case time.Time:
		*d = Date{v.Year(), v.Month(), v.Day()}
		return nil
	default:
		return fmt.Errorf("cannot scan %T into Date", src)
	}
}

// DaysInMonth returns the number of days in the given month, honouring leap years.
func DaysInMonth(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// LastDayOfMonth returns the final calendar day of the month containing d.
func LastDayOfMonth(y int, m time.Month) Date {
	return Date{y, m, DaysInMonth(y, m)}
}

// NthWeekdayOfMonth returns the date of the nth given weekday in a month, where
// n is 1..5 counting forward or -1 counting back from the end ("last Tuesday").
// ok is false when the month has no such occurrence (e.g. a 5th Tuesday).
func NthWeekdayOfMonth(y int, m time.Month, wd time.Weekday, n int) (_ Date, ok bool) {
	if n == 0 {
		return Date{}, false
	}
	if n > 0 {
		first := Date{y, m, 1}
		offset := (int(wd) - int(first.Weekday()) + 7) % 7
		day := 1 + offset + (n-1)*7
		if day > DaysInMonth(y, m) {
			return Date{}, false
		}
		return Date{y, m, day}, true
	}
	last := LastDayOfMonth(y, m)
	offset := (int(last.Weekday()) - int(wd) + 7) % 7
	day := last.Day - offset + (n+1)*7
	if day < 1 {
		return Date{}, false
	}
	return Date{y, m, day}, true
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

func floorMod(a, b int) int {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}
