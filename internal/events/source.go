package events

import (
	"fmt"
	"strings"

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
// eventUID is the name of the row eventID names, which is the part a reused id cannot
// imitate. The two are read off one row on purpose: the name is there to say which of
// the events that id has held this reference is about, and a name taken from anywhere
// else does not answer that. For an occurrence somebody has edited, eventID is still the
// series template's — that is the handle the prunes use — so the name is the template's
// too, and not the copy standing in for that date. Reading it off the copy was the same
// reminder filed twice: the copy is created by the edit and named then, so the first
// edit that left the hour alone moved the reference of every reminder the family had
// already been sent, past the delivered row that was supposed to absorb the re-plan.
//
// Where it may go is settled by the layout above being a prune hierarchy before it is an
// identity. The two prefixes stop after the occurrence date, so what a name must not do
// is displace either of the first two components — which leaves the last two positions,
// not only the last. It takes the last one because the layout reads broadest to narrowest
// and a name qualifying the whole reference belongs at the narrow end, and because the
// pre-0007 spelling is then a truncation of the current one rather than a different
// arrangement of the same fields: one reference read two ways, which is what lets old and
// new rows sit in the outbox together. TestSourceRefPrefixesNest holds the arrangement.
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
//
// The trailing colon is belt-and-braces and no test can be written for it: an occurrence
// date is always ten characters, so "reminder:12:2026-08-04" cannot reach another
// occurrence's references however it is spelled. It stays because the prefix below, whose
// colon does real work, would otherwise be the odd one out — and because the day a date
// stops being fixed-width is not the day to discover which of the two was load-bearing.
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

// TestSourceRef identifies the notification the "send me a test" button files.
//
// It is a reminder as far as delivery is concerned — same payload, same channels, so the
// button exercises the real pipeline rather than a special path that could work while
// the real one is broken — but the planner never produces one. That is why it has a
// reference of its own rather than a reminder's: PlannerOwns below is what keeps the
// outbox reconciliation, which deletes undelivered reminder rows a pass no longer calls
// for, from deleting the one row no pass will ever call for. Without it a test
// notification was dropped by the next planning pass, which on a thirty-second tick is
// most of them, and the button reported success having sent nothing.
func TestSourceRef(userID int64, nonce int64) string {
	return fmt.Sprintf("test:%d:%d", userID, nonce)
}

// PlannerOwns reports whether a queued row's reference is one a planning pass produces,
// and therefore whether reconcile may delete it when a pass no longer calls for it.
//
// Asked of the reference rather than of the kind, because the two can disagree: a test
// notification is filed under the reminder kind so that it travels the reminder's
// delivery path, and the kind alone would hand it to a reconciliation that recomputes
// every reminder from the calendar and finds no reason for this one.
func PlannerOwns(sourceRef string) bool {
	for _, prefix := range []string{"reminder:", "digest:", "summary:", "activity:"} {
		if strings.HasPrefix(sourceRef, prefix) {
			return true
		}
	}
	return false
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
