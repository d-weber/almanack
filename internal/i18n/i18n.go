// Package i18n holds the fr/en catalogs and the date formatting the server needs to
// compose notification and email text.
//
// The catalogs are the same files the browser fetches — embedded once here and served
// through FS() — so a string can never be right in one place and stale in the other.
//
// Date formatting lives here because Go's time.Format is English-only: there is no
// stdlib way to write "mardi 4 août", and the month and weekday names therefore come
// from the catalogs rather than from a table in Go, which keeps the browser's Intl
// output and the server's notification text in agreement.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"almanack/internal/domain"
)

//go:embed locales/*.json
var embedded embed.FS

const localesDir = "locales"

// Catalog keys used by the formatters. Callers that compose their own text use the
// keys directly; these are the ones this package resolves itself.
const (
	KeyToday     = "date.today"
	KeyTomorrow  = "date.tomorrow"
	KeyYesterday = "date.yesterday"
	// KeyDateTime joins a formatted date and time ("{date} à {time}").
	KeyDateTime = "date.dateTime"
)

// FallbackLang is used when a language has no catalog. The family is French; a French
// notification is a far better failure than an empty one.
const FallbackLang = domain.LangFR

// Catalog is the loaded set of translation tables. It is read-only after Load and
// therefore safe to share across goroutines.
type Catalog struct {
	tables map[domain.Language]map[string]string
	files  fs.FS
}

// Load parses the embedded catalogs.
func Load() (*Catalog, error) {
	files, err := fs.Sub(embedded, localesDir)
	if err != nil {
		return nil, fmt.Errorf("open embedded locales: %w", err)
	}
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded locales: %w", err)
	}
	c := &Catalog{tables: make(map[domain.Language]map[string]string), files: files}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, fmt.Errorf("read locale %s: %w", name, err)
		}
		var table map[string]string
		if err := json.Unmarshal(data, &table); err != nil {
			return nil, fmt.Errorf("parse locale %s: %w", name, err)
		}
		if len(table) == 0 {
			return nil, fmt.Errorf("locale %s is empty", name)
		}
		c.tables[domain.Language(strings.TrimSuffix(name, ".json"))] = table
	}
	if c.tables[FallbackLang] == nil {
		return nil, fmt.Errorf("locale %s.json is missing: it is the fallback and must exist", FallbackLang)
	}
	return c, nil
}

// MustLoad is Load for main and for tests: the catalogs are compiled into the binary,
// so a failure here is a broken build, not a runtime condition anyone can handle.
func MustLoad() *Catalog {
	c, err := Load()
	if err != nil {
		panic(err)
	}
	return c
}

// FS returns the embedded locale files, rooted so that "fr.json" is at the top. The
// HTTP layer serves these at /locales/{lang}.json, which is how the browser and the
// server end up reading exactly the same strings.
func (c *Catalog) FS() fs.FS { return c.files }

// T translates key into lang, substituting {placeholder} occurrences from params.
//
// A missing language falls back to French. A missing key returns the key itself: a
// notification reading "notify.digest.title" is an obvious bug report, while an empty
// one looks like a working app that had nothing to say.
func (c *Catalog) T(lang domain.Language, key string, params map[string]string) string {
	table := c.tables[lang]
	if table == nil {
		table = c.tables[FallbackLang]
	}
	s, ok := table[key]
	if !ok {
		return key
	}
	return substitute(s, params)
}

// substitute replaces {name} with params[name] in one pass. A placeholder with no
// matching param is left in place rather than blanked, for the same reason a missing
// key returns the key. One pass also means a substituted value containing braces is
// never itself rescanned.
func substitute(s string, params map[string]string) string {
	if len(params) == 0 || !strings.ContainsRune(s, '{') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '{' {
			b.WriteByte(s[i])
			i++
			continue
		}
		rel := strings.IndexByte(s[i:], '}')
		if rel < 0 {
			b.WriteString(s[i:])
			break
		}
		if v, ok := params[s[i+1:i+rel]]; ok {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : i+rel+1])
		}
		i += rel + 1
	}
	return b.String()
}

// FormatDate renders "mardi 4 août" / "Tuesday 4 August".
func (c *Catalog) FormatDate(lang domain.Language, d domain.Date) string {
	return c.weekday(lang, d) + " " + strconv.Itoa(d.Day) + " " + c.month(lang, d)
}

// FormatDateFull renders "mardi 4 août 2026" / "Tuesday 4 August 2026".
func (c *Catalog) FormatDateFull(lang domain.Language, d domain.Date) string {
	return c.FormatDate(lang, d) + " " + strconv.Itoa(d.Year)
}

// FormatDateShort renders "4 août" / "4 Aug", for lines where the weekday is noise.
func (c *Catalog) FormatDateShort(lang domain.Language, d domain.Date) string {
	return strconv.Itoa(d.Day) + " " + c.T(lang, "date.month.short."+strconv.Itoa(int(d.Month)), nil)
}

// FormatTime renders t as "14:30" or "2:30 PM", in loc.
//
// loc is explicit and never the machine's zone: the family timezone decides what time
// an event is at, whatever the server is set to. A nil loc is treated as UTC rather
// than as local time, so a caller that forgets is wrong in an obvious, constant way
// instead of a way that depends on the host.
func (c *Catalog) FormatTime(lang domain.Language, t time.Time, loc *time.Location, format24 bool) string {
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	if format24 {
		return fmt.Sprintf("%02d:%02d", t.Hour(), t.Minute())
	}
	// The 0/12 case: hour 0 is 12 AM and hour 12 is 12 PM, not 0 AM and 0 PM.
	suffix := "AM"
	if t.Hour() >= 12 {
		suffix = "PM"
	}
	h := t.Hour() % 12
	if h == 0 {
		h = 12
	}
	return fmt.Sprintf("%d:%02d %s", h, t.Minute(), suffix)
}

// FormatDateTime renders "mardi 4 août à 14:30" / "Tuesday 4 August at 2:30 PM".
// The date is the one t falls on in loc, which is the only correct way to name the day
// of an instant.
func (c *Catalog) FormatDateTime(lang domain.Language, t time.Time, loc *time.Location, format24 bool) string {
	if loc == nil {
		loc = time.UTC
	}
	return c.T(lang, KeyDateTime, map[string]string{
		"date": c.FormatDate(lang, domain.DateIn(t, loc)),
		"time": c.FormatTime(lang, t, loc, format24),
	})
}

// RelativeDay names d relative to today ("Aujourd'hui", "Demain", "Hier") and falls
// back to FormatDate beyond that window. Callers pass the family-tz today, since that
// is the day the family is living in.
func (c *Catalog) RelativeDay(lang domain.Language, d, today domain.Date) string {
	switch today.DaysUntil(d) {
	case 0:
		return c.T(lang, KeyToday, nil)
	case 1:
		return c.T(lang, KeyTomorrow, nil)
	case -1:
		return c.T(lang, KeyYesterday, nil)
	}
	return c.FormatDate(lang, d)
}

func (c *Catalog) weekday(lang domain.Language, d domain.Date) string {
	return c.T(lang, "date.weekday."+strconv.Itoa(int(d.Weekday())), nil)
}

func (c *Catalog) month(lang domain.Language, d domain.Date) string {
	return c.T(lang, "date.month."+strconv.Itoa(int(d.Month)), nil)
}
