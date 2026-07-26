package httpapi

import (
	"strings"
	"unicode"

	"agenda/internal/auth"
	"agenda/internal/domain"
	"agenda/internal/i18n"
)

// defaultLang is what a client that states no preference gets. The family is French.
const defaultLang = i18n.FallbackLang

// maxTextLen bounds the free-text fields. Nothing in a family calendar needs more, and
// an unbounded title is a way to make every month view unreadable.
const (
	maxNameLen  = 80
	maxTextLen  = 2000
	maxEmailLen = 254
)

// normalizeEmail lower-cases and trims an address and checks it looks like one. The
// check is deliberately shallow: RFC 5322 permits things no family member will ever
// type, and the only real proof an address works is that mail to it arrives.
func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" {
		return "", invalidf("an email address is required")
	}
	if len(email) > maxEmailLen {
		return "", invalidf("that email address is too long")
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.Count(email, "@") != 1 {
		return "", invalidf("that does not look like an email address")
	}
	if strings.ContainsAny(email, " \t\r\n,;<>\"") || !strings.Contains(email[at:], ".") {
		return "", invalidf("that does not look like an email address")
	}
	return email, nil
}

// validatePassword mirrors internal/auth's rule, so a too-short password is a 400 with
// a message rather than a 500 from the hasher.
func validatePassword(pw string) error {
	if len(pw) < auth.MinPasswordLength {
		return invalidf("the password must be at least %d characters", auth.MinPasswordLength)
	}
	if len(pw) > 1024 {
		return invalidf("that password is too long")
	}
	return nil
}

// normalizeColor accepts "#rrggbb" (case-insensitive) and returns it lower-cased.
// Colours reach CSS custom properties in the browser, so anything else is refused here
// rather than sanitised there.
func normalizeColor(raw string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(raw))
	if len(c) != 7 || c[0] != '#' {
		return "", invalidf("a colour must be in #rrggbb form")
	}
	for _, r := range c[1:] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return "", invalidf("a colour must be in #rrggbb form")
		}
	}
	return c, nil
}

// cleanText trims a free-text field and rejects control characters, which have no place
// in a title and are how a log line or a mail header gets forged.
// cleanSingleLine is cleanText for fields rendered on one line and — the reason this
// exists — interpolated into an email Subject. cleanText deliberately tolerates
// newlines for multi-line notes, but a newline in an event title makes the mailer
// refuse the whole message, so one such title silently costs every participant their
// reminder email: the channel that exists precisely because iOS push dies quietly.
func cleanSingleLine(raw string, max int, what string) (string, error) {
	r := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ")
	return cleanText(r.Replace(raw), max, what)
}

func cleanText(raw string, max int, what string) (string, error) {
	text := strings.TrimSpace(raw)
	if len([]rune(text)) > max {
		return "", invalidf("%s is too long (max %d characters)", what, max)
	}
	for _, r := range text {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return "", invalidf("%s contains a control character", what)
		}
	}
	return text, nil
}

func validateHHMM(raw, what string) (string, error) {
	value := strings.TrimSpace(raw)
	h, m, ok := strings.Cut(value, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return "", invalidf("%s must be HH:MM", what)
	}
	hh, err1 := atoiFixed(h)
	mm, err2 := atoiFixed(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return "", invalidf("%s must be HH:MM", what)
	}
	return value, nil
}

func atoiFixed(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, invalidf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func validLanguage(raw string) (domain.Language, error) {
	lang := domain.Language(strings.TrimSpace(raw))
	if !lang.Valid() {
		return "", invalidf("unknown language %q", raw)
	}
	return lang, nil
}
