package i18n

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	// The tests pin behaviour in Europe/Paris, and a build machine without a system
	// zoneinfo database would otherwise fail for reasons unrelated to this package.
	_ "time/tzdata"

	"agenda/internal/domain"
)

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("load Europe/Paris: %v", err)
	}
	return loc
}

func keys(table map[string]string) []string {
	out := make([]string, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestCatalogsHaveTheSameKeys is the translation-gap guard: every future string added
// to one catalog and forgotten in the other fails here, by name.
func TestCatalogsHaveTheSameKeys(t *testing.T) {
	c := MustLoad()
	fr, en := c.tables[domain.LangFR], c.tables[domain.LangEN]
	if fr == nil || en == nil {
		t.Fatal("both fr and en catalogs must exist")
	}
	var missingEN, missingFR []string
	for _, k := range keys(fr) {
		if _, ok := en[k]; !ok {
			missingEN = append(missingEN, k)
		}
	}
	for _, k := range keys(en) {
		if _, ok := fr[k]; !ok {
			missingFR = append(missingFR, k)
		}
	}
	if len(missingEN) > 0 {
		t.Errorf("%d key(s) in fr.json but not en.json:\n  %s", len(missingEN), strings.Join(missingEN, "\n  "))
	}
	if len(missingFR) > 0 {
		t.Errorf("%d key(s) in en.json but not fr.json:\n  %s", len(missingFR), strings.Join(missingFR, "\n  "))
	}
}

var placeholderRE = regexp.MustCompile(`\{[a-zA-Z0-9_]+\}`)

// TestCatalogsHaveTheSamePlaceholders catches the other half of a translation gap: a
// key present in both catalogs whose translation dropped or renamed a {placeholder},
// which would ship a notification with a literal brace in it.
func TestCatalogsHaveTheSamePlaceholders(t *testing.T) {
	c := MustLoad()
	fr, en := c.tables[domain.LangFR], c.tables[domain.LangEN]
	for _, k := range keys(fr) {
		v, ok := en[k]
		if !ok {
			continue // reported by TestCatalogsHaveTheSameKeys
		}
		a, b := placeholderRE.FindAllString(fr[k], -1), placeholderRE.FindAllString(v, -1)
		slices.Sort(a)
		slices.Sort(b)
		a, b = slices.Compact(a), slices.Compact(b)
		if !slices.Equal(a, b) {
			t.Errorf("%s: fr has %v, en has %v", k, a, b)
		}
	}
}

func TestT(t *testing.T) {
	c := MustLoad()
	tests := []struct {
		name   string
		lang   domain.Language
		key    string
		params map[string]string
		want   string
	}{
		{"plain", domain.LangFR, "action.save", nil, "Enregistrer"},
		{"plain en", domain.LangEN, "action.save", nil, "Save"},
		{"substitution", domain.LangFR, "event.delete.confirm",
			map[string]string{"title": "Dentiste"}, "Supprimer « Dentiste » ?"},
		{"missing param stays visible", domain.LangFR, "event.delete.confirm", nil,
			"Supprimer « {title} » ?"},
		{"missing param among others", domain.LangEN, "notify.reminder.tomorrow",
			map[string]string{"title": "Dentist"}, "Tomorrow at {time}: Dentist"},
		{"extra params ignored", domain.LangEN, "action.save",
			map[string]string{"nope": "x"}, "Save"},
		{"missing key returns the key", domain.LangFR, "no.such.key", nil, "no.such.key"},
		{"missing key with params returns the key", domain.LangEN, "no.such.key",
			map[string]string{"a": "b"}, "no.such.key"},
		{"unknown language falls back to French", domain.Language("de"), "action.save", nil, "Enregistrer"},
		{"empty language falls back to French", domain.Language(""), "nav.calendar", nil, "Calendrier"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.T(tc.lang, tc.key, tc.params); got != tc.want {
				t.Errorf("T(%q, %q) = %q, want %q", tc.lang, tc.key, got, tc.want)
			}
		})
	}
}

func TestSubstitute(t *testing.T) {
	tests := []struct {
		name, in string
		params   map[string]string
		want     string
	}{
		{"repeated placeholder", "{n} of {n}", map[string]string{"n": "3"}, "3 of 3"},
		{"two placeholders", "{a}-{b}", map[string]string{"a": "1", "b": "2"}, "1-2"},
		{"missing one of two", "{a}-{b}", map[string]string{"a": "1"}, "1-{b}"},
		{"no params at all", "{a}", nil, "{a}"},
		{"unclosed brace", "{a", map[string]string{"a": "1"}, "{a"},
		{"empty name", "{}", map[string]string{"": "x"}, "x"},
		{"value containing a placeholder is not rescanned", "{a}",
			map[string]string{"a": "{b}", "b": "boom"}, "{b}"},
		{"nothing to do", "plain text", map[string]string{"a": "1"}, "plain text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := substitute(tc.in, tc.params); got != tc.want {
				t.Errorf("substitute(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatDate(t *testing.T) {
	c := MustLoad()
	d := domain.MustParseDate("2026-08-04") // a Tuesday
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"fr", c.FormatDate(domain.LangFR, d), "mardi 4 août"},
		{"en", c.FormatDate(domain.LangEN, d), "Tuesday 4 August"},
		{"fr full", c.FormatDateFull(domain.LangFR, d), "mardi 4 août 2026"},
		{"en full", c.FormatDateFull(domain.LangEN, d), "Tuesday 4 August 2026"},
		{"fr short", c.FormatDateShort(domain.LangFR, d), "4 août"},
		{"en short", c.FormatDateShort(domain.LangEN, d), "4 Aug"},
		{"fr leap day", c.FormatDateFull(domain.LangFR, domain.MustParseDate("2024-02-29")),
			"jeudi 29 février 2024"},
		{"fr new year", c.FormatDate(domain.LangFR, domain.MustParseDate("2026-01-01")),
			"jeudi 1 janvier"},
		{"unknown language falls back to French", c.FormatDate(domain.Language("de"), d), "mardi 4 août"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestFormatTime(t *testing.T) {
	loc := paris(t)
	c := MustLoad()
	// 12:30 UTC is 14:30 in Paris in August: the conversion, not the formatting, is
	// what makes a reminder say the right thing.
	summer := time.Date(2026, time.August, 4, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name     string
		t        time.Time
		loc      *time.Location
		format24 bool
		want     string
	}{
		{"24h Paris summer", summer, loc, true, "14:30"},
		{"12h Paris summer", summer, loc, false, "2:30 PM"},
		{"24h UTC", summer, time.UTC, true, "12:30"},
		{"24h winter offset", time.Date(2026, time.January, 4, 12, 30, 0, 0, time.UTC), loc, true, "13:30"},
		{"midnight 24h", time.Date(2026, time.August, 4, 0, 0, 0, 0, time.UTC), time.UTC, true, "00:00"},
		{"midnight 12h", time.Date(2026, time.August, 4, 0, 0, 0, 0, time.UTC), time.UTC, false, "12:00 AM"},
		{"just after midnight 12h", time.Date(2026, time.August, 4, 0, 5, 0, 0, time.UTC), time.UTC, false, "12:05 AM"},
		{"noon 24h", time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC), time.UTC, true, "12:00"},
		{"noon 12h", time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC), time.UTC, false, "12:00 PM"},
		{"11:59 is AM", time.Date(2026, time.August, 4, 11, 59, 0, 0, time.UTC), time.UTC, false, "11:59 AM"},
		{"23:05 12h", time.Date(2026, time.August, 4, 23, 5, 0, 0, time.UTC), time.UTC, false, "11:05 PM"},
		{"09:05 24h pads", time.Date(2026, time.August, 4, 9, 5, 0, 0, time.UTC), time.UTC, true, "09:05"},
		{"nil location is UTC, never the host zone", summer, nil, true, "12:30"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.FormatTime(domain.LangFR, tc.t, tc.loc, tc.format24); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatDateTime(t *testing.T) {
	loc := paris(t)
	c := MustLoad()
	// 22:30 UTC on the 4th is 00:30 on the 5th in Paris: the date has to come from
	// the instant in loc, not from the UTC calendar day.
	late := time.Date(2026, time.August, 4, 22, 30, 0, 0, time.UTC)
	tests := []struct{ name, got, want string }{
		{"fr", c.FormatDateTime(domain.LangFR, time.Date(2026, time.August, 4, 12, 30, 0, 0, time.UTC), loc, true),
			"mardi 4 août à 14:30"},
		{"en 12h", c.FormatDateTime(domain.LangEN, time.Date(2026, time.August, 4, 12, 30, 0, 0, time.UTC), loc, false),
			"Tuesday 4 August at 2:30 PM"},
		{"date follows the zone", c.FormatDateTime(domain.LangFR, late, loc, true), "mercredi 5 août à 00:30"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestRelativeDay(t *testing.T) {
	c := MustLoad()
	today := domain.MustParseDate("2026-01-01") // crosses a year boundary both ways
	tests := []struct {
		name, date, wantFR string
	}{
		{"today", "2026-01-01", "Aujourd'hui"},
		{"tomorrow", "2026-01-02", "Demain"},
		{"yesterday, previous year", "2025-12-31", "Hier"},
		{"two days ahead", "2026-01-03", "samedi 3 janvier"},
		{"two days back", "2025-12-30", "mardi 30 décembre"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.RelativeDay(domain.LangFR, domain.MustParseDate(tc.date), today); got != tc.wantFR {
				t.Errorf("got %q, want %q", got, tc.wantFR)
			}
		})
	}
	if got := c.RelativeDay(domain.LangEN, domain.MustParseDate("2026-01-02"), today); got != "Tomorrow" {
		t.Errorf("en tomorrow = %q", got)
	}
	if got := c.RelativeDay(domain.LangEN, domain.MustParseDate("2026-03-01"), today); got != "Sunday 1 March" {
		t.Errorf("en distant = %q", got)
	}
}

// TestFS pins what the HTTP layer serves: the same bytes the server translates from,
// at the paths the browser asks for.
func TestFS(t *testing.T) {
	c := MustLoad()
	for _, name := range []string{"fr.json", "en.json"} {
		data, err := fs.ReadFile(c.FS(), name)
		if err != nil {
			t.Fatalf("read %s from FS(): %v", name, err)
		}
		var table map[string]string
		if err := json.Unmarshal(data, &table); err != nil {
			t.Fatalf("%s is not a flat JSON string map: %v", name, err)
		}
		lang := domain.Language(strings.TrimSuffix(name, ".json"))
		if table["app.name"] != c.T(lang, "app.name", nil) {
			t.Errorf("%s served content disagrees with the loaded catalog", name)
		}
	}
	if _, err := fs.ReadFile(c.FS(), "locales/fr.json"); err == nil {
		t.Error("FS() should be rooted at the locale files, not at the locales directory")
	}
}

func TestLoadIsIndependent(t *testing.T) {
	a, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := MustLoad()
	if a.T(domain.LangFR, "app.name", nil) != b.T(domain.LangFR, "app.name", nil) {
		t.Error("two loads disagree")
	}
}
