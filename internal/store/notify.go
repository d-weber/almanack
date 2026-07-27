package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"almanack/internal/domain"
)

// ---------------------------------------------------------------------------
// Reminders
// ---------------------------------------------------------------------------

const reminderCols = `id, event_id, recurrence_id, user_id, offset_minutes, days_before, at_time_local`

func scanReminder(row rowScanner) (domain.Reminder, error) {
	var r domain.Reminder
	var eventID, recurrenceID, offsetMinutes, daysBefore sql.NullInt64
	var atTime sql.NullString
	err := row.Scan(&r.ID, &eventID, &recurrenceID, &r.UserID, &offsetMinutes, &daysBefore, &atTime)
	if err != nil {
		return domain.Reminder{}, mapErr(err)
	}
	r.EventID = i64ptr(eventID)
	r.RecurrenceID = i64ptr(recurrenceID)
	r.OffsetMinutes = intptr(offsetMinutes)
	r.DaysBefore = intptr(daysBefore)
	r.AtTimeLocal = atTime.String
	return r, nil
}

// reminderScope validates that exactly one of eventID / recurrenceID is set and returns
// the WHERE fragment and arguments selecting that scope for one user. The schema's
// CHECK says the same thing about the rows; this catches it before the round trip and
// with a clearer message.
func reminderScope(eventID, recurrenceID *int64, userID int64) (string, []any, error) {
	switch {
	case (eventID == nil) == (recurrenceID == nil):
		return "", nil, fmt.Errorf("reminders need exactly one of event or recurrence: %w", domain.ErrInvalid)
	case eventID != nil:
		return `event_id = ? AND user_id = ?`, []any{*eventID, userID}, nil
	default:
		return `recurrence_id = ? AND user_id = ?`, []any{*recurrenceID, userID}, nil
	}
}

// reminderShape is everything a reminder says: "30 minutes before", or "09:00 on the day
// before". There is no name, no note and no other column, so two reminders of the same
// shape are the same reminder — they fall due at the same instant, carry the same
// payload and cannot be told apart by anything that reads them. It is also the identity
// the editor already works in: web/js/views/event.js keys its picker on these fields and
// will not offer a shape the list is holding.
//
// The values are compared as they are stored, byte for byte; normalising "9:00" to
// "09:00" belongs to the caller that accepts it (internal/httpapi.parseReminders).
//
// Neither branch can be missed: ReplaceReminders rejects a reminder with neither shape
// and the table's CHECK says the same about the rows. The default is there so that a
// database someone has taken the constraint off cannot panic this.
func reminderShape(r domain.Reminder) string {
	switch {
	case r.OffsetMinutes != nil:
		return fmt.Sprintf("m%d", *r.OffsetMinutes)
	case r.DaysBefore != nil:
		return fmt.Sprintf("d%d@%s", *r.DaysBefore, r.AtTimeLocal)
	default:
		return ""
	}
}

// ListReminders returns one user's reminders for one event or one series.
//
// Reminders are per user by design: creating an event never pushes reminders onto
// anyone else, so this never returns another member's rows.
func (s *Store) ListReminders(ctx context.Context, eventID *int64, recurrenceID *int64, userID int64) ([]domain.Reminder, error) {
	where, args, err := reminderScope(eventID, recurrenceID, userID)
	if err != nil {
		return nil, fmt.Errorf("list reminders: %w", err)
	}
	rows, err := s.q.QueryContext(ctx, `SELECT `+reminderCols+` FROM reminders WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list reminders: %w", mapErr(err))
	}
	defer rows.Close()
	var out []domain.Reminder
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, fmt.Errorf("list reminders: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list reminders: %w", err)
	}
	return out, nil
}

// ReplaceReminders sets one user's reminders for one event or series to exactly rs,
// inside a transaction.
//
// The scope and the owner come from the arguments, not from the structs: whatever
// EventID, RecurrenceID and UserID the caller left in rs are overwritten, so a
// hand-built or replayed request cannot write reminders onto another member's account
// or another calendar's event. Passing an empty rs clears them.
//
// Each reminder must carry either OffsetMinutes (timed events: N minutes before the
// start) or DaysBefore together with AtTimeLocal (all-day events: "09:00, the day
// before"), never both and never neither — "09:00 on the day" is not expressible as an
// offset from midnight, which is why there are two shapes.
//
// What is there is reconciled against rs rather than deleted and re-inserted, so a
// reminder in both keeps its row and with it its id. The id is part of the reference the
// outbox files that reminder's notification under, and reminders.id is INTEGER PRIMARY
// KEY without AUTOINCREMENT, so re-inserting moved the reference of the whole list
// unless the rows deleted happened to be the highest in the table. The delivered row
// then no longer absorbed the re-plan, and the second copy was not merely queued but
// sent: a reminder whose slot has passed is planned while its event is still ahead, and
// a late warning is delivered on purpose. Opening the reminder editor and pressing save
// without changing anything sent the reminder again (#65).
//
// Matching is by shape, lowest id first, so that a list holding the same reminder twice
// settles on one answer rather than on map order — and so that saving it again keeps
// settling on the same one.
//
// A reminder moved to another time is a new row, and should be: it is a different
// warning at a different instant. What was queued for the old one is not left behind,
// and not because this prunes — nothing on this path does. The planner recomputes the
// window on every pass and drops the undelivered rows it would no longer produce
// (notify.reconcile), which is the single place that answers moving a reminder, deleting
// one, muting a calendar and every other edit that invalidates the outbox.
func (s *Store) ReplaceReminders(ctx context.Context, eventID *int64, recurrenceID *int64, userID int64, rs []domain.Reminder) error {
	where, args, err := reminderScope(eventID, recurrenceID, userID)
	if err != nil {
		return fmt.Errorf("replace reminders: %w", err)
	}
	for i, r := range rs {
		offsetSet := r.OffsetMinutes != nil
		allDaySet := r.DaysBefore != nil && r.AtTimeLocal != ""
		if offsetSet == allDaySet {
			return fmt.Errorf("replace reminders: reminder %d needs either offset_minutes or days_before+at_time_local: %w",
				i, domain.ErrInvalid)
		}
	}

	err = s.tx(ctx, func(tx *sql.Tx) error {
		// Read back through the same scope, so that what is compared cannot be a
		// wider or narrower set of rows than what is about to be written.
		stored, err := s.withTx(tx).ListReminders(ctx, eventID, recurrenceID, userID)
		if err != nil {
			return err
		}
		// ListReminders orders by id, so each shape's candidates arrive lowest first
		// and stay that way — the whole of what makes the matching below repeatable.
		byShape := map[string][]int64{}
		for _, r := range stored {
			shape := reminderShape(r)
			byShape[shape] = append(byShape[shape], r.ID)
		}
		keep := map[int64]bool{}
		var add []domain.Reminder
		for _, r := range rs {
			shape := reminderShape(r)
			if ids := byShape[shape]; len(ids) > 0 {
				keep[ids[0]] = true
				byShape[shape] = ids[1:]
				continue
			}
			add = append(add, r)
		}

		// A row at a time rather than one statement naming them all: rs arrives from a
		// request and nothing bounds its length, and a list long enough to exceed the
		// parameters a single statement may carry would turn a save that used to work
		// into an error. Each still carries the scope beside the id, so that what a
		// DELETE on this table can reach is legible where the DELETE is rather than by
		// tracing where the id was read.
		for _, r := range stored {
			if keep[r.ID] {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE id = ? AND `+where,
				append([]any{r.ID}, args...)...); err != nil {
				return mapErr(err)
			}
		}
		for _, r := range add {
			var atTime any
			if r.AtTimeLocal != "" {
				atTime = r.AtTimeLocal
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO reminders (event_id, recurrence_id, user_id, offset_minutes, days_before, at_time_local)
				VALUES (?, ?, ?, ?, ?, ?)`,
				putInt64Ptr(eventID), putInt64Ptr(recurrenceID), userID,
				putIntPtr(r.OffsetMinutes), putIntPtr(r.DaysBefore), atTime); err != nil {
				return mapErr(err)
			}
		}
		if eventID != nil {
			// Saving a list against an edited occurrence is what detaches it from its
			// series, and it is recorded here rather than inferred from the rows,
			// because the list saved may be empty — "no reminder, just for this one" —
			// and an empty list is indistinguishable from never having set one.
			// Recording it in the same transaction as the rows is what stops the two
			// disagreeing. The WHERE keeps the table to what it is about: events that
			// are somebody's edited copy of one occurrence of a series.
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO reminder_detachments (event_id, user_id)
				SELECT ?, ? WHERE EXISTS (
					SELECT 1 FROM event_overrides WHERE override_event_id = ?)`,
				*eventID, userID, *eventID); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("replace reminders: %w", err)
	}
	return nil
}

// A member's reminders for an occurrence somebody has edited are their own if they have
// set them on the copy, and the series' otherwise. Two things say they have set them:
// the marker row ReplaceReminders writes, and — for calendars written before that table
// existed — reminders of their own already sitting on the copy. The second is what makes
// the upgrade honest without touching a single row of the family's data: a release
// before this one let the editor write reminders onto a copy (and then announced the
// occurrence twice, once from each list), so those rows are exactly as deliberate as a
// marker, and they are read as such rather than being overruled by the series'.
//
// The two queries below are the same predicate asked in bulk and one at a time;
// TestDetachmentReadsAgreeInBulkAndOneAtATime holds them together.
const detachedPairsSQL = `
	SELECT event_id, user_id FROM reminder_detachments
	UNION
	SELECT r.event_id, r.user_id
	  FROM reminders r
	 WHERE r.event_id IS NOT NULL
	   AND EXISTS (SELECT 1 FROM event_overrides o WHERE o.override_event_id = r.event_id)`

// ReminderDetachment names one edited occurrence and the member whose reminders for it
// are that occurrence's own rather than its series'.
type ReminderDetachment struct {
	EventID int64
	UserID  int64
}

// ListReminderDetachments returns every such pair. The planner reads the whole set once
// per pass, beside the whole set of reminders, because it plans every user in one walk.
func (s *Store) ListReminderDetachments(ctx context.Context) ([]ReminderDetachment, error) {
	rows, err := s.q.QueryContext(ctx, detachedPairsSQL)
	if err != nil {
		return nil, fmt.Errorf("list reminder detachments: %w", mapErr(err))
	}
	defer rows.Close()
	var out []ReminderDetachment
	for rows.Next() {
		var d ReminderDetachment
		if err := rows.Scan(&d.EventID, &d.UserID); err != nil {
			return nil, fmt.Errorf("list reminder detachments: %w", mapErr(err))
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list reminder detachments: %w", mapErr(err))
	}
	return out, nil
}

// RemindersDetached answers the same question for one pair: has this member set the
// reminders for this edited occurrence on the occurrence itself? It is what
// `GET /events/{id}` asks to decide whether to report the copy's list or the series',
// so that the editor shows what will actually fire.
func (s *Store) RemindersDetached(ctx context.Context, eventID, userID int64) (bool, error) {
	var detached bool
	err := s.q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM (`+detachedPairsSQL+`) WHERE event_id = ? AND user_id = ?)`,
		eventID, userID).Scan(&detached)
	if err != nil {
		return false, fmt.Errorf("reminders detached for event %d, user %d: %w", eventID, userID, mapErr(err))
	}
	return detached, nil
}

// ListAllReminders returns every reminder in the database. The planner walks them each
// pass to materialize the next 48 hours of the queue; at family scale that is a few
// hundred rows.
func (s *Store) ListAllReminders(ctx context.Context) ([]domain.Reminder, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+reminderCols+` FROM reminders ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list all reminders: %w", mapErr(err))
	}
	defer rows.Close()
	var out []domain.Reminder
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, fmt.Errorf("list all reminders: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all reminders: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Push subscriptions
// ---------------------------------------------------------------------------

const pushCols = `id, user_id, endpoint, p256dh, auth, ua_label, created_at, last_ok_at, last_confirmed_at, failures`

func scanPush(row rowScanner) (domain.PushSubscription, error) {
	var p domain.PushSubscription
	err := row.Scan(&p.ID, &p.UserID, &p.Endpoint, &p.P256DH, &p.Auth, &p.UALabel,
		instantCol{&p.CreatedAt}, instantCol{&p.LastOKAt}, instantCol{&p.LastConfirmedAt}, &p.Failures)
	if err != nil {
		return domain.PushSubscription{}, mapErr(err)
	}
	return p, nil
}

// UpsertPushSubscription stores a browser's push subscription, keyed on its endpoint.
//
// It is idempotent because the client re-registers on every app open as part of the
// liveness loop: the endpoint is the identity, and a repeat registration refreshes the
// keys, the user it belongs to and the device label. The failure counter resets, since
// a browser that has just handed over a working subscription is by definition not the
// dead one that accumulated those failures.
func (s *Store) UpsertPushSubscription(ctx context.Context, p domain.PushSubscription) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, ua_label, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (endpoint) DO UPDATE SET
			user_id  = excluded.user_id,
			p256dh   = excluded.p256dh,
			auth     = excluded.auth,
			ua_label = excluded.ua_label,
			failures = 0`,
		p.UserID, p.Endpoint, p.P256DH, p.Auth, p.UALabel, mustInstant(s.now()))
	if err != nil {
		return fmt.Errorf("upsert push subscription for user %d: %w", p.UserID, mapErr(err))
	}
	return nil
}

// ListPushSubscriptions returns one user's devices, oldest first.
func (s *Store) ListPushSubscriptions(ctx context.Context, userID int64) ([]domain.PushSubscription, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+pushCols+` FROM push_subscriptions WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions for user %d: %w", userID, mapErr(err))
	}
	return collectPush(rows, fmt.Sprintf("list push subscriptions for user %d", userID))
}

// ListAllPushSubscriptions returns every device. The health check uses it to spot
// subscriptions nothing has confirmed in a fortnight — the signature of a silently
// revoked iOS subscription, which keeps returning success to the sender.
func (s *Store) ListAllPushSubscriptions(ctx context.Context) ([]domain.PushSubscription, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+pushCols+` FROM push_subscriptions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list all push subscriptions: %w", mapErr(err))
	}
	return collectPush(rows, "list all push subscriptions")
}

func collectPush(rows *sql.Rows, what string) ([]domain.PushSubscription, error) {
	defer rows.Close()
	var out []domain.PushSubscription
	for rows.Next() {
		p, err := scanPush(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// DeletePushSubscription drops a subscription by endpoint. Idempotent: the caller is
// usually reacting to a 404 or 410 from the push service, and a concurrent delivery
// attempt may already have pruned the same row.
func (s *Store) DeletePushSubscription(ctx context.Context, endpoint string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint); err != nil {
		return fmt.Errorf("delete push subscription: %w", mapErr(err))
	}
	return nil
}

// ConfirmPushSubscription records that the client checked in and the subscription is
// really still there, clearing the failure counter with it. This is the only signal
// that distinguishes a live subscription from a dead one on iOS.
func (s *Store) ConfirmPushSubscription(ctx context.Context, endpoint string, now time.Time) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE push_subscriptions SET last_confirmed_at = ?, failures = 0 WHERE endpoint = ?`,
		mustInstant(now), endpoint))
	if err != nil {
		return fmt.Errorf("confirm push subscription: %w", err)
	}
	return nil
}

// MarkPushOK records a delivery the push service accepted.
func (s *Store) MarkPushOK(ctx context.Context, id int64, now time.Time) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE push_subscriptions SET last_ok_at = ?, failures = 0 WHERE id = ?`, mustInstant(now), id))
	if err != nil {
		return fmt.Errorf("mark push subscription %d ok: %w", id, err)
	}
	return nil
}

// MarkPushFailure counts a delivery failure. It is a signal for the health check, not
// a pruning rule: a 404/410 is what removes a subscription, and a failure count alone
// never proves a subscription is dead.
func (s *Store) MarkPushFailure(ctx context.Context, id int64) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE push_subscriptions SET failures = failures + 1 WHERE id = ?`, id))
	if err != nil {
		return fmt.Errorf("mark push subscription %d failed: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Notification preferences
// ---------------------------------------------------------------------------

// prefsSelect projects a prefs row with the schema defaults substituted for a user who
// has never opened the settings screen. Keeping the defaults in the SQL rather than in
// Go means Prefs and ListAllPrefs cannot drift apart.
const prefsSelect = `
	SELECT u.id,
	       COALESCE(p.digest_enabled, 1),
	       COALESCE(p.digest_time, '07:30'),
	       COALESCE(p.digest_on_empty, 0),
	       COALESCE(p.daily_summary_mode, 0),
	       COALESCE(p.summary_time, '20:00'),
	       COALESCE(p.email_reminders, 1),
	       COALESCE(p.email_digest, 0),
	       COALESCE(p.activity_push, 1)
	  FROM users u
	  LEFT JOIN notification_prefs p ON p.user_id = u.id`

func scanPrefs(row rowScanner) (domain.NotificationPrefs, error) {
	var p domain.NotificationPrefs
	err := row.Scan(&p.UserID, &p.DigestEnabled, &p.DigestTime, &p.DigestOnEmpty,
		&p.DailySummaryMode, &p.SummaryTime, &p.EmailReminders, &p.EmailDigest, &p.ActivityPush)
	if err != nil {
		return domain.NotificationPrefs{}, mapErr(err)
	}
	return p, nil
}

// Prefs returns a user's notification settings, filling in the schema defaults when
// they have never saved any. It reports domain.ErrNotFound only when the user does not
// exist.
func (s *Store) Prefs(ctx context.Context, userID int64) (domain.NotificationPrefs, error) {
	p, err := scanPrefs(s.q.QueryRowContext(ctx, prefsSelect+` WHERE u.id = ?`, userID))
	if err != nil {
		return domain.NotificationPrefs{}, fmt.Errorf("prefs for user %d: %w", userID, err)
	}
	return p, nil
}

// UpdatePrefs saves a user's notification settings, inserting the row if this is the
// first time they have touched them. Idempotent.
func (s *Store) UpdatePrefs(ctx context.Context, p domain.NotificationPrefs) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO notification_prefs
			(user_id, digest_enabled, digest_time, digest_on_empty, daily_summary_mode,
			 summary_time, email_reminders, email_digest, activity_push)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			digest_enabled     = excluded.digest_enabled,
			digest_time        = excluded.digest_time,
			digest_on_empty    = excluded.digest_on_empty,
			daily_summary_mode = excluded.daily_summary_mode,
			summary_time       = excluded.summary_time,
			email_reminders    = excluded.email_reminders,
			email_digest       = excluded.email_digest,
			activity_push      = excluded.activity_push`,
		p.UserID, boolArg(p.DigestEnabled), p.DigestTime, boolArg(p.DigestOnEmpty),
		boolArg(p.DailySummaryMode), p.SummaryTime, boolArg(p.EmailReminders),
		boolArg(p.EmailDigest), boolArg(p.ActivityPush))
	if err != nil {
		return fmt.Errorf("update prefs for user %d: %w", p.UserID, mapErr(err))
	}
	return nil
}

// ListAllPrefs returns settings for every user, defaults included.
//
// It returns one row per *user*, not one per stored notification_prefs row: the
// planner walks this list to decide who gets a digest, and a family member who never
// opened the settings screen must still get the default-on digest rather than being
// invisible to the planner.
func (s *Store) ListAllPrefs(ctx context.Context) ([]domain.NotificationPrefs, error) {
	rows, err := s.q.QueryContext(ctx, prefsSelect+` ORDER BY u.id`)
	if err != nil {
		return nil, fmt.Errorf("list all prefs: %w", mapErr(err))
	}
	defer rows.Close()
	var out []domain.NotificationPrefs
	for rows.Next() {
		p, err := scanPrefs(rows)
		if err != nil {
			return nil, fmt.Errorf("list all prefs: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all prefs: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The notification outbox
// ---------------------------------------------------------------------------

const queueCols = `id, user_id, kind, source_ref, payload, due_at, sending_started_at, sent_at,
	email_sent_at, skipped, attempts`

func scanQueued(row rowScanner) (domain.QueuedNotification, error) {
	var q domain.QueuedNotification
	var skipped sql.NullString
	err := row.Scan(&q.ID, &q.UserID, &q.Kind, &q.SourceRef, &q.Payload,
		instantCol{&q.DueAt}, instantCol{&q.SendingStartedAt}, instantCol{&q.SentAt},
		instantCol{&q.EmailSentAt}, &skipped, &q.Attempts)
	if err != nil {
		return domain.QueuedNotification{}, mapErr(err)
	}
	q.Skipped = skipped.String
	return q, nil
}

// EnqueueNotification adds a row to the durable outbox, ignoring it if an identical
// one is already queued.
//
// The INSERT OR IGNORE against UNIQUE(user_id, kind, source_ref, due_at) is what makes
// the planner idempotent structurally rather than by being careful: it can recompute
// the same 48-hour window on every 30-second tick, and only genuinely new work lands.
// SourceRef is the stable identity of the thing being announced, e.g.
// "reminder:12:2026-08-04".
func (s *Store) EnqueueNotification(ctx context.Context, q domain.QueuedNotification) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO notification_queue (user_id, kind, source_ref, payload, due_at)
		VALUES (?, ?, ?, ?, ?)`,
		q.UserID, string(q.Kind), q.SourceRef, q.Payload, mustInstant(q.DueAt))
	if err != nil {
		return fmt.Errorf("enqueue %s notification %q: %w", q.Kind, q.SourceRef, mapErr(err))
	}
	return nil
}

// DueNotifications returns unsent, unskipped rows that have come due, oldest first.
func (s *Store) DueNotifications(ctx context.Context, now time.Time, limit int) ([]domain.QueuedNotification, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+queueCols+`
		  FROM notification_queue
		 WHERE sent_at IS NULL AND skipped IS NULL AND due_at <= ?
		 ORDER BY due_at, id
		 LIMIT ?`, mustInstant(now), limit)
	if err != nil {
		return nil, fmt.Errorf("due notifications: %w", mapErr(err))
	}
	return collectQueued(rows, "due notifications")
}

// ListUnsentBefore returns everything still unsent whose slot has already passed. The
// boot catch-up walks it to decide, per row, between a late delivery and a
// stale-skip — a reminder for an event that has not happened yet is still worth
// sending; one for last Tuesday is not.
func (s *Store) ListUnsentBefore(ctx context.Context, t time.Time) ([]domain.QueuedNotification, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+queueCols+`
		  FROM notification_queue
		 WHERE sent_at IS NULL AND skipped IS NULL AND due_at < ?
		 ORDER BY due_at, id`, mustInstant(t))
	if err != nil {
		return nil, fmt.Errorf("list unsent notifications: %w", mapErr(err))
	}
	return collectQueued(rows, "list unsent notifications")
}

func collectQueued(rows *sql.Rows, what string) ([]domain.QueuedNotification, error) {
	defer rows.Close()
	var out []domain.QueuedNotification
	for rows.Next() {
		q, err := scanQueued(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// MarkSending records that delivery has been attempted and counts the attempt.
func (s *Store) MarkSending(ctx context.Context, id int64, now time.Time) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE notification_queue SET sending_started_at = ?, attempts = attempts + 1 WHERE id = ?`,
		mustInstant(now), id))
	if err != nil {
		return fmt.Errorf("mark notification %d sending: %w", id, err)
	}
	return nil
}

// MarkSent records that a provider accepted the message.
//
// It must be called *after* the send, never before. Delivery is at-least-once on
// purpose: a crash in between duplicates a notification, and a duplicate reminder is
// an annoyance where a missed one is the failure this application exists to prevent.
// Do not "fix" duplicates by moving this call earlier.
func (s *Store) MarkSent(ctx context.Context, id int64, now time.Time) error {
	err := affected(s.q.ExecContext(ctx, `UPDATE notification_queue SET sent_at = ? WHERE id = ?`,
		mustInstant(now), id))
	if err != nil {
		return fmt.Errorf("mark notification %d sent: %w", id, err)
	}
	return nil
}

// MarkEmailSent records that the MTA accepted this row's email, which is a
// different fact from the row being finished.
//
// The two channels fail independently, and the one that lies about success is push.
// So the email leg is recorded the moment it is accepted, even when the row stays
// queued because the push leg is still owed — and the next attempt reads this and
// leaves the mail alone. Without it, retrying a row until its event is past would
// mail the family the same reminder on every pass.
func (s *Store) MarkEmailSent(ctx context.Context, id int64, now time.Time) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE notification_queue SET email_sent_at = COALESCE(email_sent_at, ?) WHERE id = ?`,
		mustInstant(now), id))
	if err != nil {
		return fmt.Errorf("mark notification %d email sent: %w", id, err)
	}
	return nil
}

// MarkSkipped retires a queued row without delivering it, recording why — a reminder
// whose event is long past, a digest whose morning slot went by hours ago.
//
// It deliberately leaves sent_at NULL: nothing was sent, and the column means exactly
// that. now is recorded as sending_started_at, so the log still shows when the
// decision was taken.
func (s *Store) MarkSkipped(ctx context.Context, id int64, reason string, now time.Time) error {
	err := affected(s.q.ExecContext(ctx,
		`UPDATE notification_queue SET skipped = ?, sending_started_at = ? WHERE id = ?`,
		reason, mustInstant(now), id))
	if err != nil {
		return fmt.Errorf("mark notification %d skipped: %w", id, err)
	}
	return nil
}

// DeleteUnsentBySourcePrefix drops queued-but-unsent rows whose source_ref starts with
// prefix, and returns how many went.
//
// This is the other half of planner reconciliation: when an occurrence is moved,
// edited or cancelled, the rows already materialized for it have to go, or the family
// gets a reminder for a dentist appointment that is no longer at that time. Sent and
// skipped rows are left alone — they are history.
//
// The prefix is a literal, not a pattern: LIKE wildcards inside it are escaped.
func (s *Store) DeleteUnsentBySourcePrefix(ctx context.Context, prefix string) (int, error) {
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM notification_queue
		 WHERE sent_at IS NULL AND skipped IS NULL AND source_ref LIKE ? ESCAPE '\'`,
		likeEscape(prefix)+"%")
	if err != nil {
		return 0, fmt.Errorf("delete unsent notifications %q: %w", prefix, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete unsent notifications %q: %w", prefix, err)
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Activity log
// ---------------------------------------------------------------------------

const activityCols = `id, calendar_id, user_id, action, event_id, title, at, change_uid`

func scanActivity(row rowScanner) (domain.Activity, error) {
	var a domain.Activity
	var eventID sql.NullInt64
	err := row.Scan(&a.ID, &a.CalendarID, &a.UserID, &a.Action, &eventID, &a.Title,
		instantCol{&a.At}, &a.ChangeUID)
	if err != nil {
		return domain.Activity{}, mapErr(err)
	}
	a.EventID = i64ptr(eventID)
	return a, nil
}

// LogActivity appends to the change log. The store timestamps the entry and names it;
// a.At and a.ChangeUID are ignored.
//
// The name is minted here, beside the timestamp, because it has exactly one job — to
// be unlike every other — and a caller is the wrong place to rely on for that. It is
// what the notification outbox files the entry under, since id will not do: SQLite
// reissues the ids of deleted rows, and an announcement filed under a reused one is
// taken for the announcement already made under it (migration 0006).
//
// crypto/rand.Text is 130 bits and has no error to return; the second half is what
// decides it. A few thousand changes a decade needs nothing like that width, whereas an
// error here would have to travel up through the edit's transaction and fail the edit —
// an appointment that could not be saved because the machine was briefly short of
// randomness.
//
// Title is stored denormalized on purpose: "Claire deleted Dentiste" has to keep
// reading that way after the event is gone, and activity_log.event_id is deliberately
// not a foreign key for the same reason.
func (s *Store) LogActivity(ctx context.Context, a domain.Activity) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO activity_log (calendar_id, user_id, action, event_id, title, at, change_uid)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.CalendarID, a.UserID, string(a.Action), putInt64Ptr(a.EventID), a.Title,
		mustInstant(s.now()), rand.Text())
	if err != nil {
		return fmt.Errorf("log activity %s on calendar %d: %w", a.Action, a.CalendarID, mapErr(err))
	}
	return nil
}

// ListActivity returns the newest entries across the given calendars, oldest-last.
//
// beforeID is the pagination cursor: only entries with a smaller id are returned. A
// zero beforeID means "from the newest". limit defaults to 50 when not positive.
//
// The cursor is an id and not the instant it happened at, because `at` is stored to
// the second and a family can make two changes inside one. An instant cursor drops
// whatever else shared the second the page ended on, and nothing brings it back.
// The id is unique, so a page boundary can fall anywhere without losing a row.
//
// Ordering by id is ordering by when it happened: `at` is written from the same clock
// read that assigns the id, so the two agree unless the machine's clock has stepped
// backwards. When it has, id order is the more truthful of the two — it is the order
// the changes were actually made in.
func (s *Store) ListActivity(ctx context.Context, calendarIDs []int64, limit int, beforeID int64) ([]domain.Activity, error) {
	where := `calendar_id IN (` + placeholders(len(calendarIDs)) + `)`
	args := idArgs(calendarIDs)
	if beforeID > 0 {
		where += ` AND id < ?`
		args = append(args, beforeID)
	}
	return s.listActivity(ctx, calendarIDs, where+` ORDER BY id DESC`, args, limit)
}

// ListActivityAfter returns entries newer than afterID, oldest first.
//
// This is the notification planner's read: its cursor is the last activity row it
// fanned out, and it wants what has happened since, in the order it happened.
func (s *Store) ListActivityAfter(ctx context.Context, calendarIDs []int64, afterID int64, limit int) ([]domain.Activity, error) {
	where := `calendar_id IN (` + placeholders(len(calendarIDs)) + `) AND id > ? ORDER BY id`
	return s.listActivity(ctx, calendarIDs, where, append(idArgs(calendarIDs), afterID), limit)
}

// ListActivityBetween returns the entries whose instant falls in [from, to), newest
// first. A zero from means "from the beginning"; to is required.
//
// This one really is a question about time rather than about order — "what changed
// today", for the daily summary — so it is the one read that still bounds on `at`.
func (s *Store) ListActivityBetween(ctx context.Context, calendarIDs []int64, from, to time.Time, limit int) ([]domain.Activity, error) {
	where := `calendar_id IN (` + placeholders(len(calendarIDs)) + `)`
	args := idArgs(calendarIDs)
	if !from.IsZero() {
		where += ` AND at >= ?`
		args = append(args, mustInstant(from))
	}
	where += ` AND at < ?`
	args = append(args, mustInstant(to))
	return s.listActivity(ctx, calendarIDs, where+` ORDER BY id DESC`, args, limit)
}

// ActivityByID returns one entry from the calendars given, or domain.ErrNotFound.
//
// It is how the notification planner checks its own cursor. That cursor is an
// activity_log id kept outside the table, in meta; activity_log.id is INTEGER PRIMARY
// KEY without AUTOINCREMENT, so SQLite hands the ids of deleted rows out again and the
// number can end up naming a row that has gone, or a different row that has taken its
// place. Neither shows in the number, and neither shows in comparing it against the
// log's current maximum — reused ids climb back towards a stranded cursor and reach it.
// Reading the row back and looking at it is what tells them apart.
func (s *Store) ActivityByID(ctx context.Context, calendarIDs []int64, id int64) (domain.Activity, error) {
	if len(calendarIDs) == 0 {
		return domain.Activity{}, fmt.Errorf("activity %d: %w", id, domain.ErrNotFound)
	}
	a, err := scanActivity(s.q.QueryRowContext(ctx,
		`SELECT `+activityCols+` FROM activity_log
		  WHERE id = ? AND calendar_id IN (`+placeholders(len(calendarIDs))+`)`,
		append([]any{any(id)}, idArgs(calendarIDs)...)...))
	if err != nil {
		return domain.Activity{}, fmt.Errorf("activity %d: %w", id, err)
	}
	return a, nil
}

// listActivity runs one of the activity reads. where carries its own ORDER BY;
// limit defaults to 50 when not positive.
func (s *Store) listActivity(ctx context.Context, calendarIDs []int64, where string, args []any, limit int) ([]domain.Activity, error) {
	if len(calendarIDs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+activityCols+` FROM activity_log WHERE `+where+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", mapErr(err))
	}
	defer rows.Close()
	var out []domain.Activity
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			return nil, fmt.Errorf("list activity: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Holiday overrides and the meta key/value store
// ---------------------------------------------------------------------------

// HolidayOverrides returns the family's corrections to the computed French holidays,
// keyed by date. A nil value suppresses a computed holiday; a non-nil value adds or
// renames one.
//
// French public holidays are computed by internal/holidays rather than stored, so this
// table only has to hold the differences — which is what keeps the calendar right in
// 2040 without a data refresh, and still correctable the year the law changes.
func (s *Store) HolidayOverrides(ctx context.Context) (map[domain.Date]*string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT date, name FROM holiday_overrides ORDER BY date`)
	if err != nil {
		return nil, fmt.Errorf("list holiday overrides: %w", mapErr(err))
	}
	defer rows.Close()
	out := map[domain.Date]*string{}
	for rows.Next() {
		var d domain.Date
		var name sql.NullString
		if err := rows.Scan(&d, &name); err != nil {
			return nil, fmt.Errorf("list holiday overrides: %w", mapErr(err))
		}
		out[d] = strptr(name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list holiday overrides: %w", err)
	}
	return out, nil
}

// SetHolidayOverride adds, renames or (with a nil name) suppresses a holiday.
// Idempotent.
func (s *Store) SetHolidayOverride(ctx context.Context, date domain.Date, name *string) error {
	if date.IsZero() {
		return fmt.Errorf("set holiday override: date must be set: %w", domain.ErrInvalid)
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO holiday_overrides (date, name) VALUES (?, ?)
		ON CONFLICT (date) DO UPDATE SET name = excluded.name`, date, putStrPtr(name))
	if err != nil {
		return fmt.Errorf("set holiday override %s: %w", date, mapErr(err))
	}
	return nil
}

// GetMeta reads a key from the small operational key/value store — the planner's
// materialization horizon, the last backup result, the scheduler heartbeat. A missing
// key is "" and no error: every one of these has a sensible "never happened yet" value,
// and making callers distinguish it from an error buys nothing.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if isNotFound(mapErr(err)) {
			return "", nil
		}
		return "", fmt.Errorf("get meta %q: %w", key, mapErr(err))
	}
	return v, nil
}

// SetMeta writes a key. Idempotent.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set meta %q: %w", key, mapErr(err))
	}
	return nil
}

// ListUnsentByKind returns undelivered notifications of the given kinds whose slot
// falls in [from, to]. The planner uses it to reconcile: rows it would no longer
// create are stale and must go, or a reminder someone deleted still fires.
func (s *Store) ListUnsentByKind(ctx context.Context, from, to time.Time, kinds []domain.NotificationKind) ([]domain.QueuedNotification, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(kinds))
	args := []any{mustInstant(from), mustInstant(to)}
	for i, k := range kinds {
		placeholders[i] = "?"
		args = append(args, string(k))
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+queueCols+`
		  FROM notification_queue
		 WHERE sent_at IS NULL AND skipped IS NULL
		   AND due_at >= ? AND due_at <= ?
		   AND kind IN (`+strings.Join(placeholders, ",")+`)
		 ORDER BY due_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list unsent notifications by kind: %w", mapErr(err))
	}
	return collectQueued(rows, "list unsent notifications by kind")
}

// DeleteQueued removes one undelivered notification. It refuses to touch a row that
// has already been delivered: the outbox is also the record of what was sent.
func (s *Store) DeleteQueued(ctx context.Context, id int64) error {
	_, err := s.q.ExecContext(ctx,
		`DELETE FROM notification_queue WHERE id = ? AND sent_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("delete queued notification %d: %w", id, mapErr(err))
	}
	return nil
}
