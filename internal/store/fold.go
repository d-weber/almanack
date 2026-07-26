package store

import (
	"strings"
	"unicode/utf8"
)

// searchNorm builds the value of events.search_norm: the event's title, location and
// notes joined, lowercased, and stripped of French diacritics. It is recomputed by the
// store on every event write, and SearchEvents folds the query the same way, so a
// search for "ecole" finds "École" and a search for "École" finds "ecole".
//
// This is deliberately a hand-rolled table rather than Unicode NFD normalisation:
// golang.org/x/text is not on the two-module dependency allowlist (CONVENTIONS.md §1),
// and the alphabet a French-speaking family types into a calendar is small and fully
// enumerated below.
func searchNorm(title, location, notes string) string {
	return foldSearch(title + " " + location + " " + notes)
}

// foldSearch lowercases s and strips diacritics. Both sides of the LIKE go through it.
func foldSearch(s string) string {
	return foldAccents(strings.ToLower(s))
}

// foldRunes maps the accented letters of French — plus the neighbouring European
// letters a family address book tends to collect — onto their unaccented form. The
// uppercase forms are listed too so that foldAccents is correct on its own, without
// depending on a caller having lowercased first.
var foldRunes = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a",
	'ç': "c",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i",
	'ñ': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u",
	'ý': "y", 'ÿ': "y",
	'œ': "oe", 'æ': "ae",

	'À': "a", 'Á': "a", 'Â': "a", 'Ã': "a", 'Ä': "a", 'Å': "a",
	'Ç': "c",
	'È': "e", 'É': "e", 'Ê': "e", 'Ë': "e",
	'Ì': "i", 'Í': "i", 'Î': "i", 'Ï': "i",
	'Ñ': "n",
	'Ò': "o", 'Ó': "o", 'Ô': "o", 'Õ': "o", 'Ö': "o",
	'Ù': "u", 'Ú': "u", 'Û': "u", 'Ü': "u",
	'Ý': "y", 'Ÿ': "y",
	'Œ': "oe", 'Æ': "ae",
}

// foldAccents replaces every rune in foldRunes with its unaccented spelling and leaves
// everything else alone.
func foldAccents(s string) string {
	if isASCII(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if rep, ok := foldRunes[r]; ok {
			b.WriteString(rep)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
