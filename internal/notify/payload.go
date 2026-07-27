package notify

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"almanack/internal/domain"
	"almanack/internal/i18n"
	"almanack/internal/webpush"
)

// payload is what a queued row carries: the *data* of a notification, never its
// text. Localization happens at delivery time from the recipient's users.lang
// (the notification rules in docs/architecture.md), so a queued row must survive a user switching language
// between planning and delivery — and it does, because there is not a word of
// French or English in here.
//
// It is also the only thing CatchUp needs in order to apply the staleness policy:
// EventStart and EventDate say when the announced event is, so a row can be judged
// long after the event it referred to was edited or deleted.
type payload struct {
	Kind domain.NotificationKind `json:"kind"`

	// Reminder fields.
	EventID    int64       `json:"event_id,omitempty"`
	OccDate    domain.Date `json:"occ_date,omitzero"`
	Title      string      `json:"title,omitempty"`
	Location   string      `json:"location,omitempty"`
	AllDay     bool        `json:"all_day,omitempty"`
	EventStart time.Time   `json:"event_start,omitzero"` // UTC, timed events
	EventDate  domain.Date `json:"event_date,omitzero"`  // family-tz date, all-day events

	// Digest and summary fields. Day is the family-tz day being summarised.
	Day   domain.Date  `json:"day,omitzero"`
	Items []digestItem `json:"items,omitempty"`
	Total int          `json:"total,omitempty"`

	// Activity fields.
	ActivityID int64                 `json:"activity_id,omitempty"`
	Action     domain.ActivityAction `json:"action,omitempty"`
	Actor      string                `json:"actor,omitempty"`
	Calendar   string                `json:"calendar,omitempty"`
}

// digestItem is one line of a digest. Titles are truncated when the row is
// planned, so the stored payload is bounded too and an oversized digest cannot be
// created by someone with a talent for long event names.
type digestItem struct {
	Title    string    `json:"title"`
	AllDay   bool      `json:"all_day,omitempty"`
	StartsAt time.Time `json:"starts_at,omitzero"`
}

// pushPayload is the wire format the service worker parses
// (docs/api.md, "Push payload"). Keep it in sync with web/js/sw.js.
type pushPayload struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
	Lang  string `json:"lang"`
}

const (
	// maxTitleRunes bounds a title stored in a payload. Long enough for
	// "Rendez-vous chez l'orthodontiste pour Léo", short enough that a digest of
	// many of them still fits the aes128gcm ceiling.
	maxTitleRunes = 80
	// maxDigestItems is the "first N truncated titles" of the notification rules in docs/architecture.md. The
	// rest is fetched on notificationclick.
	maxDigestItems = 6
)

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		n++
		if n > max {
			return strings.TrimSpace(s[:i]) + "…"
		}
	}
	return s
}

func encodePayload(p payload) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode %s payload: %w", p.Kind, err)
	}
	return string(b), nil
}

// eventStillAhead answers the question the notification rules in docs/architecture.md.2 asks of every overdue
// reminder: is the thing it warns about still in the future? A late warning beats
// no warning; a warning about last Tuesday is noise.
func (p payload) eventStillAhead(now time.Time, loc *time.Location) bool {
	if p.AllDay {
		if p.EventDate.IsZero() {
			return false
		}
		// An all-day event is "still ahead" for the whole of its day: telling
		// someone at 09:00 that today is the school trip is useful.
		return !p.EventDate.Before(domain.DateIn(now, loc))
	}
	if p.EventStart.IsZero() {
		return false
	}
	return p.EventStart.After(now)
}

// ---------------------------------------------------------------------------
// Rendering — the only place notification text is composed
// ---------------------------------------------------------------------------

// recipient is everything about a person that the wording depends on.
type recipient struct {
	user  domain.User
	prefs domain.NotificationPrefs
}

func (r recipient) lang() domain.Language {
	if r.user.Lang.Valid() {
		return r.user.Lang
	}
	return i18n.FallbackLang
}

func (r recipient) format24() bool { return r.user.TimeFormat != "12h" }

// render composes the push payload and, when the recipient's preferences ask for
// it, the email. Both are built here, per recipient, per language, at delivery
// time — never at planning time.
//
// tag is the queue row's source reference: the service worker uses it as the
// notification tag, so a re-delivered duplicate replaces the first one in the
// tray instead of stacking beside it. That is the only thing standing between
// at-least-once delivery and two identical dentist reminders on a lock screen.
func (n *Notifier) render(p payload, to recipient, tag string, now time.Time) (push []byte, email *mailerMessage, err error) {
	lang := to.lang()
	today := domain.DateIn(now, n.loc)

	var pp pushPayload
	pp.Kind = string(p.Kind)
	pp.Lang = string(lang)
	pp.Tag = tag

	switch p.Kind {
	case domain.KindReminder:
		pp.Title = p.Title
		pp.Body = n.reminderWhen(p, to, today)
		pp.URL = n.eventURL(p)
		if to.prefs.EmailReminders {
			email = n.reminderEmail(p, to, today)
		}
	case domain.KindDigest:
		pp.Title = n.cat.T(lang, "notify.digest.title", nil)
		pp.URL = n.dayURL(p.Day)
		if to.prefs.EmailDigest {
			email = n.digestEmail(p, to)
		}
	case domain.KindSummary:
		pp.Title = n.cat.T(lang, "notify.summary.title", nil)
		pp.Body = n.countLine(lang, "notify.summary.body", p.Total)
		pp.URL = "/#/activity"
	case domain.KindActivity:
		pp.Title = n.cat.T(lang, "notify.activity.title", map[string]string{"calendar": p.Calendar})
		pp.Body = n.cat.T(lang, "activity."+string(p.Action), map[string]string{
			"user":  p.Actor,
			"title": p.Title,
		})
		pp.URL = "/#/activity"
	default:
		return nil, nil, fmt.Errorf("notify: unknown notification kind %q", p.Kind)
	}

	push, err = n.encodePush(pp, p, to, today)
	if err != nil {
		return nil, nil, err
	}
	return push, email, nil
}

// encodePush serialises the push payload and guarantees it fits
// webpush.MaxPayloadBytes. A digest sheds its item lines one at a time and then
// its count; anything still too large falls back to the catalog's generic
// notification, because a push that displays *something* is what keeps an iOS
// subscription alive (three silent pushes and Apple revokes it).
func (n *Notifier) encodePush(pp pushPayload, p payload, to recipient, today domain.Date) ([]byte, error) {
	lang := to.lang()

	build := func(items int) ([]byte, error) {
		out := pp
		if p.Kind == domain.KindDigest {
			out.Body = n.digestBody(p, to, today, items)
		}
		return json.Marshal(out)
	}

	items := 0
	if p.Kind == domain.KindDigest {
		items = len(p.Items)
	}
	for ; items >= 0; items-- {
		b, err := build(items)
		if err != nil {
			return nil, fmt.Errorf("encode push payload: %w", err)
		}
		if len(b) <= webpush.MaxPayloadBytes {
			return b, nil
		}
	}

	// Last resort. Reaching here means even a bare count did not fit, which takes
	// a pathological title; the notification still has to display.
	b, err := json.Marshal(pushPayload{
		Kind:  pp.Kind,
		Title: n.cat.T(lang, "notify.fallback.title", nil),
		Body:  n.cat.T(lang, "notify.fallback.body", nil),
		URL:   pp.URL,
		Tag:   pp.Tag,
		Lang:  pp.Lang,
	})
	if err != nil {
		return nil, fmt.Errorf("encode fallback push payload: %w", err)
	}
	if len(b) > webpush.MaxPayloadBytes {
		return nil, fmt.Errorf("notify: even the fallback payload is %d bytes, over the %d limit", len(b), webpush.MaxPayloadBytes)
	}
	return b, nil
}

// reminderWhen is the "Demain à 14:30" line of docs/api.md. Every part
// of it comes from the catalog: Go cannot write "mardi 4 août".
func (n *Notifier) reminderWhen(p payload, to recipient, today domain.Date) string {
	lang := to.lang()
	var when string
	if p.AllDay {
		when = n.cat.RelativeDay(lang, p.EventDate, today) + " · " + n.cat.T(lang, "date.allDay", nil)
	} else {
		when = n.cat.T(lang, i18n.KeyDateTime, map[string]string{
			"date": n.cat.RelativeDay(lang, domain.DateIn(p.EventStart, n.loc), today),
			"time": n.cat.FormatTime(lang, p.EventStart, n.loc, to.format24()),
		})
	}
	if p.Location != "" {
		when += " · " + p.Location
	}
	return when
}

// digestBody renders the count line plus at most items event lines.
func (n *Notifier) digestBody(p payload, to recipient, today domain.Date, items int) string {
	lang := to.lang()
	if p.Total == 0 {
		return n.cat.T(lang, "notify.digest.empty", nil)
	}
	lines := []string{n.countLine(lang, "notify.digest.body", p.Total)}
	if items > len(p.Items) {
		items = len(p.Items)
	}
	for _, it := range p.Items[:items] {
		lines = append(lines, n.itemLine(lang, to, it, today))
	}
	if rest := p.Total - items; rest > 0 && items > 0 {
		lines = append(lines, n.cat.T(lang, "notify.digest.more", map[string]string{"count": strconv.Itoa(rest)}))
	}
	return strings.Join(lines, "\n")
}

// itemLine renders one agenda line. An all-day item borrows the timed layout with
// "Journée entière" where the time would be, so the catalog needs no extra key and
// the two read as one list.
func (n *Notifier) itemLine(lang domain.Language, to recipient, it digestItem, today domain.Date) string {
	when := n.cat.T(lang, "date.allDay", nil)
	if !it.AllDay {
		when = n.cat.FormatTime(lang, it.StartsAt, n.loc, to.format24())
	}
	return n.cat.T(lang, "notify.reminder.timed", map[string]string{"time": when, "title": it.Title})
}

// countLine picks the singular or plural key. Two keys rather than a plural rule
// engine: French and English agree at one, and the catalog is a flat dictionary
// shared with the browser.
func (n *Notifier) countLine(lang domain.Language, prefix string, count int) string {
	if count == 1 {
		return n.cat.T(lang, prefix+".one", nil)
	}
	return n.cat.T(lang, prefix+".many", map[string]string{"count": strconv.Itoa(count)})
}

// eventURL is the app-relative deep link the service worker opens. It points at
// the *series* event plus the occurrence date, which is how the client resolves
// an occurrence — including one that has been overridden.
func (n *Notifier) eventURL(p payload) string {
	if p.EventID == 0 {
		return "/"
	}
	if p.OccDate.IsZero() {
		return fmt.Sprintf("/#/event/%d", p.EventID)
	}
	return fmt.Sprintf("/#/event/%d/%s", p.EventID, p.OccDate)
}

func (n *Notifier) dayURL(d domain.Date) string {
	if d.IsZero() {
		return "/"
	}
	return fmt.Sprintf("/#/day/%s", d)
}

// ---------------------------------------------------------------------------
// Email
// ---------------------------------------------------------------------------

// mailerMessage mirrors mailer.Message. It exists so render can build a message
// without importing a nil Mailer into the decision, and so tests can assert on the
// composed text.
type mailerMessage struct {
	To      string
	Subject string
	Text    string
}

func (n *Notifier) reminderEmail(p payload, to recipient, today domain.Date) *mailerMessage {
	lang := to.lang()
	var b strings.Builder
	b.WriteString(n.cat.T(lang, "mail.greeting", map[string]string{"name": to.user.DisplayName}))
	b.WriteString("\n\n")
	b.WriteString(p.Title)
	b.WriteString("\n")
	b.WriteString(n.reminderWhen(p, to, today))
	b.WriteString("\n\n")
	b.WriteString(n.absURL(n.eventURL(p)))
	b.WriteString("\n\n")
	b.WriteString(n.cat.T(lang, "mail.footer", nil))
	b.WriteString("\n")
	return &mailerMessage{
		To:      to.user.Email,
		Subject: n.cat.T(lang, "mail.subject.reminder", map[string]string{"title": p.Title}),
		Text:    b.String(),
	}
}

func (n *Notifier) digestEmail(p payload, to recipient) *mailerMessage {
	lang := to.lang()
	var b strings.Builder
	b.WriteString(n.cat.T(lang, "mail.greeting", map[string]string{"name": to.user.DisplayName}))
	b.WriteString("\n\n")
	if p.Total == 0 {
		b.WriteString(n.cat.T(lang, "notify.digest.empty", nil))
		b.WriteString("\n")
	} else {
		b.WriteString(n.countLine(lang, "notify.digest.body", p.Total))
		b.WriteString("\n\n")
		// Email has no size ceiling worth worrying about, so it carries the whole
		// list the push had to truncate.
		for _, it := range p.Items {
			b.WriteString(n.itemLine(lang, to, it, p.Day))
			b.WriteString("\n")
		}
		if rest := p.Total - len(p.Items); rest > 0 {
			b.WriteString(n.cat.T(lang, "notify.digest.more", map[string]string{"count": strconv.Itoa(rest)}))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(n.absURL(n.dayURL(p.Day)))
	b.WriteString("\n\n")
	b.WriteString(n.cat.T(lang, "mail.footer", nil))
	b.WriteString("\n")
	return &mailerMessage{
		To:      to.user.Email,
		Subject: n.cat.T(lang, "mail.subject.digest", map[string]string{"date": n.cat.FormatDateFull(lang, p.Day)}),
		Text:    b.String(),
	}
}

func (n *Notifier) absURL(rel string) string {
	if n.baseURL == "" {
		return rel
	}
	return n.baseURL + rel
}
