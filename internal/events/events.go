// Package events turns stored rows into the occurrences a calendar actually shows,
// and applies the scoped edits ("this / this and following / the whole series") that
// recurring events need.
//
// Occurrences are computed on read and never stored, so nothing can drift out of
// sync with the series that produced it. The only materialized derivative of an
// event anywhere in this application is the notification outbox, and internal/notify
// reconciles that on every planning pass.
package events

import (
	"context"
	"fmt"
	"sort"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"
	"almanack/internal/recur"
	"almanack/internal/store"
)

// Service reads and writes events. It owns the rules that involve more than one
// table — expansion, overrides, series splitting — which is why they are here and
// not in the HTTP layer or the store.
type Service struct {
	st  *store.Store
	loc *time.Location
	clk clock.Clock
}

func New(st *store.Store, loc *time.Location, clk clock.Clock) *Service {
	return &Service{st: st, loc: loc, clk: clk}
}

// Occurrences returns every occurrence visible in [from, to] for the given
// calendars, ordered by start.
func (s *Service) Occurrences(ctx context.Context, calendarIDs []int64, from, to domain.Date) ([]domain.Occurrence, error) {
	if len(calendarIDs) == 0 {
		return nil, nil
	}
	if to.Before(from) {
		return nil, fmt.Errorf("%w: window ends (%s) before it starts (%s)", domain.ErrInvalid, to, from)
	}

	rr, err := s.st.EventsInRange(ctx, calendarIDs, from, to)
	if err != nil {
		return nil, fmt.Errorf("load events %s..%s: %w", from, to, err)
	}

	var out []domain.Occurrence
	for _, e := range rr.Singles {
		occ := s.occurrenceOf(e, s.startDateOf(e), false, nil)
		if s.overlaps(occ, from, to) {
			out = append(out, occ)
		}
	}
	for _, sr := range rr.Series {
		out = append(out, s.expandSeries(sr, from, to)...)
	}

	sort.SliceStable(out, func(i, j int) bool { return s.sortKey(out[i]).Before(s.sortKey(out[j])) })
	return out, nil
}

// Occurrence returns a single occurrence identified by its event and the date of the
// original occurrence in its series. For a non-recurring event, date may be zero.
//
// The event may equally be the copy an earlier single-occurrence edit produced, which
// is what the client holds after that edit; it resolves back to the occurrence it
// stands for. Reporting such a copy as a plain event is how the series became
// unreachable for a second edit — a client that is told there is no recurrence never
// asks which occurrences the edit should touch.
func (s *Service) Occurrence(ctx context.Context, eventID int64, date domain.Date) (domain.Occurrence, error) {
	eventID, _, date, err := s.resolveOverride(ctx, eventID, domain.ScopeThis, date)
	if err != nil {
		return domain.Occurrence{}, err
	}
	e, err := s.st.EventByID(ctx, eventID)
	if err != nil {
		return domain.Occurrence{}, err
	}
	if e.RecurrenceID == nil {
		// A plain event, or a copy a series split detached: both stand alone.
		return s.occurrenceOf(e, s.startDateOf(e), false, nil), nil
	}
	if date.IsZero() {
		return domain.Occurrence{}, fmt.Errorf("%w: event %d is recurring, so an occurrence date is required", domain.ErrInvalid, eventID)
	}

	rec, err := s.st.RecurrenceByID(ctx, *e.RecurrenceID)
	if err != nil {
		return domain.Occurrence{}, err
	}
	overrides, err := s.st.Overrides(ctx, rec.ID)
	if err != nil {
		return domain.Occurrence{}, err
	}
	if ov, ok := overrides[date]; ok {
		if ov == nil {
			return domain.Occurrence{}, fmt.Errorf("occurrence %s of event %d is cancelled: %w", date, eventID, domain.ErrNotFound)
		}
		oe, err := s.st.EventByID(ctx, *ov)
		if err != nil {
			return domain.Occurrence{}, err
		}
		return s.occurrenceOf(oe, date, true, &e.ID), nil
	}
	if !recur.Occurs(rec, date) {
		return domain.Occurrence{}, fmt.Errorf("%s is not an occurrence of event %d: %w", date, eventID, domain.ErrNotFound)
	}
	return s.shift(e, date), nil
}

// UserOccurrences returns what one person should see in a window: their calendars,
// minus the ones they have muted, filtered to events that concern them where they
// asked for that. This is what the digest and reminder planner read, so the rules
// live here rather than being reimplemented per notification kind.
func (s *Service) UserOccurrences(ctx context.Context, userID int64, from, to domain.Date) ([]domain.Occurrence, error) {
	cals, err := s.st.ListCalendarsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}

	ids := make([]int64, 0, len(cals))
	participatingOnly := map[int64]bool{}
	for _, c := range cals {
		m, err := s.st.Membership(ctx, c.ID, userID)
		if err != nil {
			return nil, err
		}
		if m.Muted {
			continue
		}
		ids = append(ids, c.ID)
		participatingOnly[c.ID] = m.ParticipatingOnly
	}
	if len(ids) == 0 {
		return nil, nil
	}

	all, err := s.Occurrences(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}

	out := all[:0]
	for _, occ := range all {
		if participatingOnly[occ.CalendarID] && !contains(occ.Participants, userID) {
			continue
		}
		out = append(out, occ)
	}
	return out, nil
}

// expandSeries produces the occurrences of one series that fall in the window.
func (s *Service) expandSeries(sr store.SeriesRow, from, to domain.Date) []domain.Occurrence {
	// An occurrence that starts before the window can still reach into it, so
	// expansion starts early by the event's own span.
	span := s.spanDays(sr.Event)
	dates := recur.Expand(sr.Recurrence, from.AddDays(-span-1), to)

	expanded := make(map[domain.Date]bool, len(dates))
	var out []domain.Occurrence

	for _, d := range dates {
		expanded[d] = true
		ov, overridden := sr.Overrides[d]
		if overridden && ov == nil {
			continue // this occurrence was cancelled
		}
		var occ domain.Occurrence
		if overridden {
			oe, ok := sr.OverrideEvents[*ov]
			if !ok {
				continue // dangling override; treated as cancelled rather than crashing
			}
			occ = s.occurrenceOf(oe, d, true, &sr.Event.ID)
		} else {
			occ = s.shift(sr.Event, d)
		}
		if s.overlaps(occ, from, to) {
			out = append(out, occ)
		}
	}

	// An override can move an occurrence *into* this window from a date outside it,
	// so the overrides that expansion did not reach still have to be considered.
	for d, ov := range sr.Overrides {
		if expanded[d] || ov == nil {
			continue
		}
		oe, ok := sr.OverrideEvents[*ov]
		if !ok || !recur.Occurs(sr.Recurrence, d) {
			continue
		}
		occ := s.occurrenceOf(oe, d, true, &sr.Event.ID)
		if s.overlaps(occ, from, to) {
			out = append(out, occ)
		}
	}
	return out
}

// shift moves a series template onto date d, preserving wall-clock time in the family
// timezone. Doing the arithmetic in local time and converting to UTC afterwards is
// what keeps a 16:30 event at 16:30 on both sides of a daylight-saving change.
func (s *Service) shift(template domain.Event, d domain.Date) domain.Occurrence {
	e := template
	if template.AllDay {
		e.StartDate = d
		e.EndDate = d.AddDays(s.spanDays(template))
	} else {
		wall := template.StartsAt.In(s.loc)
		duration := template.EndsAt.Sub(template.StartsAt)
		start := time.Date(d.Year, d.Month, d.Day, wall.Hour(), wall.Minute(), wall.Second(), 0, s.loc)
		e.StartsAt = start.UTC()
		e.EndsAt = start.Add(duration).UTC()
	}
	return s.occurrenceOf(e, d, false, &template.ID)
}

func (s *Service) occurrenceOf(e domain.Event, date domain.Date, isOverride bool, seriesEventID *int64) domain.Occurrence {
	return domain.Occurrence{
		Event:          e,
		OccurrenceDate: date,
		IsOverride:     isOverride,
		SeriesEventID:  seriesEventID,
	}
}

// spanDays is how many days beyond its first the event covers (0 for a same-day event).
func (s *Service) spanDays(e domain.Event) int {
	if e.AllDay {
		if e.EndDate.IsZero() || e.StartDate.IsZero() {
			return 0
		}
		if n := e.StartDate.DaysUntil(e.EndDate); n > 0 {
			return n
		}
		return 0
	}
	start := domain.DateIn(e.StartsAt, s.loc)
	end := s.lastDay(e)
	if n := start.DaysUntil(end); n > 0 {
		return n
	}
	return 0
}

// lastDay is the final calendar day a timed event touches. An event ending exactly at
// midnight belongs to the day before, not to the one that is only just beginning.
func (s *Service) lastDay(e domain.Event) domain.Date {
	end := e.EndsAt
	if end.After(e.StartsAt) {
		end = end.Add(-time.Nanosecond)
	}
	return domain.DateIn(end, s.loc)
}

func (s *Service) startDateOf(e domain.Event) domain.Date {
	if e.AllDay {
		return e.StartDate
	}
	return domain.DateIn(e.StartsAt, s.loc)
}

func (s *Service) overlaps(occ domain.Occurrence, from, to domain.Date) bool {
	var start, end domain.Date
	if occ.AllDay {
		start, end = occ.StartDate, occ.EndDate
		if end.IsZero() {
			end = start
		}
	} else {
		start = domain.DateIn(occ.StartsAt, s.loc)
		end = s.lastDay(occ.Event)
	}
	return !start.After(to) && !end.Before(from)
}

// sortKey orders occurrences by when they start, with all-day events first on their
// day, which is how people read a day's agenda.
func (s *Service) sortKey(occ domain.Occurrence) time.Time {
	if occ.AllDay {
		return occ.StartDate.In(s.loc).UTC()
	}
	return occ.StartsAt
}

func contains(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
