// Package domain holds the core types shared across the application. It depends on
// nothing but the standard library, and contains no I/O: every other package may
// import it, and it imports none of them.
package domain

import (
	"errors"
	"time"
)

// Sentinel errors. Handlers map these to HTTP status codes; nothing else should
// need to inspect error strings.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrForbidden    = errors.New("forbidden")
	ErrUnauthorized = errors.New("unauthorized")
	ErrInvalid      = errors.New("invalid")
)

// Language is a UI language. The family speaks French and English; adding a third
// means adding a catalog in web/locales and a case in the server-side date formatter.
type Language string

const (
	LangFR Language = "fr"
	LangEN Language = "en"
)

func (l Language) Valid() bool { return l == LangFR || l == LangEN }

// User is a family member.
type User struct {
	ID          int64        `json:"id"`
	Email       string       `json:"email"`
	DisplayName string       `json:"display_name"`
	Color       string       `json:"color"` // #rrggbb, drives per-person event colouring
	Lang        Language     `json:"lang"`
	WeekStart   time.Weekday `json:"week_start"`  // display only; never reaches internal/recur
	TimeFormat  string       `json:"time_format"` // "24h" or "12h"
	HasAvatar   bool         `json:"has_avatar"`
	IsAdmin     bool         `json:"is_admin"` // first account; may invite to any calendar, sees ops heartbeat
	CreatedAt   time.Time    `json:"created_at"`
}

// Calendar is a shared calendar. Permissions are deliberately flat: every member may
// create, edit and delete any event. Only the creator may remove members.
type Calendar struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	CreatorID int64     `json:"creator_id"`
	CreatedAt time.Time `json:"created_at"`
	// HasImage says whether a cover image exists, so the calendar list can stay
	// small; the bytes are fetched separately and cached by the browser.
	HasImage bool `json:"has_image"`
}

// Member is a user's membership of a calendar, including their per-calendar
// notification settings (these live here, not in NotificationPrefs, because they are
// per pair).
type Member struct {
	CalendarID        int64     `json:"calendar_id"`
	UserID            int64     `json:"user_id"`
	Muted             bool      `json:"muted"`
	ParticipatingOnly bool      `json:"participating_only"`
	JoinedAt          time.Time `json:"joined_at"`
}

// Label is one of the ten colour labels every calendar is seeded with. Labels are
// renamed, recoloured and reordered — never added or deleted, which keeps
// events.label_id non-null forever.
type Label struct {
	ID         int64  `json:"id"`
	CalendarID int64  `json:"calendar_id"`
	Name       string `json:"name"`
	Color      string `json:"color"`
	Position   int    `json:"position"`
}

// LabelsPerCalendar is fixed; see Label.
const LabelsPerCalendar = 10

// Event is a single event, or the template of a recurring series when
// RecurrenceID is set. Timed events carry StartsAt/EndsAt in UTC; all-day events
// carry StartDate/EndDate (inclusive) and no instants. Exactly one pair is populated.
type Event struct {
	ID           int64     `json:"id"`
	CalendarID   int64     `json:"calendar_id"`
	Title        string    `json:"title"`
	AllDay       bool      `json:"all_day"`
	StartsAt     time.Time `json:"starts_at,omitzero"` // UTC, timed events only
	EndsAt       time.Time `json:"ends_at,omitzero"`   // UTC, timed events only
	StartDate    Date      `json:"start_date,omitzero"`
	EndDate      Date      `json:"end_date,omitzero"` // inclusive
	Location     string    `json:"location"`
	URL          string    `json:"url"`
	Notes        string    `json:"notes"`
	LabelID      int64     `json:"label_id"`
	RecurrenceID *int64    `json:"recurrence_id,omitempty"`
	Participants []int64   `json:"participants"`
	CreatedBy    int64     `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedBy    int64     `json:"updated_by"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Freq is the recurrence frequency.
type Freq string

const (
	FreqDaily   Freq = "daily"
	FreqWeekly  Freq = "weekly"
	FreqMonthly Freq = "monthly"
	FreqYearly  Freq = "yearly"
)

func (f Freq) Valid() bool {
	switch f {
	case FreqDaily, FreqWeekly, FreqMonthly, FreqYearly:
		return true
	}
	return false
}

// Recurrence is a repeat pattern, stored as structured columns rather than an RRULE
// string so it can be validated and queried. It describes *which dates* recur; the
// time of day and duration live on the Event.
//
// Exactly one monthly mode may be set: ByMonthday, or ByWeekday+WeekOrdinal, or
// MonthLastDay.
type Recurrence struct {
	ID           int64          `json:"id"`
	Freq         Freq           `json:"freq"`
	Interval     int            `json:"interval"` // >= 1, anchored on DTStart
	ByWeekday    []time.Weekday `json:"by_weekday,omitempty"`
	ByMonthday   *int           `json:"by_monthday,omitempty"`  // 1..31
	WeekOrdinal  *int           `json:"week_ordinal,omitempty"` // 1..5, or -1 for "last"
	MonthLastDay bool           `json:"month_last_day,omitempty"`
	Until        *Date          `json:"until,omitempty"` // inclusive; nil means forever
	DTStart      Date           `json:"dtstart"`         // anchor, in family-tz dates
}

// EditScope selects what an edit or delete of a recurring occurrence applies to.
type EditScope string

const (
	ScopeThis     EditScope = "this"     // create/replace an override for one occurrence
	ScopeUpcoming EditScope = "upcoming" // split the series at this occurrence
	ScopeAll      EditScope = "all"      // mutate the series template
)

func (s EditScope) Valid() bool {
	switch s {
	case ScopeThis, ScopeUpcoming, ScopeAll:
		return true
	}
	return false
}

// Occurrence is one materialized instance of an event within a queried window. It is
// computed on read and never stored: nothing can drift out of sync with the series.
type Occurrence struct {
	Event
	// OccurrenceDate is the family-tz date of the occurrence in the *original* series
	// (the identity used by overrides). For a non-recurring event it is the start date.
	OccurrenceDate Date `json:"occurrence_date"`
	IsOverride     bool `json:"is_override"`
	// SeriesEventID is the id of the series template event, when this occurrence
	// belongs to a series (the Event fields may come from an override copy).
	SeriesEventID *int64 `json:"series_event_id,omitempty"`
}

// Reminder is one user's reminder for one event or series. Reminders are per-user by
// design: creating an event never pushes reminders onto other members.
//
// Exactly one of EventID / RecurrenceID is set, and the offset fields differ by event
// kind: timed events use OffsetMinutes (minutes before the start); all-day events use
// DaysBefore + AtTimeLocal, because "09:00 on the day" is not expressible as an
// offset before midnight.
type Reminder struct {
	ID            int64  `json:"id"`
	EventID       *int64 `json:"event_id,omitempty"`
	RecurrenceID  *int64 `json:"recurrence_id,omitempty"`
	UserID        int64  `json:"user_id"`
	OffsetMinutes *int   `json:"offset_minutes,omitempty"`
	DaysBefore    *int   `json:"days_before,omitempty"`
	AtTimeLocal   string `json:"at_time_local,omitempty"` // "HH:MM" family tz
}

// PushSubscription is one browser profile on one device. A user has as many as they
// have devices; LastConfirmedAt is bumped by the client's liveness check, which is
// how a silently-dead iOS subscription is detected (the push service keeps returning
// success for those, so delivery errors alone never reveal them).
type PushSubscription struct {
	ID              int64     `json:"id"`
	UserID          int64     `json:"user_id"`
	Endpoint        string    `json:"endpoint"`
	P256DH          string    `json:"-"`
	Auth            string    `json:"-"`
	UALabel         string    `json:"ua_label"`
	CreatedAt       time.Time `json:"created_at"`
	LastOKAt        time.Time `json:"last_ok_at,omitzero"`
	LastConfirmedAt time.Time `json:"last_confirmed_at,omitzero"`
	Failures        int       `json:"failures"`
}

// NotificationPrefs are a user's global notification settings. Per-calendar mute and
// participating-only live on Member.
type NotificationPrefs struct {
	UserID           int64  `json:"user_id"`
	DigestEnabled    bool   `json:"digest_enabled"`
	DigestTime       string `json:"digest_time"`     // "HH:MM" family tz
	DigestOnEmpty    bool   `json:"digest_on_empty"` // notify even on days with no events
	DailySummaryMode bool   `json:"daily_summary_mode"`
	SummaryTime      string `json:"summary_time"` // "HH:MM" family tz, when DailySummaryMode
	EmailReminders   bool   `json:"email_reminders"`
	EmailDigest      bool   `json:"email_digest"`
	ActivityPush     bool   `json:"activity_push"`
}

// NotificationKind identifies what a queued notification is.
type NotificationKind string

const (
	KindReminder NotificationKind = "reminder"
	KindDigest   NotificationKind = "digest"
	KindActivity NotificationKind = "activity"
	KindSummary  NotificationKind = "summary" // batched activity digest
)

// QueuedNotification is a row of the durable outbox. Delivery is at-least-once:
// SentAt is set only after a provider accepts the message. A crash between sending
// and marking may duplicate a notification, which is the correct trade — a duplicate
// reminder is annoying, a missed one is the failure this whole app exists to prevent.
// Do not "fix" duplicates by marking before sending.
type QueuedNotification struct {
	ID               int64            `json:"id"`
	UserID           int64            `json:"user_id"`
	Kind             NotificationKind `json:"kind"`
	SourceRef        string           `json:"source_ref"` // stable identity, e.g. "reminder:12:2026-08-04"
	Payload          string           `json:"payload"`    // JSON
	DueAt            time.Time        `json:"due_at"`     // UTC
	SendingStartedAt time.Time        `json:"sending_started_at,omitzero"`
	SentAt           time.Time        `json:"sent_at,omitzero"`
	// EmailSentAt records the email leg on its own, because the two channels fail
	// independently: a push acceptance is not evidence that the mail went, and a
	// retry that re-sent an email already accepted would mail the family the same
	// reminder on every pass until the row retires.
	EmailSentAt time.Time `json:"email_sent_at,omitzero"`
	Skipped     string    `json:"skipped,omitempty"` // reason, when delivered stale
	Attempts    int       `json:"attempts"`
}

// ActivityAction is what happened, for the activity feed and its notifications.
type ActivityAction string

const (
	ActionEventCreated ActivityAction = "event_created"
	ActionEventUpdated ActivityAction = "event_updated"
	ActionEventDeleted ActivityAction = "event_deleted"
	ActionMemberJoined ActivityAction = "member_joined"
	ActionMemberLeft   ActivityAction = "member_left"
)

// Activity is one entry of the change log. It is never pruned in v1: a family
// generates a few thousand rows a decade.
type Activity struct {
	ID         int64          `json:"id"`
	CalendarID int64          `json:"calendar_id"`
	UserID     int64          `json:"user_id"`
	Action     ActivityAction `json:"action"`
	EventID    *int64         `json:"event_id,omitempty"`
	Title      string         `json:"title"` // denormalized: survives the event's deletion
	At         time.Time      `json:"at"`
}

// Session is a logged-in browser. Tokens are stored hashed; the plaintext exists only
// in the cookie.
type Session struct {
	ID         int64
	UserID     int64
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// Invite is a join link for a calendar: multi-use within its window, revocable, and
// the only route to creating an account.
type Invite struct {
	ID         int64      `json:"id"`
	CalendarID int64      `json:"calendar_id"`
	CreatedBy  int64      `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	UsedCount  int        `json:"used_count"`
}

// Holiday is a public holiday shown on the calendar. French holidays are computed,
// not stored; HolidayOverride rows add or suppress dates when the law changes.
type Holiday struct {
	Date Date   `json:"date"`
	Name string `json:"name"`
}

// InviteTTL is how long a fresh invite link stays usable.
const InviteTTL = 7 * 24 * time.Hour

// SessionTTL is the sliding session lifetime, renewed on use.
const SessionTTL = 90 * 24 * time.Hour

// PasswordResetTTL is deliberately short: the token is emailed in clear.
const PasswordResetTTL = 30 * time.Minute
