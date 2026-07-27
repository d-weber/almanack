package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"almanack/internal/domain"
	"almanack/internal/events"
)

// Plan materializes everything that comes due between now and the horizon.
//
// It is safe to call as often as you like: UNIQUE(user_id, kind, source_ref,
// due_at) with INSERT OR IGNORE means re-planning the same window inserts
// nothing, and a row that has already been sent or skipped is never resurrected.
// That structural idempotency is why the scheduler can simply re-plan on every
// tick instead of tracking what it has already done.
func (n *Notifier) Plan(ctx context.Context) error {
	now := n.now()
	if err := n.checkClock(now); err != nil {
		return err
	}
	return n.plan(ctx, now, now.Add(n.horizon))
}

// plan enqueues every notification whose slot falls in [from, to]. CatchUp calls
// it with a `from` in the past to backfill the gap an outage left.
//
// One failing user or one malformed reminder must not stop the pass: errors are
// collected and returned together, and the horizon marker is only advanced when
// the whole pass was clean, so a partial failure is retried rather than sealed
// behind a marker that says the work was done.
func (n *Notifier) plan(ctx context.Context, from, to time.Time) error {
	if to.Before(from) {
		from = to
	}
	prefs, err := n.st.ListAllPrefs(ctx)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	byUser := make(map[int64]domain.NotificationPrefs, len(prefs))
	for _, p := range prefs {
		byUser[p.UserID] = p
	}

	n.planned = map[string]bool{}
	defer func() { n.planned = nil }()

	errs := []error{
		n.planReminders(ctx, from, to),
		n.planDigests(ctx, from, to, prefs),
		n.planSummaries(ctx, from, to, prefs),
		n.planActivity(ctx, byUser),
	}
	if err := errors.Join(errs...); err != nil {
		// A partial pass has an incomplete picture of what should exist, so it
		// must not delete anything.
		return fmt.Errorf("plan: %w", err)
	}
	if err := n.reconcile(ctx, from, to); err != nil {
		return fmt.Errorf("plan: reconcile outbox: %w", err)
	}
	if err := n.st.SetMeta(ctx, MetaPlannedThrough, to.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("plan: record horizon: %w", err)
	}
	return nil
}

// plannedThrough reads how far the planner last got. A missing or unparseable
// value is treated as "never planned", which makes CatchUp a no-op backfill on a
// fresh database rather than a walk back to the epoch.
func (n *Notifier) plannedThrough(ctx context.Context) (time.Time, error) {
	v, err := n.st.GetMeta(ctx, MetaPlannedThrough)
	if err != nil {
		return time.Time{}, err
	}
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		slog.Warn("planner horizon marker is unreadable, treating it as unset", "value", v, "error", err)
		return time.Time{}, nil
	}
	return t.UTC(), nil
}

func (n *Notifier) enqueue(ctx context.Context, userID int64, kind domain.NotificationKind, sourceRef string, p payload, due time.Time) error {
	body, err := encodePayload(p)
	if err != nil {
		return err
	}
	at := due.UTC().Truncate(time.Second)
	if n.planned != nil {
		n.planned[queueKey(userID, kind, sourceRef, at)] = true
	}
	return n.st.EnqueueNotification(ctx, domain.QueuedNotification{
		UserID:    userID,
		Kind:      kind,
		SourceRef: sourceRef,
		Payload:   body,
		DueAt:     at,
	})
}

// queueKey mirrors the outbox's UNIQUE constraint.
func queueKey(userID int64, kind domain.NotificationKind, sourceRef string, due time.Time) string {
	return fmt.Sprintf("%d|%s|%s|%s", userID, kind, sourceRef, due.UTC().Format(time.RFC3339))
}

// reconcilable are the kinds the planner recomputes in full on every pass, and
// therefore the only ones it may delete. Activity notifications are event-sourced
// through a cursor rather than recomputed, so a pass that no longer produces one
// says nothing about whether it is still wanted.
var reconcilable = []domain.NotificationKind{
	domain.KindReminder, domain.KindDigest, domain.KindSummary,
}

// reconcile deletes undelivered rows in the planned window that this pass would no
// longer create. Adding was never the hard part: five different edits invalidate
// the outbox — changing a reminder, moving an event, muting a calendar, switching
// the digest off, moving its time — and expecting each of them to remember to
// prune is how a reminder someone deleted still goes off. Recomputing the window
// and removing whatever is no longer in it covers all of them at once, including
// the ones nobody has thought of yet.
func (n *Notifier) reconcile(ctx context.Context, from, to time.Time) error {
	rows, err := n.st.ListUnsentByKind(ctx, from, to, reconcilable)
	if err != nil {
		return err
	}
	removed := 0
	for _, row := range rows {
		if n.planned[queueKey(row.UserID, row.Kind, row.SourceRef, row.DueAt)] {
			continue
		}
		if err := n.st.DeleteQueued(ctx, row.ID); err != nil {
			return err
		}
		removed++
		slog.Debug("dropped a notification the plan no longer calls for",
			"kind", row.Kind, "source", row.SourceRef, "due", row.DueAt)
	}
	if removed > 0 {
		slog.Info("outbox reconciled", "dropped", removed)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reminders
// ---------------------------------------------------------------------------

// planReminders walks every reminder in the database — a few hundred rows at
// family scale — grouped by owner, because the visibility rules that decide
// whether a person may be reminded about an occurrence are per person.
func (n *Notifier) planReminders(ctx context.Context, from, to time.Time) error {
	all, err := n.st.ListAllReminders(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	byUser := map[int64][]domain.Reminder{}
	for _, r := range all {
		byUser[r.UserID] = append(byUser[r.UserID], r)
	}
	users := make([]int64, 0, len(byUser))
	for id := range byUser {
		users = append(users, id)
	}
	sort.Slice(users, func(i, j int) bool { return users[i] < users[j] })

	var errs []error
	for _, userID := range users {
		if err := n.planUserReminders(ctx, userID, byUser[userID], from, to); err != nil {
			errs = append(errs, fmt.Errorf("reminders for user %d: %w", userID, err))
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) planUserReminders(ctx context.Context, userID int64, rs []domain.Reminder, from, to time.Time) error {
	byEvent := map[int64][]domain.Reminder{}
	byRecurrence := map[int64][]domain.Reminder{}
	var maxLead time.Duration
	for _, r := range rs {
		switch {
		case r.EventID != nil:
			byEvent[*r.EventID] = append(byEvent[*r.EventID], r)
		case r.RecurrenceID != nil:
			byRecurrence[*r.RecurrenceID] = append(byRecurrence[*r.RecurrenceID], r)
		default:
			continue // the schema's CHECK forbids this; be defensive anyway
		}
		if l := lead(r); l > maxLead {
			maxLead = l
		}
	}

	// A reminder fires before its occurrence, so the occurrences that can produce
	// a slot inside [from, to] reach as far ahead as the longest lead. Bounding
	// the expansion window by the actual reminders rather than by a guess is what
	// keeps "two weeks before Maman's birthday" working without expanding a year
	// of recurrences on every tick.
	//
	// The extra day on each side covers all-day reminders, whose slot can land up
	// to a day after the start of the day they are anchored to.
	fromDate := domain.DateIn(from, n.loc).AddDays(-1)
	toDate := domain.DateIn(to.Add(maxLead), n.loc).AddDays(1)

	occs, err := n.ev.UserOccurrences(ctx, userID, fromDate, toDate)
	if err != nil {
		return err
	}

	var errs []error
	for _, occ := range occs {
		// Source references are keyed on the *series* event for a recurring
		// occurrence, because that is the id internal/events prunes by when an
		// occurrence moves or the series changes shape.
		sourceEvent := occ.Event.ID
		if occ.SeriesEventID != nil {
			sourceEvent = *occ.SeriesEventID
		}

		// An edited occurrence owns its reminders. Editing one occurrence copies the
		// series' reminders onto the standalone event the edit leaves behind (see
		// internal/events.updateOccurrence) and that copy is what the editor lists and
		// what a PUT of the reminder list writes to — so counting the series' as well
		// meant two rows, with different reminder ids, for one swimming lesson: two
		// identical pushes. It also meant the reminder somebody had just removed from
		// this one occurrence went off anyway, from the series, while the screen said
		// there was none. The copy is the whole answer for its date.
		//
		// A reminder is scoped to an event or to a recurrence and never to both — the
		// schema's CHECK says so — so the two lists cannot overlap and nothing has to
		// be deduplicated between them.
		var cands []domain.Reminder
		cands = append(cands, byEvent[occ.Event.ID]...)
		if !occ.IsOverride && occ.RecurrenceID != nil {
			cands = append(cands, byRecurrence[*occ.RecurrenceID]...)
		}

		for _, r := range cands {
			due, ok := n.reminderDue(occ, r)
			if !ok {
				continue
			}
			if due.After(to) {
				continue
			}
			p := payload{
				Kind:     domain.KindReminder,
				EventID:  sourceEvent,
				OccDate:  occ.OccurrenceDate,
				Title:    truncateRunes(occ.Title, maxTitleRunes),
				Location: truncateRunes(occ.Location, maxTitleRunes),
				AllDay:   occ.AllDay,
			}
			if occ.AllDay {
				p.EventDate = occ.StartDate
			} else {
				p.EventStart = occ.StartsAt.UTC()
			}
			// A slot already in the past is still queued, as long as the event it
			// warns about has not happened yet. Refusing here is how an appointment
			// added twenty minutes before it starts — with a thirty-minute reminder,
			// so its slot is already gone — produced no notification at all, on this
			// tick or any later one. Whether a late row is worth delivering is a
			// question the delivery path already answers, in one place: staleness()
			// says a late warning beats no warning while the event is ahead.
			if due.Before(from) && !p.eventStillAhead(from, n.loc) {
				continue
			}
			ref := events.ReminderSourceRef(sourceEvent, occ.OccurrenceDate, r.ID)
			if err := n.enqueue(ctx, userID, domain.KindReminder, ref, p, due); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// lead is how far ahead of its occurrence a reminder fires, used only to size the
// expansion window. The all-day case adds a day because at_time_local can be late
// in the day the reminder is anchored to.
func lead(r domain.Reminder) time.Duration {
	switch {
	case r.OffsetMinutes != nil:
		if d := time.Duration(*r.OffsetMinutes) * time.Minute; d > 0 {
			return d
		}
	case r.DaysBefore != nil:
		if *r.DaysBefore >= 0 {
			return time.Duration(*r.DaysBefore+1) * 24 * time.Hour
		}
	}
	return 0
}

// reminderDue computes the instant a reminder fires for one occurrence.
//
// The two shapes are stored differently because they are different: a timed event
// is "N minutes before the start", which is an offset in absolute time; an all-day
// event is "09:00, D days before", which is a wall-clock time in the family
// timezone and cannot be expressed as an offset from midnight. Doing the all-day
// arithmetic in local time is also what makes it survive a DST change: 09:00 stays
// 09:00 whether that is 08:00 or 07:00 UTC.
//
// The cross-shape cases (a timed reminder left behind on an event that was turned
// all-day, or the reverse) are handled rather than dropped. A reminder that stops
// firing because someone ticked "journée entière" is exactly the silent failure
// this application exists to prevent.
func (n *Notifier) reminderDue(occ domain.Occurrence, r domain.Reminder) (time.Time, bool) {
	if occ.AllDay {
		d := occ.StartDate
		if d.IsZero() {
			return time.Time{}, false
		}
		if r.DaysBefore != nil {
			h, m, ok := parseHM(r.AtTimeLocal)
			if !ok {
				slog.Warn("reminder has an unreadable at_time_local", "reminder", r.ID, "value", r.AtTimeLocal)
				return time.Time{}, false
			}
			return d.AddDays(-*r.DaysBefore).At(h, m, n.loc), true
		}
		if r.OffsetMinutes != nil {
			return d.In(n.loc).Add(-time.Duration(*r.OffsetMinutes) * time.Minute), true
		}
		return time.Time{}, false
	}

	if occ.StartsAt.IsZero() {
		return time.Time{}, false
	}
	if r.OffsetMinutes != nil {
		return occ.StartsAt.Add(-time.Duration(*r.OffsetMinutes) * time.Minute), true
	}
	if r.DaysBefore != nil {
		h, m, ok := parseHM(r.AtTimeLocal)
		if !ok {
			slog.Warn("reminder has an unreadable at_time_local", "reminder", r.ID, "value", r.AtTimeLocal)
			return time.Time{}, false
		}
		return domain.DateIn(occ.StartsAt, n.loc).AddDays(-*r.DaysBefore).At(h, m, n.loc), true
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// Digests
// ---------------------------------------------------------------------------

// planDigests queues one row per enabled user per day, at their chosen local time.
//
// Its payload names the day and nothing else, for the same reason a summary's
// does: the row is materialized up to 48 hours ahead, and the day it announces
// keeps changing until the moment it is announced. An agenda written in here
// could never be corrected — the outbox is INSERT OR IGNORE on
// (user, kind, source_ref, due_at), so a later pass produces the same key and
// changes nothing. The agenda, and the question of whether a quiet day is worth
// a push at all, are resolved at delivery.
func (n *Notifier) planDigests(ctx context.Context, from, to time.Time, prefs []domain.NotificationPrefs) error {
	var errs []error
	days := daysCovering(from, to, n.loc)
	for _, p := range prefs {
		if !p.DigestEnabled {
			continue
		}
		h, m, ok := parseHM(p.DigestTime)
		if !ok {
			slog.Warn("digest time is unreadable, skipping this user", "user", p.UserID, "value", p.DigestTime)
			continue
		}
		for _, day := range days {
			// The slot is a wall-clock time in the family timezone, so it lands
			// on a different UTC instant either side of a DST change — which is
			// the whole point: 07:30 is 07:30 in March and in November.
			due := day.At(h, m, n.loc)
			if due.Before(from) || due.After(to) {
				continue
			}
			pl := payload{Kind: domain.KindDigest, Day: day}
			if err := n.enqueue(ctx, p.UserID, domain.KindDigest, events.DigestSourceRef(day), pl, due); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Summaries
// ---------------------------------------------------------------------------

// planSummaries queues the batched activity notification for users who asked for
// one instead of a push per change.
//
// Its payload is deliberately empty of content: the changes a summary reports have
// not happened yet when the row is materialized up to 48 hours ahead. The count and
// the wording are resolved at delivery time, and a day with nothing to report is
// skipped there rather than pushed.
func (n *Notifier) planSummaries(ctx context.Context, from, to time.Time, prefs []domain.NotificationPrefs) error {
	var errs []error
	days := daysCovering(from, to, n.loc)
	for _, p := range prefs {
		if !p.DailySummaryMode {
			continue
		}
		h, m, ok := parseHM(p.SummaryTime)
		if !ok {
			slog.Warn("summary time is unreadable, skipping this user", "user", p.UserID, "value", p.SummaryTime)
			continue
		}
		for _, day := range days {
			due := day.At(h, m, n.loc)
			if due.Before(from) || due.After(to) {
				continue
			}
			pl := payload{Kind: domain.KindSummary, Day: day}
			if err := n.enqueue(ctx, p.UserID, domain.KindSummary, events.SummarySourceRef(day), pl, due); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

// activityCatchUpLimit caps one pass. Between two 30-second ticks a family produces
// a handful of rows; the number is for the pass that follows a long outage. Rows past
// it are not lost — the cursor stops at the last one handled, so the next tick picks
// up exactly where this one left off.
const activityCatchUpLimit = 2000

// planActivity turns new activity_log rows into notifications for the members they
// concern.
//
// Notifying from the log rather than from the edit itself is what makes the
// pipeline crash-proof: the edit and its log row land in the same transaction, so
// a crash between the edit and the notification loses nothing — the next pass
// finds the row still sitting past the cursor. That transaction is real and is
// tested for; see Service.inTx in internal/events, which every edit goes through.
//
// Activity rows are exempt from the [from, to] window: their slot is when the
// change happened, which is always in the past, and they are meant to go out on
// the next tick.
func (n *Notifier) planActivity(ctx context.Context, prefs map[int64]domain.NotificationPrefs) error {
	cals, err := n.allCalendarIDs(ctx)
	if err != nil {
		return err
	}
	if len(cals) == 0 {
		return nil
	}
	raw, err := n.st.GetMeta(ctx, MetaActivityCursor)
	if err != nil {
		return err
	}
	if raw == "" {
		// First ever pass. Start from the present: replaying a decade of history
		// into everyone's notification tray is not a welcome.
		newest, err := n.st.ListActivity(ctx, cals, 1, 0)
		if err != nil {
			return err
		}
		var start int64
		if len(newest) > 0 {
			start = newest[0].ID
		}
		return n.st.SetMeta(ctx, MetaActivityCursor, strconv.FormatInt(start, 10))
	}
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("activity cursor %q is unreadable: %w", raw, err)
	}
	cursor, err = n.repairCursor(ctx, cals, cursor)
	if err != nil {
		return err
	}

	acts, err := n.st.ListActivityAfter(ctx, cals, cursor, activityCatchUpLimit)
	if err != nil {
		return err
	}

	actors := map[int64]string{}
	calendars := map[int64]string{}
	highest := cursor
	var errs []error
	for _, a := range acts {
		if err := n.planOneActivity(ctx, a, prefs, actors, calendars); err != nil {
			// Stop the batch rather than skip the row. The cursor is one
			// high-water mark, so carrying on would let the next row that succeeded
			// set it past this one — and nothing ever reads a row the cursor has
			// passed, so INSERT OR IGNORE gets no second chance at it and the change
			// is announced to nobody. The error is still collected, so the pass
			// reports failure and the rest of the batch goes out on the next tick.
			//
			// Stopping cannot wedge the feed, because no row can fail forever: a
			// deleted calendar takes its activity rows with it (ON DELETE CASCADE)
			// and drops out of cals, so what is left is store I/O; and the two reads
			// that legitimately come up empty — the actor and the participant list —
			// are already handled without an error.
			errs = append(errs, err)
			break
		}
		if a.ID > highest {
			highest = a.ID
		}
	}
	if highest != cursor {
		// The cursor moves only after the rows are durably in the outbox, so a
		// crash in between re-reads them and INSERT OR IGNORE absorbs the repeat.
		if err := n.st.SetMeta(ctx, MetaActivityCursor, strconv.FormatInt(highest, 10)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// repairCursor drops a stored cursor that has been stranded above the end of the log,
// and returns the one to read from.
//
// activity_log.id is INTEGER PRIMARY KEY without AUTOINCREMENT, so SQLite hands the ids
// of deleted rows out again, and activity rows cascade away with the calendar or the
// user they belong to. Delete the calendar holding the newest entries and the remembered
// cursor sits above every id the log will produce next: each new change is logged
// *behind* the cursor, nothing ever reads it, and activity notifications stay silent
// until the log climbs back to the old high-water mark. Nothing else the cursor is
// compared against notices, because within the surviving rows a reused id still sorts
// correctly — it is only the number kept outside the table that goes stale.
//
// Resetting to the log's highest id is the obvious repair and quietly loses the changes
// logged between the deletion and this pass: those carry reused ids below the stranded
// cursor and, by id alone, are indistinguishable from the rows that were announced
// before it. So the reset goes further back, to the last row old enough that delivery
// would refuse it anyway (maxActivityLateness). Everything after that is re-walked,
// which costs nothing the design does not already rely on — UNIQUE(user, kind,
// source_ref, due_at) with INSERT OR IGNORE absorbs a row that was announced already —
// and no change anybody still wants to hear about is stepped over. The bound matters
// too: without it a family with a year of history would re-walk all of it and file a
// year of stale rows to be skipped one by one.
func (n *Notifier) repairCursor(ctx context.Context, cals []int64, cursor int64) (int64, error) {
	newest, err := n.st.ListActivity(ctx, cals, 1, 0)
	if err != nil {
		return 0, err
	}
	var highest int64
	if len(newest) > 0 {
		highest = newest[0].ID
	}
	if cursor <= highest {
		return cursor, nil
	}

	var reset int64
	settled, err := n.st.ListActivityBetween(ctx, cals, time.Time{}, n.now().Add(-maxActivityLateness), 1)
	if err != nil {
		return 0, err
	}
	if len(settled) > 0 {
		reset = settled[0].ID
	}
	slog.Warn("the activity cursor was above the end of the change log and has been reset",
		"cursor", cursor, "newest_logged", highest, "reset_to", reset)
	if err := n.st.SetMeta(ctx, MetaActivityCursor, strconv.FormatInt(reset, 10)); err != nil {
		return 0, err
	}
	return reset, nil
}

func (n *Notifier) planOneActivity(ctx context.Context, a domain.Activity, prefs map[int64]domain.NotificationPrefs,
	actors, calendars map[int64]string) error {

	members, err := n.st.ListMembers(ctx, a.CalendarID)
	if err != nil {
		return fmt.Errorf("members of calendar %d: %w", a.CalendarID, err)
	}
	if _, ok := actors[a.UserID]; !ok {
		u, err := n.st.UserByID(ctx, a.UserID)
		if err != nil {
			// A removed member's changes still happened; name them nothing
			// rather than dropping the notification.
			actors[a.UserID] = ""
		} else {
			actors[a.UserID] = u.DisplayName
		}
	}
	if _, ok := calendars[a.CalendarID]; !ok {
		c, err := n.st.CalendarByID(ctx, a.CalendarID)
		if err != nil {
			return fmt.Errorf("calendar %d: %w", a.CalendarID, err)
		}
		calendars[a.CalendarID] = c.Name
	}

	var participants []int64
	if a.EventID != nil && a.Action != domain.ActionEventDeleted {
		if ps, err := n.st.ListParticipants(ctx, *a.EventID); err == nil {
			participants = ps
		}
	}

	p := payload{
		Kind:       domain.KindActivity,
		ActivityID: a.ID,
		Action:     a.Action,
		Actor:      actors[a.UserID],
		Calendar:   truncateRunes(calendars[a.CalendarID], maxTitleRunes),
		Title:      truncateRunes(a.Title, maxTitleRunes),
	}
	if a.EventID != nil {
		p.EventID = *a.EventID
	}
	ref := events.ActivitySourceRef(a.ID)

	var errs []error
	for _, m := range members {
		switch {
		case m.UserID == a.UserID: // nobody needs telling what they just did
			continue
		case m.Muted:
			continue
		}
		pr, ok := prefs[m.UserID]
		if !ok || !pr.ActivityPush || pr.DailySummaryMode {
			// Summary mode replaces individual activity pushes with one batched
			// row per day; sending both would defeat the setting.
			continue
		}
		// A deleted event has no participant rows left to check, so
		// participating-only cannot filter it; the change still concerns the
		// calendar and goes out.
		if m.ParticipatingOnly && participants != nil && !containsID(participants, m.UserID) {
			continue
		}
		if err := n.enqueue(ctx, m.UserID, domain.KindActivity, ref, p, a.At); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// allCalendarIDs is the union of every user's calendars.
//
// The store has no "list all calendars" — every read is scoped to a member, which
// is the right default for an authorisation-sensitive API. The planner is the one
// caller that legitimately needs the whole set, and at family scale the union of
// ten memberships is cheaper than adding an unscoped query somebody could later
// reach for from a handler.
func (n *Notifier) allCalendarIDs(ctx context.Context) ([]int64, error) {
	users, err := n.st.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []int64
	for _, u := range users {
		cals, err := n.st.ListCalendarsForUser(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range cals {
			if !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c.ID)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
