package events

import (
	"fmt"

	"agenda/internal/domain"
)

// Source references identify what a queued notification was created for. They are a
// string rather than a set of foreign keys because the outbox has to survive the
// thing it refers to being edited or deleted, and because pruning is then a prefix
// match instead of a join.
//
// The layout is hierarchical, narrowest last:
//
//	reminder:{eventID}:{occurrenceDate}:{reminderID}
//
// which makes the two prunes the application actually needs into prefix deletes:
//
//	reminder:12:              everything queued for event 12 (the series changed shape)
//	reminder:12:2026-08-04:   everything queued for one occurrence (it moved or was cancelled)
//
// internal/events prunes; internal/notify enqueues. Both use these helpers, so the
// format lives in one place and a change cannot desynchronise them.

// ReminderSourceRef identifies one user's reminder for one occurrence.
func ReminderSourceRef(eventID int64, occDate domain.Date, reminderID int64) string {
	return fmt.Sprintf("reminder:%d:%s:%d", eventID, occDate, reminderID)
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

// ActivitySourceRef identifies a single activity notification.
func ActivitySourceRef(activityID int64) string {
	return fmt.Sprintf("activity:%d", activityID)
}
