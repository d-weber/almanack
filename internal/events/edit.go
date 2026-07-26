package events

import (
	"time"

	"context"
	"fmt"
	"log/slog"
	"strings"

	"agenda/internal/domain"
	"agenda/internal/recur"
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

// Create stores a new event, and its series when Input carries a recurrence.
func (s *Service) Create(ctx context.Context, actor int64, in Input) (domain.Event, error) {
	if err := s.authorize(ctx, actor, in.CalendarID); err != nil {
		return domain.Event{}, err
	}
	if err := s.validate(ctx, in); err != nil {
		return domain.Event{}, err
	}

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
}

// Update applies an edit at the requested scope. For a recurring event, scope and
// occDate decide whether this touches one occurrence, the rest of the series, or all
// of it; the three do genuinely different things to the data and cannot be collapsed.
func (s *Service) Update(ctx context.Context, actor, eventID int64, scope domain.EditScope, occDate domain.Date, in Input) (domain.Event, error) {
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
		return s.updateSeries(ctx, actor, existing, rec, in)
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
		// so it is simply an edit of the whole series.
		if !occDate.After(rec.DTStart) {
			return s.updateSeries(ctx, actor, existing, rec, in)
		}
		return s.splitSeries(ctx, actor, existing, rec, occDate, in)
	}
	return domain.Event{}, fmt.Errorf("%w: unknown edit scope %q", domain.ErrInvalid, scope)
}

func (s *Service) updatePlain(ctx context.Context, actor int64, existing domain.Event, in Input) (domain.Event, error) {
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
}

// updateSeries edits the template. Existing overrides are left alone: someone
// deliberately changed those occurrences, and silently reverting their work would be
// worse than the inconsistency of leaving them.
func (s *Service) updateSeries(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence, in Input) (domain.Event, error) {
	now := s.clk.Now()
	updated := s.apply(template, in, actor, now)

	newRec := rec
	if in.Recurrence != nil {
		newRec = *in.Recurrence
		newRec.ID = rec.ID
	}
	newRec.DTStart = s.startDateOf(updated)
	if err := recur.Validate(newRec); err != nil {
		return domain.Event{}, err
	}

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
}

// updateOccurrence records an exception for one date: a standalone event holding the
// edited values, pointed at by an override row. The series is untouched, which is
// what makes editing one occurrence safe to do repeatedly.
func (s *Service) updateOccurrence(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence, occDate domain.Date, in Input) (domain.Event, error) {
	if !recur.Occurs(rec, occDate) {
		return domain.Event{}, fmt.Errorf("%w: %s is not an occurrence of event %d", domain.ErrNotFound, occDate, template.ID)
	}
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
	s.pruneQueued(ctx, OccurrenceSourcePrefix(template.ID, occDate))
	s.logActivity(ctx, actor, created, domain.ActionEventUpdated)
	return created, nil
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

	// 1. Work out both halves and check them BEFORE writing anything.
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
	until := splitDate.AddDays(-1)
	oldRec.Until = &until
	if err := recur.Validate(oldRec); err != nil {
		return domain.Event{}, fmt.Errorf("closing the original series: %w", err)
	}

	// 2. Write. These are still separate statements — see the note on atomicity in
	// docs/architecture.md — but nothing here can now fail for a reason that was
	// knowable beforehand.
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
	// An override only survives if the new pattern still produces its date. When the
	// pattern moved — Tuesdays to Wednesdays — the ones it no longer produces would
	// otherwise become rows nothing can reach: invisible to the series (which does
	// not generate that date) and hidden from the plain-event query (which excludes
	// anything an override points at). Detaching them turns each back into an
	// ordinary event the family can still see and deal with.
	if err := s.detachUnreachableOverrides(ctx, newRecID, newRec); err != nil {
		return domain.Event{}, err
	}

	// 4. Carry everyone's reminders across, or the second half goes quiet.
	if err := s.copyReminders(ctx, rec.ID, newRecID); err != nil {
		return domain.Event{}, err
	}

	// 5. Both halves changed shape; let the planner rebuild their notifications.
	s.pruneQueued(ctx, EventSourcePrefix(template.ID))
	s.pruneQueued(ctx, EventSourcePrefix(created.ID))

	s.logActivity(ctx, actor, created, domain.ActionEventUpdated)
	return created, nil
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
func (s *Service) detachUnreachableOverrides(ctx context.Context, recurrenceID int64, rec domain.Recurrence) error {
	overrides, err := s.st.Overrides(ctx, recurrenceID)
	if err != nil {
		return err
	}
	for date, ov := range overrides {
		if recur.Occurs(rec, date) {
			continue
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

// Delete removes an event at the requested scope.
func (s *Service) Delete(ctx context.Context, actor, eventID int64, scope domain.EditScope, occDate domain.Date) error {
	existing, err := s.st.EventByID(ctx, eventID)
	if err != nil {
		return err
	}
	if err := s.authorize(ctx, actor, existing.CalendarID); err != nil {
		return err
	}

	if existing.RecurrenceID == nil {
		if err := s.st.DeleteEvent(ctx, eventID); err != nil {
			return fmt.Errorf("delete event %d: %w", eventID, err)
		}
		s.pruneQueued(ctx, EventSourcePrefix(eventID))
		s.logActivity(ctx, actor, existing, domain.ActionEventDeleted)
		return nil
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
		until := occDate.AddDays(-1)
		rec.Until = &until
		if err := s.st.UpdateRecurrence(ctx, rec); err != nil {
			return fmt.Errorf("end series %d at %s: %w", rec.ID, until, err)
		}
		s.pruneQueued(ctx, EventSourcePrefix(existing.ID))
		s.logActivity(ctx, actor, existing, domain.ActionEventUpdated)
		return nil
	case domain.ScopeAll:
		return s.deleteSeries(ctx, actor, existing, rec)
	}
	return fmt.Errorf("%w: deleting a recurring event needs a scope", domain.ErrInvalid)
}

func (s *Service) cancelOccurrence(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence, occDate domain.Date) error {
	if !recur.Occurs(rec, occDate) {
		return fmt.Errorf("%w: %s is not an occurrence of event %d", domain.ErrNotFound, occDate, template.ID)
	}
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
}

// deleteSeries removes a series and everything hanging off it. Deleting the
// recurrence cascades its override rows and reminders, but the events those rows
// pointed at are ordinary rows that nothing else would ever clean up, so they go first.
func (s *Service) deleteSeries(ctx context.Context, actor int64, template domain.Event, rec domain.Recurrence) error {
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
		// The activity feed is a nice-to-have; losing an entry must never fail the
		// edit the family actually asked for.
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
