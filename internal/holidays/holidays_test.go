package holidays

import (
	"testing"
	"time"

	"agenda/internal/domain"
	"agenda/internal/i18n"
)

func TestEaster(t *testing.T) {
	// Published Gregorian Easter dates, spanning 1900–2100 so that a wrong century
	// term in the computus (the part table-driven implementations get wrong) fails.
	want := []string{
		"1900-04-15", "1901-04-07", "1918-03-31", "1943-04-25", "1954-04-18",
		"1961-04-02", "1981-04-19", "1997-03-30", "2000-04-23", "2005-03-27",
		"2008-03-23", "2011-04-24", "2016-03-27", "2020-04-12", "2021-04-04",
		"2022-04-17", "2023-04-09", "2024-03-31", "2025-04-20", "2026-04-05",
		"2027-03-28", "2028-04-16", "2029-04-01", "2030-04-21", "2038-04-25",
		"2049-04-18", "2050-04-10", "2078-04-03", "2100-03-28",
	}
	for _, s := range want {
		d := domain.MustParseDate(s)
		if got := Easter(d.Year); !got.Equal(d) {
			t.Errorf("Easter(%d) = %s, want %s", d.Year, got, d)
		}
	}
}

// TestEasterInvariants covers the years the table does not: Easter is always a Sunday
// between 22 March and 25 April, for every year the family will ever see.
func TestEasterInvariants(t *testing.T) {
	for year := 1583; year <= 2299; year++ {
		e := Easter(year)
		if e.Weekday() != time.Sunday {
			t.Fatalf("Easter(%d) = %s is a %v, want Sunday", year, e, e.Weekday())
		}
		if e.Before(domain.NewDate(year, time.March, 22)) || e.After(domain.NewDate(year, time.April, 25)) {
			t.Fatalf("Easter(%d) = %s is outside 22 March–25 April", year, e)
		}
	}
}

func dates(entries []Entry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.Key] = e.Date.String()
	}
	return m
}

func TestFrenchDerived(t *testing.T) {
	tests := []struct {
		year                                         int
		easterMonday, ascension, whitMonday, goodFri string
	}{
		{2026, "2026-04-06", "2026-05-14", "2026-05-25", "2026-04-03"},
		{2025, "2025-04-21", "2025-05-29", "2025-06-09", "2025-04-18"},
		{2024, "2024-04-01", "2024-05-09", "2024-05-20", "2024-03-29"},
	}
	for _, tc := range tests {
		got := dates(French(tc.year, Options{AlsaceMoselle: true}))
		for _, c := range []struct{ key, want string }{
			{KeyEasterMonday, tc.easterMonday},
			{KeyAscension, tc.ascension},
			{KeyWhitMonday, tc.whitMonday},
			{KeyGoodFriday, tc.goodFri},
		} {
			if got[c.key] != c.want {
				t.Errorf("%d %s = %s, want %s", tc.year, c.key, got[c.key], c.want)
			}
		}
	}
}

func TestFrenchFixedAndOrder(t *testing.T) {
	got := French(2026, Options{})
	if len(got) != 11 {
		t.Fatalf("got %d holidays, want 11: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Date.Before(got[i-1].Date) {
			t.Fatalf("not sorted: %s before %s", got[i].Date, got[i-1].Date)
		}
	}
	byKey := dates(got)
	fixed := map[string]string{
		KeyNewYear:     "2026-01-01",
		KeyLabourDay:   "2026-05-01",
		KeyVictory1945: "2026-05-08",
		KeyBastilleDay: "2026-07-14",
		KeyAssumption:  "2026-08-15",
		KeyAllSaints:   "2026-11-01",
		KeyArmistice:   "2026-11-11",
		KeyChristmas:   "2026-12-25",
	}
	for key, want := range fixed {
		if byKey[key] != want {
			t.Errorf("%s = %s, want %s", key, byKey[key], want)
		}
	}
	for _, e := range got {
		if e.Name != "" {
			t.Errorf("computed entry %s carries a Name %q; Name is for overrides only", e.Key, e.Name)
		}
	}
}

func TestAlsaceMoselle(t *testing.T) {
	off := dates(French(2026, Options{}))
	if _, ok := off[KeyGoodFriday]; ok {
		t.Error("Good Friday present without the Alsace-Moselle option")
	}
	if _, ok := off[KeyStStephen]; ok {
		t.Error("St Stephen's Day present without the Alsace-Moselle option")
	}
	on := French(2026, Options{AlsaceMoselle: true})
	if len(on) != 13 {
		t.Fatalf("got %d holidays with Alsace-Moselle, want 13", len(on))
	}
	byKey := dates(on)
	if byKey[KeyGoodFriday] != "2026-04-03" {
		t.Errorf("Good Friday = %s, want 2026-04-03", byKey[KeyGoodFriday])
	}
	if byKey[KeyStStephen] != "2026-12-26" {
		t.Errorf("St Stephen = %s, want 2026-12-26", byKey[KeyStStephen])
	}
}

// TestSharedDate pins the years where Ascension lands on a fixed holiday: both
// entries must survive, because the day is genuinely two holidays.
func TestSharedDate(t *testing.T) {
	for _, tc := range []struct{ year, count int }{{2008, 11}, {1997, 11}} {
		got := French(tc.year, Options{})
		if len(got) != tc.count {
			t.Fatalf("%d: got %d entries, want %d", tc.year, len(got), tc.count)
		}
	}
	byKey := dates(French(2008, Options{}))
	if byKey[KeyAscension] != "2008-05-01" || byKey[KeyLabourDay] != "2008-05-01" {
		t.Errorf("2008: ascension=%s labourDay=%s, want both 2008-05-01",
			byKey[KeyAscension], byKey[KeyLabourDay])
	}
}

func ptr(s string) *string { return &s }

func TestBetween(t *testing.T) {
	all := Between(domain.MustParseDate("2026-01-01"), domain.MustParseDate("2026-12-31"), Options{}, nil)
	if len(all) != 11 {
		t.Fatalf("full year: got %d, want 11", len(all))
	}

	// Window boundaries are inclusive at both ends.
	w := Between(domain.MustParseDate("2026-07-14"), domain.MustParseDate("2026-08-15"), Options{}, nil)
	if len(w) != 2 || w[0].Key != KeyBastilleDay || w[1].Key != KeyAssumption {
		t.Errorf("inclusive window = %v, want Bastille Day and Assumption", w)
	}

	if got := Between(domain.MustParseDate("2026-02-01"), domain.MustParseDate("2026-02-28"), Options{}, nil); len(got) != 0 {
		t.Errorf("February = %v, want none", got)
	}
	if got := Between(domain.MustParseDate("2026-05-01"), domain.MustParseDate("2026-04-01"), Options{}, nil); got != nil {
		t.Errorf("reversed window = %v, want nil", got)
	}
}

func TestBetweenAcrossYearBoundary(t *testing.T) {
	got := Between(domain.MustParseDate("2025-12-24"), domain.MustParseDate("2026-01-02"), Options{}, nil)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(got), got)
	}
	if got[0].Date.String() != "2025-12-25" || got[0].Key != KeyChristmas {
		t.Errorf("first = %v, want Christmas 2025", got[0])
	}
	if got[1].Date.String() != "2026-01-01" || got[1].Key != KeyNewYear {
		t.Errorf("second = %v, want New Year 2026", got[1])
	}

	// Alsace-Moselle adds Boxing Day on the 2025 side of the boundary.
	got = Between(domain.MustParseDate("2025-12-24"), domain.MustParseDate("2026-01-02"), Options{AlsaceMoselle: true}, nil)
	if len(got) != 3 || got[1].Key != KeyStStephen || got[1].Date.String() != "2025-12-26" {
		t.Fatalf("Alsace-Moselle across the boundary = %v", got)
	}
}

func TestBetweenOverrides(t *testing.T) {
	from, to := domain.MustParseDate("2026-01-01"), domain.MustParseDate("2026-12-31")

	t.Run("suppress", func(t *testing.T) {
		var suppressed *string
		got := Between(from, to, Options{}, map[domain.Date]*string{
			domain.MustParseDate("2026-05-08"): suppressed,
		})
		if len(got) != 10 {
			t.Fatalf("got %d entries, want 10", len(got))
		}
		for _, e := range got {
			if e.Date.String() == "2026-05-08" {
				t.Errorf("8 May survived suppression: %v", e)
			}
		}
	})

	t.Run("rename", func(t *testing.T) {
		got := Between(from, to, Options{}, map[domain.Date]*string{
			domain.MustParseDate("2026-07-14"): ptr("Fête de la République"),
		})
		if len(got) != 11 {
			t.Fatalf("got %d entries, want 11", len(got))
		}
		var found bool
		for _, e := range got {
			if e.Date.String() != "2026-07-14" {
				continue
			}
			found = true
			if e.Name != "Fête de la République" {
				t.Errorf("Name = %q, want the override", e.Name)
			}
			if e.Key != KeyBastilleDay {
				t.Errorf("Key = %q, want it preserved as %q", e.Key, KeyBastilleDay)
			}
			if got := e.Display(func(string) string { return "Fête nationale" }); got != "Fête de la République" {
				t.Errorf("Display() = %q, want the override to win over the catalog", got)
			}
		}
		if !found {
			t.Error("14 July missing after rename")
		}
	})

	t.Run("add", func(t *testing.T) {
		got := Between(from, to, Options{}, map[domain.Date]*string{
			domain.MustParseDate("2026-06-18"): ptr("Appel du 18 Juin"),
		})
		if len(got) != 12 {
			t.Fatalf("got %d entries, want 12", len(got))
		}
		var e Entry
		for _, c := range got {
			if c.Date.String() == "2026-06-18" {
				e = c
			}
		}
		if e.Name != "Appel du 18 Juin" || e.Key != "" {
			t.Errorf("added entry = %+v, want name-only", e)
		}
		if got := e.Display(func(k string) string { return "translated:" + k }); got != "Appel du 18 Juin" {
			t.Errorf("Display() = %q", got)
		}
		for i := 1; i < len(got); i++ {
			if got[i].Date.Before(got[i-1].Date) {
				t.Fatalf("added entry broke the ordering: %v", got)
			}
		}
	})

	t.Run("outside the window is ignored", func(t *testing.T) {
		got := Between(from, to, Options{}, map[domain.Date]*string{
			domain.MustParseDate("2027-06-18"): ptr("next year"),
			domain.MustParseDate("2025-06-18"): ptr("last year"),
		})
		if len(got) != 11 {
			t.Fatalf("got %d entries, want 11: %v", len(got), got)
		}
	})

	t.Run("shared date collapses to one entry", func(t *testing.T) {
		// 1 May 2008 is both Labour Day and Ascension; naming it means the family
		// decided what that day is called.
		got := Between(domain.MustParseDate("2008-04-30"), domain.MustParseDate("2008-05-02"), Options{},
			map[domain.Date]*string{domain.MustParseDate("2008-05-01"): ptr("Fête du Travail et Ascension")})
		if len(got) != 1 {
			t.Fatalf("got %d entries, want 1: %v", len(got), got)
		}
		if got[0].Name != "Fête du Travail et Ascension" {
			t.Errorf("Name = %q", got[0].Name)
		}
	})
}

// TestKeysExistInCatalogs is the guard against a holiday rendering as a raw key in a
// push notification: every key this package can emit must be translatable in every
// language the app ships.
func TestKeysExistInCatalogs(t *testing.T) {
	cat := i18n.MustLoad()
	for _, e := range French(2026, Options{AlsaceMoselle: true}) {
		for _, lang := range []domain.Language{domain.LangFR, domain.LangEN} {
			if got := cat.T(lang, e.Key, nil); got == e.Key {
				t.Errorf("%s has no %s translation", e.Key, lang)
			}
		}
	}
}
