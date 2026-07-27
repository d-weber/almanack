package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"almanack/internal/domain"
	"almanack/internal/mailer"
	"almanack/internal/webpush"
)

// Dispatch delivers every queued notification that has come due.
func (n *Notifier) Dispatch(ctx context.Context) error {
	now := n.now()
	if err := n.checkClock(now); err != nil {
		return err
	}
	_, err := n.drain(ctx)
	return err
}

// drainCounts is what one drain did, which is also what CatchUp reports.
type drainCounts struct {
	Delivered int
	Skipped   int
	Deferred  int // left queued for the next tick after a transient failure
}

type outcome int

const (
	outcomeDelivered outcome = iota
	outcomeSkipped
	outcomeDeferred
)

// drain delivers due rows until none are left.
//
// It re-reads the clock on each pass so that a long catch-up run picks up rows
// that fall due while it works, and it stops as soon as a pass retires nothing —
// otherwise a row that keeps failing would be retried in a tight loop instead of
// on the next tick.
func (n *Notifier) drain(ctx context.Context) (drainCounts, error) {
	var c drainCounts
	for pass := 0; pass < maxDrainPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		rows, err := n.st.DueNotifications(ctx, n.now(), drainBatch)
		if err != nil {
			return c, err
		}
		if len(rows) == 0 {
			return c, nil
		}
		retired := 0
		for _, q := range rows {
			switch n.deliver(ctx, q) {
			case outcomeDelivered:
				c.Delivered++
				retired++
			case outcomeSkipped:
				c.Skipped++
				retired++
			default:
				c.Deferred++
			}
		}
		if len(rows) < drainBatch {
			// A short batch was the whole of what was due; anything left in it is
			// a transient failure that belongs to the next tick, not to a tight
			// retry loop that would burn through maxDeliveryAttempts in seconds.
			return c, nil
		}
		if retired == 0 {
			return c, nil
		}
	}
	slog.Warn("notification drain hit its pass limit; the rest will go out on the next tick",
		"delivered", c.Delivered, "skipped", c.Skipped)
	return c, nil
}

// deliver sends one row.
//
// The order here is load-bearing and must not be rearranged: the row is marked
// sending (which counts the attempt), the message is built, the providers are
// called, and only then is sent_at written. Delivery is at-least-once by design —
// a crash between the provider's acceptance and MarkSent duplicates a
// notification, which is the right trade, because a duplicate reminder is an
// annoyance and a missed one is the failure this application exists to prevent.
// Marking before sending would turn this into at-most-once silently.
func (n *Notifier) deliver(ctx context.Context, q domain.QueuedNotification) outcome {
	now := n.now()

	var p payload
	if err := json.Unmarshal([]byte(q.Payload), &p); err != nil {
		// A row nobody can read will never become deliverable; retiring it keeps
		// it from blocking the queue behind it forever.
		n.skip(ctx, q, "unreadable payload", now)
		slog.Error("queued notification has an unreadable payload", "id", q.ID, "source", q.SourceRef, "error", err)
		return outcomeSkipped
	}
	if reason, stale := n.staleness(q, p, now); stale {
		n.skip(ctx, q, reason, now)
		return outcomeSkipped
	}

	if err := n.st.MarkSending(ctx, q.ID, now); err != nil {
		slog.Error("mark notification sending", "id", q.ID, "error", err)
		return outcomeDeferred
	}

	user, err := n.st.UserByID(ctx, q.UserID)
	if err != nil {
		n.skip(ctx, q, "recipient no longer exists", now)
		return outcomeSkipped
	}
	prefs, err := n.st.Prefs(ctx, q.UserID)
	if err != nil {
		slog.Error("load notification prefs", "user", q.UserID, "error", err)
		return outcomeDeferred
	}
	to := recipient{user: user, prefs: prefs}

	if q.Kind == domain.KindSummary {
		// A summary's content does not exist when its row is planned, so it is
		// resolved now. A day with nothing to report is skipped rather than
		// pushed: "0 changements aujourd'hui" is not worth a notification.
		filled, err := n.fillSummary(ctx, q.UserID, p)
		if err != nil {
			slog.Error("build activity summary", "user", q.UserID, "day", p.Day, "error", err)
			return outcomeDeferred
		}
		if filled.Total == 0 {
			n.skip(ctx, q, "no activity to summarise", now)
			return outcomeSkipped
		}
		p = filled
	}

	body, email, err := n.render(p, to, q.SourceRef, now)
	if err != nil {
		n.skip(ctx, q, "cannot compose notification", now)
		slog.Error("compose notification", "id", q.ID, "kind", q.Kind, "error", err)
		return outcomeSkipped
	}

	attempted, accepted := n.send(ctx, q, p, to, body, email, now)

	switch {
	case accepted > 0 || attempted == 0:
		// attempted == 0 means the person has no channel at all — no device, no
		// email preference. There is nothing to retry, and leaving the row queued
		// would make it immortal.
		if err := n.st.MarkSent(ctx, q.ID, n.now()); err != nil {
			slog.Error("mark notification sent", "id", q.ID, "error", err)
			return outcomeDeferred
		}
		return outcomeDelivered
	case q.Attempts+1 >= maxDeliveryAttempts:
		n.skip(ctx, q, fmt.Sprintf("undeliverable after %d attempts", q.Attempts+1), now)
		return outcomeSkipped
	default:
		return outcomeDeferred
	}
}

// send fans one message out over both channels and reports how many providers
// were asked and how many accepted.
//
// Email is sent whether or not the push succeeded. It is a parallel channel, not
// a fallback: an iOS push subscription dies silently with the push service still
// returning 201, so waiting for a delivery error before sending mail would wait
// forever (the notification rules in docs/architecture.md).
func (n *Notifier) send(ctx context.Context, q domain.QueuedNotification, p payload, to recipient,
	body []byte, email *mailerMessage, now time.Time) (attempted, accepted int) {

	if n.push != nil {
		subs, err := n.st.ListPushSubscriptions(ctx, to.user.ID)
		if err != nil {
			slog.Error("list push subscriptions", "user", to.user.ID, "error", err)
		}
		opts := pushOptions(q.Kind, p, now)
		for _, sub := range subs {
			err := n.push.Send(ctx, sub, body, opts)
			switch {
			case err == nil:
				attempted++
				accepted++
				n.notePush(sub.Endpoint, nil)
				if err := n.st.MarkPushOK(ctx, sub.ID, now); err != nil {
					slog.Error("mark push ok", "subscription", sub.ID, "error", err)
				}
			case errors.Is(err, webpush.ErrGone):
				// 404/410 is terminal: the push service has forgotten this
				// subscription and will answer the same way forever. It is not a
				// service failure and not something to retry — the row goes.
				n.notePush(sub.Endpoint, nil)
				if err := n.st.DeletePushSubscription(ctx, sub.Endpoint); err != nil {
					slog.Error("delete dead push subscription", "subscription", sub.ID, "error", err)
				}
				slog.Info("push subscription is gone, deleted", "subscription", sub.ID,
					"user", to.user.ID, "service", serviceOf(sub.Endpoint))
			default:
				attempted++
				n.notePush(sub.Endpoint, err)
				if err := n.st.MarkPushFailure(ctx, sub.ID); err != nil {
					slog.Error("mark push failure", "subscription", sub.ID, "error", err)
				}
				slog.Warn("push delivery failed", "subscription", sub.ID,
					"service", serviceOf(sub.Endpoint), "kind", q.Kind, "error", err)
			}
		}
	}

	if n.mail != nil && email != nil && email.To != "" {
		attempted++
		if err := n.mail.Send(ctx, mailer.Message{To: email.To, Subject: email.Subject, Text: email.Text}); err != nil {
			slog.Error("email delivery failed", "user", to.user.ID, "kind", q.Kind, "error", err)
		} else {
			accepted++
		}
	}
	return attempted, accepted
}

// pushOptions is the header matrix of the notification rules in docs/architecture.md.
//
//	reminders  TTL min(time-to-event, 6 h)  Urgency high    — it is worthless after the event
//	digests    TTL 6 h                      Urgency normal  + Topic, so a superseded digest
//	                                                          collapses instead of stacking
//	summaries  TTL 6 h                      Urgency low     + Topic, same reasoning
//	activity   TTL 24 h                     Urgency low
func pushOptions(kind domain.NotificationKind, p payload, now time.Time) webpush.Options {
	switch kind {
	case domain.KindReminder:
		return webpush.Options{TTL: reminderTTL(p, now), Urgency: webpush.UrgencyHigh}
	case domain.KindDigest:
		return webpush.Options{TTL: 6 * time.Hour, Urgency: webpush.UrgencyNormal, Topic: "digest"}
	case domain.KindSummary:
		return webpush.Options{TTL: 6 * time.Hour, Urgency: webpush.UrgencyLow, Topic: "summary"}
	default:
		return webpush.Options{TTL: 24 * time.Hour, Urgency: webpush.UrgencyLow}
	}
}

// minReminderTTL keeps a TTL off zero. TTL: 0 tells the push service to discard
// the message unless the device happens to be connected at that instant, which is
// never what a reminder wants — least of all a late one for a phone that has been
// in a pocket all morning.
const minReminderTTL = time.Minute

func reminderTTL(p payload, now time.Time) time.Duration {
	const cap = 6 * time.Hour
	var until time.Duration
	switch {
	case p.AllDay && !p.EventDate.IsZero():
		until = cap // an all-day event is relevant for the whole day
	case !p.EventStart.IsZero():
		until = p.EventStart.Sub(now)
	default:
		until = cap
	}
	if until > cap {
		until = cap
	}
	if until < minReminderTTL {
		until = minReminderTTL
	}
	return until
}

// staleness is the notification rules in docs/architecture.md, applied to every row rather than only
// at boot — a row can fall behind for reasons other than an outage, and the answer
// is the same either way.
//
// Rows that are barely late are always delivered: in normal operation a tick lands
// a few seconds after the slot, and testing "is the event still ahead?" on those
// would skip every reminder set for the moment the event starts.
func (n *Notifier) staleness(q domain.QueuedNotification, p payload, now time.Time) (reason string, stale bool) {
	late := now.Sub(q.DueAt)
	if late <= lateThreshold {
		return "", false
	}
	switch q.Kind {
	case domain.KindReminder:
		if p.eventStillAhead(now, n.loc) {
			return "", false // a late warning beats no warning
		}
		return fmt.Sprintf("event already past, reminder was due %s ago", late.Round(time.Minute)), true
	case domain.KindDigest, domain.KindSummary:
		if late < maxDigestLateness {
			return "", false
		}
		return fmt.Sprintf("slot passed %s ago", late.Round(time.Minute)), true
	case domain.KindActivity:
		if late < maxActivityLateness {
			return "", false
		}
		return fmt.Sprintf("change happened %s ago", late.Round(time.Minute)), true
	}
	return "", false
}

// skip retires a row without delivering it, recording why. Skipped rows are kept,
// not deleted: "we decided not to send this, and here is the reason" is the only
// evidence a missed notification was a decision rather than a bug.
func (n *Notifier) skip(ctx context.Context, q domain.QueuedNotification, reason string, now time.Time) {
	if err := n.st.MarkSkipped(ctx, q.ID, reason, now); err != nil {
		slog.Error("mark notification skipped", "id", q.ID, "error", err)
		return
	}
	slog.Info("notification skipped", "id", q.ID, "kind", q.Kind, "source", q.SourceRef,
		"user", q.UserID, "due", q.DueAt.Format(time.RFC3339), "reason", reason)
}

// fillSummary counts the day's changes across the calendars this user can see,
// excluding their own and the ones they have muted.
func (n *Notifier) fillSummary(ctx context.Context, userID int64, p payload) (payload, error) {
	if p.Day.IsZero() {
		return p, nil
	}
	cals, err := n.st.ListCalendarsForUser(ctx, userID)
	if err != nil {
		return p, err
	}
	var ids []int64
	for _, c := range cals {
		m, err := n.st.Membership(ctx, c.ID, userID)
		if err != nil {
			return p, err
		}
		if !m.Muted {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return p, nil
	}

	dayStart := p.Day.In(n.loc)
	dayEnd := p.Day.AddDays(1).In(n.loc)
	acts, err := n.st.ListActivity(ctx, ids, 500, dayEnd)
	if err != nil {
		return p, err
	}
	for _, a := range acts {
		if a.At.Before(dayStart) {
			break // the feed is newest-first, so this is the end of the day
		}
		if a.UserID == userID {
			continue
		}
		p.Total++
	}
	return p, nil
}
