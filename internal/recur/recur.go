// Package recur turns a recurrence rule into the calendar dates it occurs on.
//
// Everything here works in domain.Date: calendar dates, never instants. Time of day,
// the family timezone, and the conversion to UTC belong to the caller. A recurrence
// answers "which days", not "which moments", and that is precisely what keeps DST out
// of this package — there is no such thing as a 23-hour day when a day is a label
// rather than a span. Nothing here imports a location or reads a clock, and every
// function is pure.
//
// The policies below are normative (docs/architecture.md, "The recurrence policy table"). They were decided
// deliberately, and two of them deliberately disagree with each other:
//
//   - Monthly on a day a short month lacks (the 29th, 30th or 31st): the month is
//     SKIPPED, never clamped — RFC 5545 and Google Calendar both do this. End-of-month
//     intent has its own mode (MonthLastDay) rather than being guessed at.
//   - Yearly on 29 February: CLAMPED to 28 February in common years, the opposite
//     choice, because a leap-day birthday must not vanish three years in four while a
//     bill due on the 31st genuinely has no February date. A birthday and a monthly
//     bill have different intent; one rule cannot serve both.
//   - Until is inclusive.
//   - The interval anchor is DTStart, for every frequency.
//   - Weekly parity counts Monday-started weeks from the week containing DTStart.
//     WKST is Monday, always, and is not configurable: the per-user week_start
//     preference is display only, and if it reached this package two members of the
//     same family would see different occurrences of the same event.
//   - No occurrence before DTStart or after Until is ever emitted.
//
// Fields that carry no meaning for a frequency are ignored rather than rejected (the
// monthly mode fields on a weekly rule, for instance); see Validate for what is
// rejected.
package recur

import (
	"slices"
	"time"

	"agenda/internal/domain"
)

// searchHorizonYears bounds the search Next performs when a rule has no Until. Some
// rules genuinely never occur again — "every 24 months on the 29th" anchored to an
// odd-numbered February never lands on a leap year — and a calendar has to be able to
// answer "no" to those rather than loop forever. A century is far beyond the horizon
// at which a family calendar means anything.
const searchHorizonYears = 100

// Expand returns every occurrence of r in [from, to], inclusive at both ends, in
// ascending order and without duplicates. It returns nil when the window holds no
// occurrence, or when r is malformed (see Validate) — an unusable rule expands to
// nothing rather than to a guess.
//
// Cost is proportional to the number of occurrences inside the window, not to the age
// of the series: expansion computes the first period that can intersect the window and
// starts there. A daily series running since 2015, queried for one month in 2040, does
// about thirty days of work.
func Expand(r domain.Recurrence, from, to domain.Date) []domain.Date {
	lo, hi, ok := window(r, from, to)
	if !ok {
		return nil
	}
	var out []domain.Date
	each(r, lo, hi, func(d domain.Date) bool {
		out = append(out, d)
		return true
	})
	return out
}

// Next returns the first occurrence strictly after the given date, honouring Until.
// ok is false when the series has ended, when r is malformed, or when the rule has no
// occurrence within searchHorizonYears of the query.
func Next(r domain.Recurrence, after domain.Date) (domain.Date, bool) {
	from := after.AddDays(1) // strictly after
	if from.Before(r.DTStart) {
		from = r.DTStart
	}
	to := domain.Date{Year: from.Year + searchHorizonYears, Month: time.December, Day: 31}
	lo, hi, ok := window(r, from, to)
	if !ok {
		return domain.Date{}, false
	}
	var found domain.Date
	got := false
	each(r, lo, hi, func(d domain.Date) bool {
		found, got = d, true
		return false
	})
	return found, got
}

// Occurs reports whether d is an occurrence of r. It is defined in terms of the same
// expansion Expand uses, so the two can never disagree.
func Occurs(r domain.Recurrence, d domain.Date) bool {
	lo, hi, ok := window(r, d, d)
	if !ok {
		return false
	}
	found := false
	each(r, lo, hi, func(domain.Date) bool {
		found = true
		return false
	})
	return found
}

// window intersects the caller's [from, to] with the series' own bounds. ok is false
// when the rule is unusable or the intersection is empty, which is the single place
// "never before DTStart, never after Until" is enforced.
func window(r domain.Recurrence, from, to domain.Date) (lo, hi domain.Date, ok bool) {
	if Validate(r) != nil {
		return domain.Date{}, domain.Date{}, false
	}
	lo, hi = from, to
	if lo.Before(r.DTStart) {
		lo = r.DTStart
	}
	if r.Until != nil && r.Until.Before(hi) {
		hi = *r.Until // inclusive: an occurrence exactly on Until is kept
	}
	return lo, hi, !lo.After(hi)
}

// each calls yield with every occurrence in [lo, hi] in ascending order, stopping
// early if yield returns false. lo and hi must already be clamped by window.
func each(r domain.Recurrence, lo, hi domain.Date, yield func(domain.Date) bool) {
	switch r.Freq {
	case domain.FreqDaily:
		eachDaily(r, lo, hi, yield)
	case domain.FreqWeekly:
		eachWeekly(r, lo, hi, yield)
	case domain.FreqMonthly:
		eachMonthly(r, lo, hi, yield)
	case domain.FreqYearly:
		eachYearly(r, lo, hi, yield)
	}
}

func eachDaily(r domain.Recurrence, lo, hi domain.Date, yield func(domain.Date) bool) {
	// Jump straight to the first multiple of the interval that reaches lo instead of
	// walking day by day from DTStart.
	skip := 0
	if gap := daysBetween(r.DTStart, lo); gap > 0 {
		skip = ceilDiv(gap, r.Interval)
	}
	for d := r.DTStart.AddDays(skip * r.Interval); !d.After(hi); d = d.AddDays(r.Interval) {
		if !yield(d) {
			return
		}
	}
}

func eachWeekly(r domain.Recurrence, lo, hi domain.Date, yield func(domain.Date) bool) {
	offsets := weeklyOffsets(r)
	anchor := mondayOf(r.DTStart) // WKST = Monday, always
	skip := 0
	if gap := daysBetween(anchor, mondayOf(lo)) / 7; gap > 0 {
		skip = ceilDiv(gap, r.Interval)
	}
	step := r.Interval * 7
	for mon := anchor.AddDays(skip * step); !mon.After(hi); mon = mon.AddDays(step) {
		for _, off := range offsets {
			d := mon.AddDays(off)
			// A qualifying week may straddle either end of the window — DTStart's own
			// week in particular usually starts before DTStart.
			if d.Before(lo) {
				continue
			}
			if d.After(hi) {
				break
			}
			if !yield(d) {
				return
			}
		}
	}
}

func eachMonthly(r domain.Recurrence, lo, hi domain.Date, yield func(domain.Date) bool) {
	skip := 0
	if gap := (lo.Year-r.DTStart.Year)*12 + int(lo.Month) - int(r.DTStart.Month); gap > 0 {
		skip = ceilDiv(gap, r.Interval)
	}
	// Reused across months so a long window does not allocate per month.
	buf := make([]domain.Date, 0, 7)
	for n := skip; ; n++ {
		y, m := shiftMonth(r.DTStart.Year, r.DTStart.Month, n*r.Interval)
		if (domain.Date{Year: y, Month: m, Day: 1}).After(hi) {
			return
		}
		for _, d := range monthDates(r, y, m, buf[:0]) {
			if d.Before(lo) {
				continue
			}
			if d.After(hi) {
				break
			}
			if !yield(d) {
				return
			}
		}
	}
}

// monthDates returns the dates the monthly rule produces in the given month, ascending
// and deduplicated. The slice may be empty: a month that cannot satisfy the rule is
// skipped, never approximated. That covers both a February asked for the 31st and a
// month with no fifth Tuesday.
func monthDates(r domain.Recurrence, y int, m time.Month, dst []domain.Date) []domain.Date {
	switch {
	case r.MonthLastDay:
		dst = append(dst, domain.LastDayOfMonth(y, m))

	case r.ByMonthday != nil:
		// Policy: skip, do not clamp. Clamping would move a bill due on the 31st to
		// the 28th of February, which is a different bill.
		if *r.ByMonthday <= domain.DaysInMonth(y, m) {
			dst = append(dst, domain.Date{Year: y, Month: m, Day: *r.ByMonthday})
		}

	case r.WeekOrdinal != nil && len(r.ByWeekday) > 0:
		for _, wd := range r.ByWeekday {
			if d, ok := domain.NthWeekdayOfMonth(y, m, wd, *r.WeekOrdinal); ok {
				dst = append(dst, d)
			}
		}
		slices.SortFunc(dst, func(a, b domain.Date) int { return a.Compare(b) })
		dst = slices.Compact(dst)
	}
	return dst
}

func eachYearly(r domain.Recurrence, lo, hi domain.Date, yield func(domain.Date) bool) {
	skip := 0
	if gap := lo.Year - r.DTStart.Year; gap > 0 {
		skip = ceilDiv(gap, r.Interval)
	}
	for n := skip; ; n++ {
		d := yearlyDate(r.DTStart.Year+n*r.Interval, r.DTStart.Month, r.DTStart.Day)
		if d.After(hi) {
			return
		}
		if d.Before(lo) {
			continue
		}
		if !yield(d) {
			return
		}
	}
}

// yearlyDate applies the 29 February clamp: in a common year a leap-day series falls on
// 28 February. This is the only case where the day can fail to exist, because DTStart's
// day already fits its own month and no other month changes length.
func yearlyDate(y int, m time.Month, day int) domain.Date {
	if day > domain.DaysInMonth(y, m) {
		return domain.LastDayOfMonth(y, m)
	}
	return domain.Date{Year: y, Month: m, Day: day}
}

// weeklyOffsets returns the rule's weekdays as ascending Monday-based offsets (0..6),
// deduplicated. An empty ByWeekday means DTStart's own weekday.
func weeklyOffsets(r domain.Recurrence) []int {
	if len(r.ByWeekday) == 0 {
		return []int{mondayOffset(r.DTStart.Weekday())}
	}
	var seen [7]bool
	offsets := make([]int, 0, len(r.ByWeekday))
	for _, wd := range r.ByWeekday {
		off := mondayOffset(wd)
		if !seen[off] {
			seen[off] = true
			offsets = append(offsets, off)
		}
	}
	slices.Sort(offsets)
	return offsets
}

// mondayOffset maps a weekday to its position in a Monday-started week.
func mondayOffset(wd time.Weekday) int { return (int(wd) + 6) % 7 }

// mondayOf returns the Monday of the week containing d.
func mondayOf(d domain.Date) domain.Date { return d.AddDays(-mondayOffset(d.Weekday())) }

// shiftMonth moves (y, m) by n months.
func shiftMonth(y int, m time.Month, n int) (int, time.Month) {
	// The 1st exists in every month, so AddMonths cannot fail here.
	d, _ := domain.Date{Year: y, Month: m, Day: 1}.AddMonths(n)
	return d.Year, d.Month
}

// daysBetween returns the number of days from a to b, negative if b is earlier.
//
// It does its own civil arithmetic instead of calling Date.DaysUntil because that
// helper measures with a time.Duration, which saturates past roughly 292 years — and
// the whole point of this package's jumping is to cross arbitrary spans in one step.
func daysBetween(a, b domain.Date) int { return dayNumber(b) - dayNumber(a) }

// dayNumber is the count of days from 1970-01-01 to d (Hinnant's days_from_civil: shift
// the year to start in March so the leap day is last and the month lengths repeat).
func dayNumber(d domain.Date) int {
	y, m := d.Year, int(d.Month)
	if m <= 2 {
		y--
	}
	era := floorDiv(y, 400)
	yoe := y - era*400                     // year of era, 0..399
	mp := (m + 9) % 12                     // March = 0
	doy := (153*mp+2)/5 + d.Day - 1        // day of the March-based year, 0..365
	doe := yoe*365 + yoe/4 - yoe/100 + doy // day of era, 0..146096
	return era*146097 + doe - 719468
}

func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// ceilDiv rounds a/b up. Both arguments are positive at every call site.
func ceilDiv(a, b int) int { return (a + b - 1) / b }
