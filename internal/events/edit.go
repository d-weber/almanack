package events

import (
	"time"

	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"almanack/internal/domain"
	"almanack/internal/recur"
	"almanack/internal/store"
)

// Input is the whole editable state of an event. Handlers fill it from JSON; the
// service is what decides how it lands on the database.
type Input struct {
	CalendarID   int64
	Title        string
	AllDay       bool
	StartsAt     time.Time
	EndsAt       time.Time
	StartDate    domain.Date
	EndDate      domain.Date
	Location     string
	URL          string
	Notes        string
	LabelID      int64
	Participants []int64
	// Recurrence is the repeat pattern, or nil for a one-off. Its DTStart is set by
	// the service from the event's own start, so callers never have to keep the two
	// in agreement.
	Recurrence *domain.Recurrence
}

// inTx runs fn inside one database transaction, against a copy of the Service whose
// store writes through it. Every flow in this file that writes more than once uses it.
//
// The writes a scoped edit makes are not independent: "this and following" ends the old
// series and creates its replacement, and every one of those statements runs on the
// request's context. A phone that loses its connection part-way through therefore
// really does stop the sequence, and without a transaction it stopped it wherever it
// happened to be — most of a series gone, with an error on screen that said nothing
// about it. The activity row goes in here too, because internal/notify plans the
// change notification from the log rather than from the edit, and a committed edit
// whose log row was lost is a change nobody is ever told about.
//
// fn is handed the transaction-scoped Service and deliberately shadows the outer one:
// a write through the Service this was called on would go straight to the pool, outside
// the transaction, and survive the rollback.
func (s *Service) inTx(ctx context.Context, fn func(*Service) error) error {
	return s.st.InTx(ctx, func(st *store.Store) error {
		scoped := *s
		scoped.st = st
		return fn(&scoped)
	})
}

// inTxEvent is inTx for the flows that answer with the event they wrote.
func (s *Service) inTxEvent(ctx context.Context, fn func(*Service) (domain.Event, error)) (domain.Event, error) {
	var out domain.Event
	err := s.inTx(ctx, func(s *Service) error {
		var err error
		out, err = fn(s)
		return err
	})
	if err != nil {
		return domain.Event{}, err
	}
	return out, nil
}

// Create stores a new event, and its series when Input carries a recurrence.
func (s *Service) Create(ctx context.Context, actor int64, in Input) (domain.Event, error) {
	if err := s.authorize(ctx, actor, in.CalendarID); err != nil {
		return domain.Event{}, err
	}
	if err := s.validate(ctx, in); err != nil {
		return domain.Event{}, err
	}

	return s.inTxEvent(ctx, func(s *Service) (domain.Event, error) {
		now := s.clk.Now()
		e := s.apply(domain.Event{CalendarID: in.CalendarID, CreatedBy: actor, CreatedAt: now}, in, actor, now)

		var rec *domain.Recurrence
		if in.Recurrence != nil {
			r := *in.Recurrence
			r.DTStart = s.startDateOf(e)
			if err := recur.Validate(r); err != nil {
				return domain.Event{}, err
			}
			rec = &r
		}

		created, err := s.st.CreateEvent(ctx, e, rec)
		if err != nil {
			return domain.Event{}, fmt.Errorf("create event: %w", err)
		}
		s.logActivity(ctx, actor, created, domain.ActionEventCreated)
		return created, nil
	})
}

// resolveOverride rewrites a request addressed to an edited occurrence so that it names
// the occurrence rather than the copy standing in for it.
//
// Editing one occurrence leaves behind a standalone copy of the event, and it is that
// copy the API publishes and the client sends back — so the *second* thing anyone does
// to an occurrence arrives naming the copy. A copy has no recurrence of its own, so
// without this every such request took the plain-event path: scope and date were read
// and then ignored, deleting the occurrence removed the override row instead of the
// occurrence (bringing it back at its original time), and the pruning that stops a
// moved occurrence firing its old reminder cleared the wrong rows.
//
// The date comes from the override row rather than from the request, because that row
// is the only authority on which occurrence the copy replaced; the copy's own start
// date is wherever the family moved it to.
//
// The requested scope is kept. Once a client can see the series again it will ask
// "this / this and following / the whole series", and all three have to reach it. Only
// a missing or unusable scope becomes ScopeThis — a copy exists for exactly one
// occurrence, so an unscoped request about one can only mean that occurrence, and
// refusing it would break the clients that have been addressing copies all along.
//
// Events that are not override copies, including a copy a series split deliberately
// detached, come back untouched.
func (s *Service) resolveOverride(ctx context.Context, eventID int64, scope domain.EditScope, occDate domain.Date) (int64, domain.EditScope, domain.Date, error) {
	ref, err := s.st.OverrideRefByEventID(ctx, eventID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return eventID, scope, occDate, nil
		}
		return 0, "", domain.Date{}, err
	}
	if !scope.Valid() {
		scope = domain.ScopeThis
	}
	return ref.SeriesEventID, scope, ref.OccurrenceDate, nil
}

// Update applies an edit at the requested scope. For a recurring event, scope and
// occDate decide whether this touches one occurrence, the rest of the series, or all
// of it; the three do genuinely different things to the data and cannot be collapsed.
//
// eventID may be the copy an earlier single-occurrence edit produced, in which case it
// addresses that occurrence — see resolveOverride.
func (s *Service) Update(ctx context.Context, actor, eventID int64, scope domain.EditScope, occDate domain.Date, in Input) (domain.Event, error) {
	eventID, scope, occDate, err := s.resolveOverride(ctx, eventID, scope, occDate)
	if err != nil {
		return domain.Event{}, err
	}
	existing, err := s.st.EventByID(ctx, eventID)
	if err != nil {
		return domain.Event{}, err
	}
	if err := s.authorize(ctx, actor, existing.CalendarID); err != nil {
		return domain.Event{}, err
	}
	in.CalendarID = existing.CalendarID // events never move between calendars
	if err := s.validate(ctx, in); err != nil {
		return domain.Event{}, err
	}

	if existing.RecurrenceID == nil {
		if in.Recurrence != nil {
			return domain.Event{}, errNoRepeatTransition("event %d does not repeat", eventID)
		}
		return s.updatePlain(ctx, actor, existing, in)
	}
	if !scope.Valid() {
		return domain.Event{}, fmt.Errorf("%w: event %d is recurring, so an edit scope is required", domain.ErrInvalid, eventID)
	}
	rec, err := s.st.RecurrenceByID(ctx, *existing.RecurrenceID)
	if err != nil {
		return domain.Event{}, err
	}

	switch scope {
	case domain.ScopeAll:
		// A whole-series edit is the only request that can carry a new pattern, so it is
		// also the only one where a missing pattern can mean "stop repeating". It used to
		// mean "leave the pattern alone", which is how unticking "repeat" answered 200 and
		// changed nothing. The other two scopes are about occurrences and legitimately
		// arrive without a pattern — the server owns the split — so they are untouched.
		if in.Recurrence == nil {
			return domain.Event{}, errNoRepeatTransition("event %d is a series", eventID)
		}
		return s.updateSeries(ctx, actor, existing, rec, false, in)
	case domain.ScopeThis:
		if occDate.IsZero() {
			return domain.Event{}, fmt.Errorf("%w: editing one occurrence needs its date", domain.ErrInvalid)
		}
		return s.updateOccurrence(ctx, actor, existing, rec, occDate, in)
	case domain.ScopeUpcoming:
		if occDate.IsZero() {
			return domain.Event{}, fmt.Errorf("%w: editing this and following occurrences needs the split date", domain.ErrInvalid)
		}
		// Splitting at or before the series start would leave an empty first half,
		// so it is an edit of the whole series — but it is still one occurrence being
		// moved, and the pattern has to follow it exactly as a split's does, which is
		// what the flag asks for.
		if !occDate.After(rec.DTStart) {
			return s.updateSeries(ctx, actor, existing, rec, true, in)
		}
		return s.splitSeries(ctx, actor, existing, rec, occDate, in)
	}
	return domain.Event{}, fmt.Errorf("%w: unknown edit scope %q", domain.ErrInvalid, scope)
}

// errNoRepeatTransition refuses an edit that would give an existing event a repeat or
// take one away. Both used to be accepted and then quietly dropped on the floor, which
// is the worse of the two failures: the family saw a saved event that had not changed.
//
// Neither transition is a matter of writing one more row. Adding a repeat has to move
// the event's reminders onto the new series, or every occurrence is notified twice —
// once from the reminders still scoped to the event, once from the series' own. Removing
// one destroys the override rows for occurrences somebody edited by hand, and what
// should become of those copies is a question nobody has answered yet. Until both are
// done deliberately, the edit is refused before anything is written; a repeat is decided
// when the event is created, and changing one's mind means deleting and recreating it.
func errNoRepeatTransition(reason string, args ...any) error {
	return fmt.Errorf("%w: a repeat cannot be added or removed here (%s)", domain.ErrInvalid, fmt.Sprintf(reason, args...))
}

func (s *Service) updatePlain(ctx context.Context, actor int64, existing domain.Event, in Input) (domain.Event, error) {
	return s.inTxEvent(ctx, func(s *Service) (domain.Event, error) {
		now := s.clk.Now()
		updated := s.apply(existing, in, actor, now)
		if err := s.st.UpdateEvent(ctx, updated); err != nil {
			return domain.Event{}, fmt.Errorf("update event %d: %w", existing.ID, err)
		}
		if err := s.st.SetParticipants(ctx, existing.ID, in.Participants); err != nil {
			return domain.Event{}, err
		}
		s.pruneQueued(ctx, EventSourcePrefix(existing.ID))
		s.logActivity(ctx, actor, updated, domain.ActionEventUpdated)
		return updated, nil
	})
}

// updateSeries edits the template. Existing overrides are left alone: someone
// deliberately changed those occurrences, and silently reverting their work would be
// worse than the inconsistency of leaving them.
//
// followMove says the edit is one occurrence being dragged rather than a plain
// whole-series edit, which is how "this and following" at the very first occurrence
// arrives here (Update refuses to split there — the first half would be empty). The
// pattern then has to move with the occurrence, and the result has to contain its own
// start, both for the reasons splitSeries gives. Without it the shortcut skipped every
// one of those steps: the client sends no pattern for that scope, so the series carried
// on at its old weekday while DTStart alone moved, and since a series is only ever read
// through its rule, neither the occurrence that was dragged nor the one it came from
// existed afterwards. The edit answered 200 with both gone.
//
// The two are asked only of that case. A whole-series edit carries its own pattern and
// may legitimately anchor before its first occurrence — a weekly series anchored on a
// Monday with by_weekday of Tuesday starts the day after, which internal/recur documents
// and the editor produces — so applying the same checks there would refuse it.
func (s *Service) updateSeries(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence, followMove bool, in Input) (domain.Event, error) {
	// Decided before the transaction opens, for the reason splitSeries sets out: a
	// rejected edit must not have written anything first.
	now := s.clk.Now()
	updated := s.apply(template, in, actor, now)

	newRec := rec
	if in.Recurrence != nil {
		newRec = *in.Recurrence
		newRec.ID = rec.ID
	}
	newRec.DTStart = s.startDateOf(updated)
	moved := followMove && in.Recurrence == nil
	if moved {
		newRec = reanchor(newRec, rec.DTStart, newRec.DTStart)
		// Dragging the first occurrence past the end of its own series leaves exactly
		// one occurrence rather than an impossible range.
		if newRec.Until != nil && newRec.Until.Before(newRec.DTStart) {
			last := newRec.DTStart
			newRec.Until = &last
		}
	}
	if err := recur.Validate(newRec); err != nil {
		return domain.Event{}, err
	}
	if moved && !recur.Occurs(newRec, newRec.DTStart) {
		return domain.Event{}, fmt.Errorf("%w: the new repeat pattern does not include %s, the date being moved",
			domain.ErrInvalid, newRec.DTStart)
	}

	return s.inTxEvent(ctx, func(s *Service) (domain.Event, error) {
		if err := s.st.UpdateRecurrence(ctx, newRec); err != nil {
			return domain.Event{}, fmt.Errorf("update recurrence %d: %w", rec.ID, err)
		}
		if err := s.st.UpdateEvent(ctx, updated); err != nil {
			return domain.Event{}, fmt.Errorf("update series template %d: %w", template.ID, err)
		}
		if err := s.st.SetParticipants(ctx, template.ID, in.Participants); err != nil {
			return domain.Event{}, err
		}
		s.pruneQueued(ctx, EventSourcePrefix(template.ID))
		s.logActivity(ctx, actor, updated, domain.ActionEventUpdated)
		return updated, nil
	})
}

// updateOccurrence records an exception for one date: a standalone event holding the
// edited values, pointed at by an override row. The series is untouched, which is
// what makes editing one occurrence safe to do repeatedly.
func (s *Service) updateOccurrence(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence, occDate domain.Date, in Input) (domain.Event, error) {
	if !recur.Occurs(rec, occDate) {
		return domain.Event{}, fmt.Errorf("%w: %s is not an occurrence of event %d", domain.ErrNotFound, occDate, template.ID)
	}
	return s.inTxEvent(ctx, func(s *Service) (domain.Event, error) {
		overrides, err := s.st.Overrides(ctx, rec.ID)
		if err != nil {
			return domain.Event{}, err
		}
		now := s.clk.Now()

		// Re-editing an occurrence updates the copy that already exists rather than
		// orphaning it.
		if ov, ok := overrides[occDate]; ok && ov != nil {
			current, err := s.st.EventByID(ctx, *ov)
			if err != nil {
				return domain.Event{}, err
			}
			updated := s.apply(current, in, actor, now)
			if err := s.st.UpdateEvent(ctx, updated); err != nil {
				return domain.Event{}, fmt.Errorf("update override %d: %w", current.ID, err)
			}
			if err := s.st.SetParticipants(ctx, current.ID, in.Participants); err != nil {
				return domain.Event{}, err
			}
			s.pruneQueued(ctx, OccurrenceSourcePrefix(template.ID, occDate))
			s.logActivity(ctx, actor, updated, domain.ActionEventUpdated)
			return updated, nil
		}

		copyEvent := s.apply(domain.Event{CalendarID: template.CalendarID, CreatedBy: actor, CreatedAt: now}, in, actor, now)
		copyEvent.RecurrenceID = nil // an override stands alone; it is not a second series
		created, err := s.st.CreateEvent(ctx, copyEvent, nil)
		if err != nil {
			return domain.Event{}, fmt.Errorf("create override event: %w", err)
		}
		if err := s.st.SetOverride(ctx, rec.ID, occDate, &created.ID); err != nil {
			return domain.Event{}, fmt.Errorf("record override for %s: %w", occDate, err)
		}
		// No reminders are written here, deliberately. The copy inherits the series'
		// until somebody changes them on this occurrence (docs/architecture.md), so
		// moving a lesson changes when everybody is reminded about it and nothing else.
		// Copying them onto the copy instead was tried, and it silently dropped every
		// reminder the series gained afterwards — the copy was written once and never
		// heard from the series again.
		s.pruneQueued(ctx, OccurrenceSourcePrefix(template.ID, occDate))
		s.logActivity(ctx, actor, created, domain.ActionEventUpdated)
		return created, nil
	})
}

// splitSeries implements "this and following occurrences": the old series is ended
// the day before the split, and a new series carrying the edits starts at it.
//
// The bookkeeping around the split is the part that bites. Overrides dated at or
// after the split must move to the new series, or a cancelled future occurrence
// would come back from the dead under the new one; and the old series' reminders
// must be copied, or everyone silently stops being reminded about the half of the
// series they still have.
func (s *Service) splitSeries(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence, splitDate domain.Date, in Input) (domain.Event, error) {
	now := s.clk.Now()

	// 1. Work out both halves and check them BEFORE writing anything, and outside the
	// transaction: SQLite takes its write lock at BEGIN, so deciding first keeps the
	// lock held for the writes alone.
	//
	// This ordering is the whole point. Closing the original series first and
	// validating the replacement afterwards meant a rejected edit — moving an
	// occurrence past the series' own end date is enough — returned an error to the
	// user while having already deleted every remaining occurrence. "Nothing
	// happened" on screen, half the series gone in the database.
	newEvent := s.apply(domain.Event{CalendarID: template.CalendarID, CreatedBy: actor, CreatedAt: now}, in, actor, now)
	newRec := rec
	if in.Recurrence != nil {
		newRec = *in.Recurrence
	}
	newRec.ID = 0
	newRec.DTStart = s.startDateOf(newEvent)
	newRec.Until = rec.Until

	if in.Recurrence == nil {
		// The client does not resend the pattern for this scope — the server owns
		// the split — so moving an occurrence to another weekday or day of the month
		// has to move the pattern with it. Without this the new series carried on at
		// the old day and the occurrence the user had just moved did not exist at all.
		newRec = reanchor(newRec, splitDate, newRec.DTStart)
	}
	// Moving an occurrence past the end of its own series leaves exactly one
	// occurrence rather than an impossible range.
	if newRec.Until != nil && newRec.Until.Before(newRec.DTStart) {
		last := newRec.DTStart
		newRec.Until = &last
	}
	if err := recur.Validate(newRec); err != nil {
		return domain.Event{}, fmt.Errorf("starting the new series: %w", err)
	}
	// Whatever the re-anchoring produced, a series must contain its own start;
	// otherwise the edit silently swallows the occurrence it was meant to move.
	if !recur.Occurs(newRec, newRec.DTStart) {
		return domain.Event{}, fmt.Errorf("%w: the new repeat pattern does not include %s, the date being moved",
			domain.ErrInvalid, newRec.DTStart)
	}

	oldRec := rec
	until := endOfSeries(rec, splitDate.AddDays(-1))
	oldRec.Until = &until
	if err := recur.Validate(oldRec); err != nil {
		return domain.Event{}, fmt.Errorf("closing the original series: %w", err)
	}

	// 2. Write, all of it in one transaction. Every statement below runs on the
	// request's context, so a phone that walks into a lift between two of them stops
	// the sequence for real: this used to leave the original series capped and its
	// replacement never created, which is half a family's swimming lessons gone.
	return s.inTxEvent(ctx, func(s *Service) (domain.Event, error) {
		if err := s.st.UpdateRecurrence(ctx, oldRec); err != nil {
			return domain.Event{}, fmt.Errorf("close series %d at %s: %w", rec.ID, until, err)
		}
		created, err := s.st.CreateEvent(ctx, newEvent, &newRec)
		if err != nil {
			return domain.Event{}, fmt.Errorf("create split series: %w", err)
		}
		if created.RecurrenceID == nil {
			return domain.Event{}, fmt.Errorf("split series %d was created without a recurrence", created.ID)
		}
		newRecID := *created.RecurrenceID

		// 3. Move exceptions at or after the split onto the new series.
		if err := s.st.RepointOverrides(ctx, rec.ID, newRecID, splitDate); err != nil {
			return domain.Event{}, fmt.Errorf("move overrides to the split series: %w", err)
		}

		// 4. Carry everyone's reminders across, or the second half goes quiet. This
		// comes before the detaching below, which hands an occurrence leaving the
		// series the reminders it was inheriting — from the new series, since that is
		// the one it is leaving.
		if err := s.copyReminders(ctx, rec.ID, newRecID); err != nil {
			return domain.Event{}, err
		}

		// 5. An override only survives if the new pattern still produces its date. When
		// the pattern moved — Tuesdays to Wednesdays — the ones it no longer produces
		// would otherwise become rows nothing can reach: invisible to the series (which
		// does not generate that date) and hidden from the plain-event query (which
		// excludes anything an override points at). Detaching them turns each back into
		// an ordinary event the family can still see and deal with.
		if err := s.detachUnreachableOverrides(ctx, newRecID, newRec); err != nil {
			return domain.Event{}, err
		}

		// 6. Both halves changed shape; let the planner rebuild their notifications.
		s.pruneQueued(ctx, EventSourcePrefix(template.ID))
		s.pruneQueued(ctx, EventSourcePrefix(created.ID))

		s.logActivity(ctx, actor, created, domain.ActionEventUpdated)
		return created, nil
	})
}

// reanchor moves a repeat pattern's day-selectors so that the pattern still
// describes the occurrence the user dragged. Frequencies anchored purely on the
// start date (daily, yearly, and weekly with no explicit weekdays) need nothing.
func reanchor(r domain.Recurrence, from, to domain.Date) domain.Recurrence {
	if from.Equal(to) {
		return r
	}
	switch r.Freq {
	case domain.FreqWeekly:
		if len(r.ByWeekday) == 0 {
			return r
		}
		delta := (int(to.Weekday()) - int(from.Weekday()) + 7) % 7
		if delta == 0 {
			return r
		}
		shifted := make([]time.Weekday, 0, len(r.ByWeekday))
		for _, wd := range r.ByWeekday {
			shifted = append(shifted, time.Weekday((int(wd)+delta)%7))
		}
		r.ByWeekday = shifted
	case domain.FreqMonthly:
		switch {
		case r.ByMonthday != nil:
			day := to.Day
			r.ByMonthday = &day
		case r.WeekOrdinal != nil:
			ordinal := (to.Day-1)/7 + 1
			r.WeekOrdinal = &ordinal
			r.ByWeekday = []time.Weekday{to.Weekday()}
		case r.MonthLastDay:
			// Still the last day? Then the rule still says what the user means.
			if !to.Equal(domain.LastDayOfMonth(to.Year, to.Month)) {
				day := to.Day
				r.MonthLastDay = false
				r.ByMonthday = &day
			}
		}
	}
	return r
}

// detachUnreachableOverrides removes override rows whose date the pattern no longer
// produces, leaving any edited copy as a standalone event rather than an orphan.
//
// A copy leaving its series takes the reminders it was inheriting with it. Up to this
// point it had none of its own — an edited occurrence inherits the series' until
// somebody changes them on it — and an ordinary event with no reminders of its own is
// announced by nothing at all, so without this the family would simply stop being told
// about the one occurrence a re-patterned split stranded. Members who had already set
// their own reminders on that occurrence keep exactly those.
func (s *Service) detachUnreachableOverrides(ctx context.Context, recurrenceID int64, rec domain.Recurrence) error {
	overrides, err := s.st.Overrides(ctx, recurrenceID)
	if err != nil {
		return err
	}
	for date, ov := range overrides {
		if recur.Occurs(rec, date) {
			continue
		}
		if ov != nil {
			if err := s.keepInheritedReminders(ctx, recurrenceID, *ov); err != nil {
				return err
			}
		}
		if err := s.st.DeleteOverride(ctx, recurrenceID, date); err != nil {
			return fmt.Errorf("detach override %s: %w", date, err)
		}
		if ov != nil {
			slog.Info("an edited occurrence no longer fits its series and is now a separate event",
				"event", *ov, "date", date)
		}
	}
	return nil
}

// keepInheritedReminders writes onto an override copy the series reminders it has been
// inheriting, for every member who has not set their own on it — which is what has to
// happen just before the copy stops being an occurrence of that series and inheritance
// stops applying to it.
//
// Everyone's are written and not only the editor's, because a reminder is personal:
// re-patterning the series must not take Maman's reminder off the lesson she still
// expects to be told about.
func (s *Service) keepInheritedReminders(ctx context.Context, fromRec, toEvent int64) error {
	all, err := s.st.ListAllReminders(ctx)
	if err != nil {
		return fmt.Errorf("load the reminders a detached occurrence inherits: %w", err)
	}
	byUser := map[int64][]domain.Reminder{}
	for _, r := range all {
		if r.RecurrenceID != nil && *r.RecurrenceID == fromRec {
			byUser[r.UserID] = append(byUser[r.UserID], r)
		}
	}
	for userID, rs := range byUser {
		// "Has set their own on it" includes having cleared them there, which is why
		// this asks the store rather than counting rows: an empty list somebody chose
		// must not be refilled from the series on the way out of it.
		own, err := s.st.RemindersDetached(ctx, toEvent, userID)
		if err != nil {
			return err
		}
		if own {
			continue
		}
		if err := s.st.ReplaceReminders(ctx, &toEvent, nil, userID, rs); err != nil {
			return fmt.Errorf("keep the reminders of the detached occurrence for user %d: %w", userID, err)
		}
	}
	return nil
}

// copyReminders duplicates every user's reminders from one series to another.
func (s *Service) copyReminders(ctx context.Context, fromRec, toRec int64) error {
	all, err := s.st.ListAllReminders(ctx)
	if err != nil {
		return fmt.Errorf("load reminders for the split: %w", err)
	}
	byUser := map[int64][]domain.Reminder{}
	for _, r := range all {
		if r.RecurrenceID != nil && *r.RecurrenceID == fromRec {
			r.ID = 0
			r.RecurrenceID = &toRec
			byUser[r.UserID] = append(byUser[r.UserID], r)
		}
	}
	for userID, rs := range byUser {
		if err := s.st.ReplaceReminders(ctx, nil, &toRec, userID, rs); err != nil {
			return fmt.Errorf("copy reminders for user %d: %w", userID, err)
		}
	}
	return nil
}

// Delete removes an event at the requested scope. As with Update, eventID may be the
// copy an earlier single-occurrence edit produced — deleting that copy on its own is
// what used to bring the occurrence back, since the cascade took the exception with it.
func (s *Service) Delete(ctx context.Context, actor, eventID int64, scope domain.EditScope, occDate domain.Date) error {
	eventID, scope, occDate, err := s.resolveOverride(ctx, eventID, scope, occDate)
	if err != nil {
		return err
	}
	existing, err := s.st.EventByID(ctx, eventID)
	if err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, existing.CalendarID); err != nil {
		return err
	}

	if existing.RecurrenceID == nil {
		return s.inTx(ctx, func(s *Service) error {
			if err := s.st.DeleteEvent(ctx, eventID); err != nil {
				return fmt.Errorf("delete event %d: %w", eventID, err)
			}
			s.pruneQueued(ctx, EventSourcePrefix(eventID))
			s.logActivity(ctx, actor, existing, domain.ActionEventDeleted)
			return nil
		})
	}

	rec, err := s.st.RecurrenceByID(ctx, *existing.RecurrenceID)
	if err != nil {
		return err
	}
	switch scope {
	case domain.ScopeThis:
		if occDate.IsZero() {
			return fmt.Errorf("%w: cancelling one occurrence needs its date", domain.ErrInvalid)
		}
		return s.cancelOccurrence(ctx, actor, existing, rec, occDate)
	case domain.ScopeUpcoming:
		if occDate.IsZero() {
			return fmt.Errorf("%w: deleting this and following occurrences needs the split date", domain.ErrInvalid)
		}
		if !occDate.After(rec.DTStart) {
			return s.deleteSeries(ctx, actor, existing, rec)
		}
		return s.endSeries(ctx, actor, existing, rec, occDate.AddDays(-1))
	case domain.ScopeAll:
		return s.deleteSeries(ctx, actor, existing, rec)
	}
	return fmt.Errorf("%w: deleting a recurring event needs a scope", domain.ErrInvalid)
}

// endOfSeries is where a series ends once it has been asked to stop at `at`: the
// earlier of that date and the end it already had.
//
// Ending a series can only ever bring its end forward. Written as `until = at` it also
// pushed it back, so "delete this and following" — or a split — at a date past a series'
// own end *extended* it, and every occurrence between the real end and the new one came
// back from the dead. A client can address such a date perfectly innocently: the last
// occurrence a family remembers is not the last one the rule produced.
func endOfSeries(rec domain.Recurrence, at domain.Date) domain.Date {
	if rec.Until != nil && rec.Until.Before(at) {
		return *rec.Until
	}
	return at
}

// endSeries stops a series repeating after a date, which is what "delete this and
// following occurrences" means for everything that is not the first one.
func (s *Service) endSeries(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence, until domain.Date) error {
	until = endOfSeries(rec, until)
	return s.inTx(ctx, func(s *Service) error {
		rec.Until = &until
		if err := s.st.UpdateRecurrence(ctx, rec); err != nil {
			return fmt.Errorf("end series %d at %s: %w", rec.ID, until, err)
		}
		s.pruneQueued(ctx, EventSourcePrefix(template.ID))
		s.logActivity(ctx, actor, template, domain.ActionEventUpdated)
		return nil
	})
}

func (s *Service) cancelOccurrence(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence, occDate domain.Date) error {
	if !recur.Occurs(rec, occDate) {
		return fmt.Errorf("%w: %s is not an occurrence of event %d", domain.ErrNotFound, occDate, template.ID)
	}
	return s.inTx(ctx, func(s *Service) error {
		overrides, err := s.st.Overrides(ctx, rec.ID)
		if err != nil {
			return err
		}
		if ov, ok := overrides[occDate]; ok && ov != nil {
			// Replace the edited copy with a cancellation, and remove the copy itself.
			if err := s.st.SetOverride(ctx, rec.ID, occDate, nil); err != nil {
				return err
			}
			if err := s.st.DeleteEvent(ctx, *ov); err != nil {
				return fmt.Errorf("delete override event %d: %w", *ov, err)
			}
		} else if err := s.st.SetOverride(ctx, rec.ID, occDate, nil); err != nil {
			return fmt.Errorf("cancel occurrence %s: %w", occDate, err)
		}
		s.pruneQueued(ctx, OccurrenceSourcePrefix(template.ID, occDate))
		s.logActivity(ctx, actor, template, domain.ActionEventDeleted)
		return nil
	})
}

// deleteSeries removes a series and everything hanging off it. Deleting the
// recurrence cascades its override rows and reminders, but the events those rows
// pointed at are ordinary rows that nothing else would ever clean up, so they go first.
func (s *Service) deleteSeries(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence) error {
	return s.inTx(ctx, func(s *Service) error {
		overrides, err := s.st.Overrides(ctx, rec.ID)
		if err != nil {
			return err
		}
		for _, ov := range overrides {
			if ov == nil {
				continue
			}
			if err := s.st.DeleteEvent(ctx, *ov); err != nil {
				return fmt.Errorf("delete override event %d: %w", *ov, err)
			}
		}
		if err := s.st.DeleteRecurrence(ctx, rec.ID); err != nil {
			return fmt.Errorf("delete recurrence %d: %w", rec.ID, err)
		}
		if err := s.st.DeleteEvent(ctx, template.ID); err != nil {
			return fmt.Errorf("delete series template %d: %w", template.ID, err)
		}
		s.pruneQueued(ctx, EventSourcePrefix(template.ID))
		s.logActivity(ctx, actor, template, domain.ActionEventDeleted)
		return nil
	})
}

// apply copies Input onto an event, preserving identity and creation metadata.
func (s *Service) apply(e domain.Event, in Input, actor int64, now time.Time) domain.Event {
	e.Title = strings.TrimSpace(in.Title)
	e.AllDay = in.AllDay
	e.Location = strings.TrimSpace(in.Location)
	e.URL = strings.TrimSpace(in.URL)
	e.Notes = in.Notes
	e.LabelID = in.LabelID
	e.Participants = in.Participants
	e.UpdatedBy = actor
	e.UpdatedAt = now
	if in.AllDay {
		e.StartDate, e.EndDate = in.StartDate, in.EndDate
		e.StartsAt, e.EndsAt = time.Time{}, time.Time{}
	} else {
		e.StartsAt, e.EndsAt = in.StartsAt.UTC(), in.EndsAt.UTC()
		e.StartDate, e.EndDate = domain.Date{}, domain.Date{}
	}
	return e
}

func (s *Service) validate(ctx context.Context, in Input) error {
	if strings.TrimSpace(in.Title) == "" {
		return fmt.Errorf("%w: an event needs a title", domain.ErrInvalid)
	}
	if in.AllDay {
		if in.StartDate.IsZero() || in.EndDate.IsZero() {
			return fmt.Errorf("%w: an all-day event needs a start and end date", domain.ErrInvalid)
		}
		if in.EndDate.Before(in.StartDate) {
			return fmt.Errorf("%w: the end date (%s) is before the start (%s)", domain.ErrInvalid, in.EndDate, in.StartDate)
		}
	} else {
		if in.StartsAt.IsZero() || in.EndsAt.IsZero() {
			return fmt.Errorf("%w: a timed event needs a start and an end", domain.ErrInvalid)
		}
		if !in.EndsAt.After(in.StartsAt) {
			return fmt.Errorf("%w: the end must come after the start", domain.ErrInvalid)
		}
	}

	label, err := s.st.LabelByID(ctx, in.LabelID)
	if err != nil {
		return fmt.Errorf("label %d: %w", in.LabelID, err)
	}
	if label.CalendarID != in.CalendarID {
		return fmt.Errorf("%w: label %d belongs to another calendar", domain.ErrInvalid, in.LabelID)
	}

	for _, uid := range in.Participants {
		member, err := s.st.IsMember(ctx, in.CalendarID, uid)
		if err != nil {
			return err
		}
		if !member {
			return fmt.Errorf("%w: user %d is not a member of this calendar", domain.ErrInvalid, uid)
		}
	}
	if in.Recurrence != nil {
		r := *in.Recurrence
		if in.AllDay {
			r.DTStart = in.StartDate
		} else {
			r.DTStart = domain.DateIn(in.StartsAt.UTC(), s.loc)
		}
		if err := recur.Validate(r); err != nil {
			return err
		}
	}
	return nil
}

// authorize enforces the one permission rule this app has: you may touch a calendar
// you belong to. Everything else is deliberately flat.
func (s *Service) authorize(ctx context.Context, userID, calendarID int64) error {
	member, err := s.st.IsMember(ctx, calendarID, userID)
	if err != nil {
		return err
	}
	if !member {
		return fmt.Errorf("%w: user %d is not a member of calendar %d", domain.ErrForbidden, userID, calendarID)
	}
	return nil
}

// logActivity records a change in the feed the family reads. Every call is made from
// inside the transaction that made the change, so the row commits with the edit or not
// at all: internal/notify plans change notifications from this log rather than from the
// edit itself, and that is only the crash-proof design it claims to be if the two
// cannot come apart.
func (s *Service) logActivity(ctx context.Context, actor int64, e domain.Event, action domain.ActivityAction) {
	err := s.st.LogActivity(ctx, domain.Activity{
		CalendarID: e.CalendarID,
		UserID:     actor,
		Action:     action,
		EventID:    &e.ID,
		Title:      e.Title,
		At:         s.clk.Now(),
	})
	if err != nil {
		// The feed is a nice-to-have; a row that will not insert must never fail the
		// edit the family actually asked for. Inside the transaction this is close to
		// theoretical — anything that can stop this statement, a cancelled request or
		// a full disk, stops the commit as well — which is why it is logged loudly
		// rather than counted as ordinary.
		slog.Error("record activity", "event", e.ID, "action", action, "error", err)
	}
}

// pruneQueued drops notifications the planner queued for work that has since changed.
// Without this, moving an event still fires a reminder at its old time.
func (s *Service) pruneQueued(ctx context.Context, prefix string) {
	n, err := s.st.DeleteUnsentBySourcePrefix(ctx, prefix)
	if err != nil {
		slog.Error("prune queued notifications", "prefix", prefix, "error", err)
		return
	}
	if n > 0 {
		slog.Debug("pruned queued notifications", "prefix", prefix, "count", n)
	}
}
