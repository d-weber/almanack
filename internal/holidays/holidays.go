// Package holidays computes public holidays.
//
// France is the only country implemented, in two sets — the national one and the
// Alsace-Moselle variant, which keeps two more days. Options.Countries chooses, and
// takes a list, so that adding a country is a pure function and a test rather than a
// change of shape, and so that a household observing more than one set can say so.
//
// A household somewhere else asks for no set at all and gets no computed holidays —
// the honest answer, and better than being shown somebody else's — while
// holiday_overrides still lets them name their own days.
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

	"almanack/internal/domain"
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
	// Countries are the sets to compute, unioned. Empty computes nothing: a caller
	// that forgets to say which shows a household no holidays rather than showing it
	// another country's.
	//
	// A list rather than one, because "which public holidays does this household
	// observe" genuinely has more than one answer — a family on a border, or one that
	// wants a regional set alongside the national one. Duplicates cost nothing: the
	// union is taken by date and name, so listing a set twice, or listing two that
	// overlap, produces each day once.
	Countries []Country
}

// Country is a set of public holidays: a country, by its ISO 3166-1 alpha-2 code, or a
// region within one whose observed days differ, spelled with the country first.
type Country string

const (
	// France is the set observed everywhere in France.
	France Country = "FR"
	// FranceAlsaceMoselle is France plus Good Friday and St Stephen's Day, which
	// remain public holidays in Bas-Rhin, Haut-Rhin and Moselle under the local law
	// kept in force when the departments returned to France in 1918.
	//
	// It contains France rather than sitting beside it, so that a household there
	// names one set and gets thirteen days. Naming both is the same thirteen — the
	// union deduplicates — which is what makes either spelling correct rather than one
	// of them a mistake.
	FranceAlsaceMoselle Country = "FR-ALSACE-MOSELLE"
)

// Implemented lists the sets this package computes, for the error a misconfigured
// server prints and for the documentation to stay honest against.
func Implemented() []Country { return []Country{France, FranceAlsaceMoselle} }

// Known reports whether c is a set this package can compute.
func Known(c Country) bool { return slices.Contains(Implemented(), c) }

// computed is the union of the requested sets for one year, before overrides.
//
// Deduplicated on date *and* name rather than on date alone, because a date can
// legitimately carry two holidays — Ascension falls on 1 May when Easter is 23 March —
// and collapsing those would hide one from a caller looking the date up by name.
func computed(year int, opts Options) []Entry {
	var out []Entry
	seen := map[string]bool{}
	for _, c := range opts.Countries {
		var set []Entry
		switch c {
		case France:
			set = french(year, false)
		case FranceAlsaceMoselle:
			set = french(year, true)
		default:
			// A set nothing implements. Configuration refuses one at startup, naming
			// what exists (internal/config), so reaching here means a caller built
			// Options by hand; computing nothing is the only answer that cannot be
			// wrong.
			continue
		}
		for _, e := range set {
			key := e.Date.String() + "\x00" + e.Key + "\x00" + e.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b Entry) int { return a.Date.Compare(b.Date) })
	return out
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

// French returns every public holiday observed across France in year, sorted by date.
//
// Two holidays can share a date: Ascension is Easter + 39 days, which falls on 1 May
// when Easter is 23 March (2008) and on 8 May when Easter is 30 March (1997). Both
// entries are returned — the day really is two holidays at once, and dropping one
// would hide it from a caller that looks the date up by name.
func French(year int) []Entry { return french(year, false) }

// FrenchAlsaceMoselle is French plus the two days Bas-Rhin, Haut-Rhin and Moselle keep.
func FrenchAlsaceMoselle(year int) []Entry { return french(year, true) }

func french(year int, alsaceMoselle bool) []Entry {
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
	if alsaceMoselle {
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
		for _, e := range computed(year, opts) {
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
