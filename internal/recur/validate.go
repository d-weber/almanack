package recur

import (
	"fmt"
	"time"

	"almanack/internal/domain"
)

// Validate reports why r cannot be expanded, or nil when it is well formed. Every
// error wraps domain.ErrInvalid and names the offending field, so a handler can map it
// to 400 and still show the user which control is wrong.
//
// It rejects: an unknown frequency, an interval below 1, a missing DTStart, a weekday
// outside 0..6, an Until before DTStart, and — for monthly rules — anything other than
// exactly one of the three modes (by_monthday, by_weekday + week_ordinal,
// month_last_day) together with an out-of-range monthday or ordinal.
//
// It does not reject fields that are simply meaningless for the frequency, such as a
// monthday left over on a weekly rule; expansion ignores those.
func Validate(r domain.Recurrence) error {
	if !r.Freq.Valid() {
		return fmt.Errorf("%w: unknown frequency %q", domain.ErrInvalid, string(r.Freq))
	}
	if r.Interval < 1 {
		return fmt.Errorf("%w: interval must be at least 1, got %d", domain.ErrInvalid, r.Interval)
	}
	if r.DTStart.IsZero() {
		return fmt.Errorf("%w: dtstart is required as the interval anchor", domain.ErrInvalid)
	}
	for _, wd := range r.ByWeekday {
		if wd < time.Sunday || wd > time.Saturday {
			return fmt.Errorf("%w: weekday %d out of range (0=Sunday..6=Saturday)", domain.ErrInvalid, int(wd))
		}
	}
	if r.Until != nil && r.Until.Before(r.DTStart) {
		return fmt.Errorf("%w: until %s is before dtstart %s", domain.ErrInvalid, r.Until, r.DTStart)
	}
	if r.Freq == domain.FreqMonthly {
		return validateMonthly(r)
	}
	return nil
}

func validateMonthly(r domain.Recurrence) error {
	// The two halves of the nth-weekday mode are useless apart: an ordinal with no
	// weekday says "the 2nd what?", and a weekday with no ordinal would mean "every
	// Tuesday of the month", which this engine deliberately does not support (that is
	// a weekly rule).
	if r.WeekOrdinal != nil && len(r.ByWeekday) == 0 {
		return fmt.Errorf("%w: monthly week_ordinal needs a by_weekday", domain.ErrInvalid)
	}
	if len(r.ByWeekday) > 0 && r.WeekOrdinal == nil {
		return fmt.Errorf("%w: monthly by_weekday needs a week_ordinal (use a weekly rule for every week)", domain.ErrInvalid)
	}

	modes := 0
	for _, set := range []bool{r.ByMonthday != nil, r.WeekOrdinal != nil, r.MonthLastDay} {
		if set {
			modes++
		}
	}
	if modes != 1 {
		return fmt.Errorf("%w: monthly needs exactly one of by_monthday, by_weekday+week_ordinal or month_last_day, got %d", domain.ErrInvalid, modes)
	}

	if r.ByMonthday != nil && (*r.ByMonthday < 1 || *r.ByMonthday > 31) {
		return fmt.Errorf("%w: by_monthday %d out of range 1..31", domain.ErrInvalid, *r.ByMonthday)
	}
	if r.WeekOrdinal != nil {
		if o := *r.WeekOrdinal; o != -1 && (o < 1 || o > 5) {
			return fmt.Errorf("%w: week_ordinal %d must be 1..5 or -1 for last", domain.ErrInvalid, o)
		}
	}
	return nil
}
