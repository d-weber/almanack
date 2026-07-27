package events

import (
	"fmt"

	"almanack/internal/domain"
)

// Source references identify what a queued notification was created for. They are a
// string rather than a set of foreign keys because the outbox has to survive the
// thing it refers to being edited or deleted, and because pruning is then a prefix
// match instead of a join.
//
// The layout is hierarchical, narrowest last:
//
//	reminder:{eventID}:{occurrenceDate}:{reminderID}:{eventUID}
//
// which makes the two prunes the application actually needs into prefix deletes:
//
//	reminder:12:              everything queued for event 12 (the series changed shape)
//	reminder:12:2026-08-04:   everything queued for one occurrence (it moved or was cancelled)
//
// internal/events prunes; internal/notify enqueues. Both use these helpers, so the
// format lives in one place and a change cannot desynchronise them.

// ReminderSourceRef identifies one user's reminder for one occurrence: the appointment
// it warns about rather than the row numbers that appointment happens to occupy.
//
// The three ids are all reusable — events.id, reminders.id and the occurrence date of a
// series whose template can itself be deleted and remade — and none of them is
// AUTOINCREMENT, so SQLite hands them out again as soon as the rows holding them are
// gone. Since the outbox keys on the reference and the instant, and keeps a delivered
// row for good as the record that the family was told, a replacement appointment landing
// on the same occurrence date with the same reminder instant was read as the reminder
// already sent and dropped (migration 0007).
//
// eventUID is the name of the event row this occurrence's fields came from, which is the
// part a reused id cannot imitate. For a plain event and for an ordinary occurrence of a
// series that is the event or its template; for an occurrence somebody has edited it is
// the copy standing in for that date, which is right rather than a compromise — the copy
// is what the notification says, and it is the copy being remade that makes the
// notification a different one.
//
// It goes last because the layout above is a prune hierarchy before it is an identity:
// only the tail is free, and a name anywhere earlier would put the two prefixes out of
// reach of the references they are meant to match. So it qualifies the first component
// rather than the one beside it. TestSourceRefPrefixesNest holds the arrangement.
//
// An event created before events had names keeps the old spelling. That is not a
// concession: its reminders are already in the outbox under it, the row itself is what
// is re-read on every planning pass, and nothing created since can produce that spelling
// — so the old events go on being recognised and the new ones cannot be mistaken for
// them.
func ReminderSourceRef(eventID int64, occDate domain.Date, reminderID int64, eventUID string) string {
	if eventUID == "" {
		return fmt.Sprintf("reminder:%d:%s:%d", eventID, occDate, reminderID)
	}
	return fmt.Sprintf("reminder:%d:%s:%d:%s", eventID, occDate, reminderID, eventUID)
}

// OccurrenceSourcePrefix matches everything queued for a single occurrence.
func OccurrenceSourcePrefix(eventID int64, occDate domain.Date) string {
	return fmt.Sprintf("reminder:%d:%s:", eventID, occDate)
}

// EventSourcePrefix matches everything queued for an event or its whole series.
func EventSourcePrefix(eventID int64) string {
	return fmt.Sprintf("reminder:%d:", eventID)
}

// DigestSourceRef identifies a user's daily digest for one day. The date is the
// family-tz day being summarised, so re-planning the same day is idempotent.
func DigestSourceRef(day domain.Date) string {
	return fmt.Sprintf("digest:%s", day)
}

// SummarySourceRef identifies a user's batched activity summary for one day.
func SummarySourceRef(day domain.Date) string {
	return fmt.Sprintf("summary:%s", day)
}

// ActivitySourceRef identifies a single activity notification: the change it announces
// rather than the row number that change happens to occupy.
//
// The two are not the same thing. activity_log.id is INTEGER PRIMARY KEY without
// AUTOINCREMENT, so a deleted row's id is handed out again, and a reference built from
// the number alone said the same thing about the row that took it as about the row it
// replaced. Since the outbox keys on the reference and the instant, and instants are
// stored to the second, a replacement made in the same second was taken for the
// announcement already made and dropped. The name the store gives each change
// (domain.Activity.ChangeUID) is the part a reused id cannot imitate.
//
// A change logged before the log gave out names keeps the old spelling. That is not a
// concession: its notification is already in the outbox under it, the row itself is
// what is re-read whenever this is asked again, and nothing logged since can produce
// that spelling — so the old rows keep being recognised and the new ones cannot be
// mistaken for them.
func ActivitySourceRef(a domain.Activity) string {
	if a.ChangeUID == "" {
		return fmt.Sprintf("activity:%d", a.ID)
	}
	return fmt.Sprintf("activity:%d:%s", a.ID, a.ChangeUID)
}
