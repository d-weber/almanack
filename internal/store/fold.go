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
// depending on a caller having lowercased first; they fold to the lowercase spelling,
// because every caller lowercases in the end.
//
// Adding a rune here is half a change, and shipping only this half breaks searching for
// the events that already exist. search_norm is computed once per event write while the
// query is folded on the way in, so the two sides have to agree about every rune: teach
// the query that "ø" means "o" and a row still reading "søren" stops matching — and it
// does not fall back to matching "ø" either, because the query no longer contains one.
// The event was findable before and is findable by nothing afterwards.
//
// So the other half is a migration that rewrites search_norm for the rows already
// stored: 0005_refold_search_norm.sql did it for ø ß ð þ đ, and anyone adding a rune
// below owes the same. It is a substitution over the old value rather than a
// re-derivation because every fold here is exact and callers lowercase first;
// TestBackfillMatchesSearchNorm pins that equivalence so the shortcut cannot rot.
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
	'ø': "o", 'ß': "ss", 'ð': "d", 'þ': "th", 'đ': "d",

	'À': "a", 'Á': "a", 'Â': "a", 'Ã': "a", 'Ä': "a", 'Å': "a",
	'Ç': "c",
	'È': "e", 'É': "e", 'Ê': "e", 'Ë': "e",
	'Ì': "i", 'Í': "i", 'Î': "i", 'Ï': "i",
	'Ñ': "n",
	'Ò': "o", 'Ó': "o", 'Ô': "o", 'Õ': "o", 'Ö': "o",
	'Ù': "u", 'Ú': "u", 'Û': "u", 'Ü': "u",
	'Ý': "y", 'Ÿ': "y",
	'Œ': "oe", 'Æ': "ae",
	'Ø': "o", 'ẞ': "ss", 'Ð': "d", 'Þ': "th", 'Đ': "d",
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
