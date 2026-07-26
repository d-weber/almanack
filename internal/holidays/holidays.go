// Package holidays computes French public holidays.
//
// There is no bundled dataset and no yearly refresh: eight of the eleven dates are
// fixed, and the rest derive from Easter, which is computed. A data file would be a
// maintenance obligation with an expiry date, and this app has to still be right in
// 2040 without anyone touching it. When the law changes — and it does, roughly once a
// generation — the family admin adds or suppresses a date through holiday_overrides,
// which Between applies here.
package holidays

import (
	"slices"
	"time"

	"agenda/internal/domain"
)

// Catalog keys for the computed holidays. The names themselves live in
// internal/i18n/locales/{fr,en}.json so that the browser and the server render the
// same string; this package never carries display text.
const (
	KeyNewYear      = "holiday.newYear"
	KeyEasterMonday = "holiday.easterMonday"
	KeyLabourDay    = "holiday.labourDay"
	KeyVictory1945  = "holiday.victory1945"
	KeyAscension    = "holiday.ascension"
	KeyWhitMonday   = "holiday.whitMonday"
	KeyBastilleDay  = "holiday.bastilleDay"
	KeyAssumption   = "holiday.assumption"
	KeyAllSaints    = "holiday.allSaints"
	KeyArmistice    = "holiday.armistice"
	KeyChristmas    = "holiday.christmas"
	KeyGoodFriday   = "holiday.goodFriday"
	KeyStStephen    = "holiday.stStephen"
)

// Entry is one public holiday on one date.
type Entry struct {
	Date domain.Date `json:"date"`
	// Key is the i18n catalog key naming the holiday. It is empty only for a
	// family-defined holiday that has no computed counterpart.
	Key string `json:"key,omitempty"`
	// Name is a family-defined name and is non-empty only for an override; when set
	// it wins over Key, because the family had a reason to write it down.
	Name string `json:"name,omitempty"`
}

// Display returns the name to show, resolving Key through translate unless the family
// named this day themselves. Callers hold the rule in one place instead of each
// re-deciding whether Name or Key wins.
func (e Entry) Display(translate func(key string) string) string {
	if e.Name != "" {
		return e.Name
	}
	if translate == nil {
		return e.Key
	}
	return translate(e.Key)
}

// Options selects regional variants.
type Options struct {
	// AlsaceMoselle adds Good Friday and St Stephen's Day, which remain public
	// holidays in Bas-Rhin, Haut-Rhin and Moselle under the local law kept in force
	// when the departments returned to France in 1918.
	AlsaceMoselle bool
}

// Easter returns Gregorian Easter Sunday for year, by the Meeus/Butcher "anonymous
// Gregorian" computus. It is exact for every Gregorian year (1583 onwards) with no
// table and no upper bound, which is the whole reason holidays need no data file.
func Easter(year int) domain.Date {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	n := h + l - 7*m + 114
	return domain.Date{Year: year, Month: time.Month(n / 31), Day: n%31 + 1}
}

// French returns every French public holiday in year, sorted by date.
//
// Two holidays can share a date: Ascension is Easter + 39 days, which falls on 1 May
// when Easter is 23 March (2008) and on 8 May when Easter is 30 March (1997). Both
// entries are returned — the day really is two holidays at once, and dropping one
// would hide it from a caller that looks the date up by name.
func French(year int, opts Options) []Entry {
	easter := Easter(year)
	out := []Entry{
		{Date: domain.NewDate(year, time.January, 1), Key: KeyNewYear},
		{Date: domain.NewDate(year, time.May, 1), Key: KeyLabourDay},
		{Date: domain.NewDate(year, time.May, 8), Key: KeyVictory1945},
		{Date: domain.NewDate(year, time.July, 14), Key: KeyBastilleDay},
		{Date: domain.NewDate(year, time.August, 15), Key: KeyAssumption},
		{Date: domain.NewDate(year, time.November, 1), Key: KeyAllSaints},
		{Date: domain.NewDate(year, time.November, 11), Key: KeyArmistice},
		{Date: domain.NewDate(year, time.December, 25), Key: KeyChristmas},
		{Date: easter.AddDays(1), Key: KeyEasterMonday},
		{Date: easter.AddDays(39), Key: KeyAscension},
		{Date: easter.AddDays(50), Key: KeyWhitMonday},
	}
	if opts.AlsaceMoselle {
		out = append(out,
			Entry{Date: easter.AddDays(-2), Key: KeyGoodFriday},
			Entry{Date: domain.NewDate(year, time.December, 26), Key: KeyStStephen},
		)
	}
	slices.SortStableFunc(out, func(a, b Entry) int { return a.Date.Compare(b.Date) })
	return out
}

// Between returns every holiday in the inclusive window from..to, across whatever
// years it spans, with the family's overrides applied and sorted by date.
//
// An override is keyed by date: a nil value suppresses whatever is computed for that
// day (the law changed), and a non-nil value names it. Naming a day collapses it to a
// single entry — once the family has decided what a date is called, returning a second
// entry for it would only produce a duplicate line in the UI. The computed Key is kept
// alongside the name so callers can still tell which holiday was renamed.
func Between(from, to domain.Date, opts Options, overrides map[domain.Date]*string) []Entry {
	if from.After(to) {
		return nil
	}
	var out []Entry
	named := make(map[domain.Date]bool)
	for year := from.Year; year <= to.Year; year++ {
		for _, e := range French(year, opts) {
			if e.Date.Before(from) || e.Date.After(to) {
				continue
			}
			name, ok := overrides[e.Date]
			if !ok {
				out = append(out, e)
				continue
			}
			if name == nil {
				continue // suppressed
			}
			if named[e.Date] {
				continue
			}
			named[e.Date] = true
			e.Name = *name
			out = append(out, e)
		}
	}
	for date, name := range overrides {
		if name == nil || named[date] {
			continue
		}
		if date.Before(from) || date.After(to) {
			continue
		}
		out = append(out, Entry{Date: date, Name: *name})
	}
	// Dates are unique across the two loops above, so ordering by date alone is
	// deterministic despite the map iteration.
	slices.SortStableFunc(out, func(a, b Entry) int { return a.Date.Compare(b.Date) })
	return out
}
