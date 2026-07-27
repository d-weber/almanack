package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"almanack/internal/domain"
	"almanack/internal/events"
	"almanack/internal/holidays"
	"almanack/internal/recur"
)

// maxRangeDays bounds a single read of the calendar. The client fetches a month at a
// time and prefetches its neighbours; anything beyond two years in one request is a
// mistake or an attempt to pull the database through the JSON encoder.
const maxRangeDays = 750

type eventRequest struct {
	CalendarID   int64              `json:"calendar_id"`
	Title        string             `json:"title"`
	AllDay       bool               `json:"all_day"`
	StartsAt     *time.Time         `json:"starts_at"`
	EndsAt       *time.Time         `json:"ends_at"`
	StartDate    domain.Date        `json:"start_date"`
	EndDate      domain.Date        `json:"end_date"`
	Location     string             `json:"location"`
	URL          string             `json:"url"`
	Notes        string             `json:"notes"`
	LabelID      int64              `json:"label_id"`
	Participants []int64            `json:"participants"`
	Recurrence   *domain.Recurrence `json:"recurrence"`
	Reminders    *[]reminderRequest `json:"reminders"`
}

type reminderRequest struct {
	OffsetMinutes *int   `json:"offset_minutes"`
	DaysBefore    *int   `json:"days_before"`
	AtTimeLocal   string `json:"at_time_local"`
}

// input converts a request body into what the events service takes. Everything about
// what an edit *means* — overrides, splitting, which reminders survive — lives in
// internal/events; this only translates.
func (req eventRequest) input() (events.Input, error) {
	title, err := cleanSingleLine(req.Title, maxNameLen*2, "the title")
	if err != nil {
		return events.Input{}, err
	}
	location, err := cleanSingleLine(req.Location, maxNameLen*3, "the location")
	if err != nil {
		return events.Input{}, err
	}
	link, err := cleanLink(req.URL)
	if err != nil {
		return events.Input{}, err
	}
	notes, err := cleanText(req.Notes, maxTextLen, "the notes")
	if err != nil {
		return events.Input{}, err
	}

	in := events.Input{
		CalendarID:   req.CalendarID,
		Title:        title,
		AllDay:       req.AllDay,
		StartDate:    req.StartDate,
		EndDate:      req.EndDate,
		Location:     location,
		URL:          link,
		Notes:        notes,
		LabelID:      req.LabelID,
		Participants: req.Participants,
		Recurrence:   req.Recurrence,
	}
	if req.StartsAt != nil {
		in.StartsAt = *req.StartsAt
	}
	if req.EndsAt != nil {
		in.EndsAt = *req.EndsAt
	}
	if req.LabelID <= 0 {
		return events.Input{}, invalidf("an event needs a label")
	}
	return in, nil
}

// reminders converts the request's reminder list, rejecting the shapes the store would
// reject anyway — but with a message that says which of the two forms was expected.
func (req eventRequest) reminders(userID int64) ([]domain.Reminder, error) {
	if req.Reminders == nil {
		return nil, nil
	}
	return parseReminders(*req.Reminders, req.AllDay, userID)
}

func parseReminders(list []reminderRequest, allDay bool, userID int64) ([]domain.Reminder, error) {
	out := make([]domain.Reminder, 0, len(list))
	for i, r := range list {
		offsetSet := r.OffsetMinutes != nil
		allDaySet := r.DaysBefore != nil || r.AtTimeLocal != ""
		if offsetSet == allDaySet {
			return nil, invalidf("reminder %d needs either offset_minutes, or days_before with at_time_local", i+1)
		}
		rem := domain.Reminder{UserID: userID}
		switch {
		case offsetSet:
			if allDay {
				return nil, invalidf("an all-day event's reminders use days_before and at_time_local")
			}
			if *r.OffsetMinutes < 0 || *r.OffsetMinutes > 60*24*30 {
				return nil, invalidf("offset_minutes must be between 0 and %d", 60*24*30)
			}
			rem.OffsetMinutes = r.OffsetMinutes
		default:
			if !allDay {
				return nil, invalidf("a timed event's reminders use offset_minutes")
			}
			if r.DaysBefore == nil || *r.DaysBefore < 0 || *r.DaysBefore > 60 {
				return nil, invalidf("days_before must be between 0 and 60")
			}
			at, err := validateHHMM(r.AtTimeLocal, "at_time_local")
			if err != nil {
				return nil, err
			}
			rem.DaysBefore = r.DaysBefore
			rem.AtTimeLocal = at
		}
		out = append(out, rem)
	}
	return out, nil
}

// callerCalendars returns the calendars the caller may read, optionally narrowed by a
// CSV of ids. The narrowing is always an intersection with membership: a client asking
// for somebody else's calendar gets its own list back minus that id, never the data.
func (s *Server) callerCalendars(ctx context.Context, userID int64, csv string) ([]domain.Calendar, error) {
	cals, err := s.store.ListCalendarsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return cals, nil
	}
	wanted := map[int64]bool{}
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, invalidf("calendar_ids must be a comma-separated list of ids, got %q", part)
		}
		wanted[id] = true
	}
	filtered := cals[:0]
	for _, c := range cals {
		if wanted[c.ID] {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

func calendarIDs(cals []domain.Calendar) []int64 {
	ids := make([]int64, 0, len(cals))
	for _, c := range cals {
		ids = append(ids, c.ID)
	}
	return ids
}

type eventsResponse struct {
	Occurrences []occurrenceView `json:"occurrences"`
	Holidays    []holidayView    `json:"holidays"`
}

// handleListEvents is the main read: every occurrence in a window, plus the public
// holidays that fall in it.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	from, to, err := rangeParams(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	user := userOf(ctx)

	cals, err := s.callerCalendars(ctx, user.ID, r.URL.Query().Get("calendar_ids"))
	if err != nil {
		fail(w, r, err)
		return
	}
	occs, err := s.events.Occurrences(ctx, calendarIDs(cals), from, to)
	if err != nil {
		fail(w, r, err)
		return
	}
	dec, err := s.newDecorator(ctx, cals)
	if err != nil {
		fail(w, r, err)
		return
	}
	views := make([]occurrenceView, 0, len(occs))
	for _, occ := range occs {
		views = append(views, dec.occurrence(occ))
	}
	days, err := s.holidaysBetween(ctx, from, to, user.Lang)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, eventsResponse{Occurrences: views, Holidays: days})
}

// holidaysBetween computes the French public holidays in the window, applies the
// family's overrides and renders each name in the caller's language.
//
// A date can legitimately carry two holidays — Ascension falls on 1 May when Easter is
// 23 March — so entries are not deduplicated by date.
func (s *Server) holidaysBetween(ctx context.Context, from, to domain.Date, lang domain.Language) ([]holidayView, error) {
	overrides, err := s.store.HolidayOverrides(ctx)
	if err != nil {
		return nil, err
	}
	entries := holidays.Between(from, to, holidays.Options{AlsaceMoselle: s.cfg.AlsaceMoselle}, overrides)
	out := make([]holidayView, 0, len(entries))
	for _, e := range entries {
		out = append(out, holidayView{
			Date: e.Date,
			Name: e.Display(func(key string) string { return s.catalog.T(lang, key, nil) }),
		})
	}
	return out, nil
}

func rangeParams(r *http.Request) (from, to domain.Date, err error) {
	q := r.URL.Query()
	from, err = domain.ParseDate(q.Get("from"))
	if err != nil {
		return domain.Date{}, domain.Date{}, invalidf("from must be a YYYY-MM-DD date")
	}
	to, err = domain.ParseDate(q.Get("to"))
	if err != nil {
		return domain.Date{}, domain.Date{}, invalidf("to must be a YYYY-MM-DD date")
	}
	if to.Before(from) {
		return domain.Date{}, domain.Date{}, invalidf("to (%s) is before from (%s)", to, from)
	}
	if from.DaysUntil(to) > maxRangeDays {
		return domain.Date{}, domain.Date{}, invalidf("the window may not exceed %d days", maxRangeDays)
	}
	return from, to, nil
}

func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	var req eventRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	in, err := req.input()
	if err != nil {
		fail(w, r, err)
		return
	}
	ctx := r.Context()
	user := userOf(ctx)
	reminders, err := req.reminders(user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}

	// The service authorizes the calendar and validates the rest; membership is checked
	// there so that create and edit cannot drift apart.
	created, err := s.events.Create(ctx, user.ID, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	if reminders != nil {
		if err := s.replaceReminders(ctx, created, user.ID, reminders); err != nil {
			fail(w, r, err)
			return
		}
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{"event": created})
}

type eventDetail struct {
	Occurrence  occurrenceView     `json:"occurrence"`
	MyReminders []domain.Reminder  `json:"my_reminders"`
	Recurrence  *domain.Recurrence `json:"recurrence"`
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	date, err := optionalDate(r.URL.Query().Get("date"))
	if err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	event, err := s.store.EventByID(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.requireMember(ctx, event.CalendarID, user.ID); err != nil {
		fail(w, r, err)
		return
	}

	occ, err := s.events.Occurrence(ctx, id, date)
	if err != nil {
		fail(w, r, err)
		return
	}
	cal, err := s.store.CalendarByID(ctx, event.CalendarID)
	if err != nil {
		fail(w, r, err)
		return
	}
	dec, err := s.newDecorator(ctx, []domain.Calendar{cal})
	if err != nil {
		fail(w, r, err)
		return
	}

	reminders, err := s.listReminders(ctx, event, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	// The recurrence reported is the *series'*, which for an edited occurrence is not
	// the one on the event in the path: that id is the standalone copy the edit left
	// behind, and it has no recurrence of its own. Reading it from there told the
	// client the occurrence was a plain event, so it stopped asking which occurrences
	// a further edit should touch and the series became unreachable. The occurrence
	// knows better — internal/events resolved the copy back to its series to build it.
	seriesEvent := event
	if occ.SeriesEventID != nil && *occ.SeriesEventID != event.ID {
		seriesEvent, err = s.store.EventByID(ctx, *occ.SeriesEventID)
		if err != nil {
			fail(w, r, err)
			return
		}
	}
	var rec *domain.Recurrence
	if seriesEvent.RecurrenceID != nil {
		loaded, err := s.store.RecurrenceByID(ctx, *seriesEvent.RecurrenceID)
		if err != nil {
			fail(w, r, err)
			return
		}
		rec = &loaded
	}
	writeJSON(w, r, http.StatusOK, eventDetail{
		Occurrence:  dec.occurrence(occ),
		MyReminders: reminders,
		Recurrence:  rec,
	})
}

func (s *Server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	scope, date, err := scopeParams(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	var req eventRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	in, err := req.input()
	if err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	reminders, err := req.reminders(user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}

	// scope and date go straight through: which rows an edit touches is the events
	// service's decision, and reimplementing any of it here is how the two halves of a
	// recurring edit stop agreeing.
	updated, err := s.events.Update(ctx, user.ID, id, scope, date, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	if reminders != nil {
		if err := s.replaceReminders(ctx, updated, user.ID, reminders); err != nil {
			fail(w, r, err)
			return
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"event": updated})
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	scope, date, err := scopeParams(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.events.Delete(r.Context(), userOf(r.Context()).ID, id, scope, date); err != nil {
		fail(w, r, err)
		return
	}
	writeNoContent(w)
}

type remindersRequest struct {
	Reminders []reminderRequest `json:"reminders"`
}

// handlePutReminders replaces the caller's reminders for an event or its series.
// Reminders are per person by design: this can never touch anybody else's.
func (s *Server) handlePutReminders(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	var req remindersRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	ctx := r.Context()
	user := userOf(ctx)
	event, err := s.store.EventByID(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.requireMember(ctx, event.CalendarID, user.ID); err != nil {
		fail(w, r, err)
		return
	}
	reminders, err := parseReminders(req.Reminders, event.AllDay, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	if err := s.replaceReminders(ctx, event, user.ID, reminders); err != nil {
		fail(w, r, err)
		return
	}
	saved, err := s.listReminders(ctx, event, user.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"reminders": saved})
}

// replaceReminders files reminders against the series when there is one, so that a
// reminder set on one occurrence of "swimming, every Tuesday" applies to all of them.
func (s *Server) replaceReminders(ctx context.Context, e domain.Event, userID int64, rs []domain.Reminder) error {
	if e.RecurrenceID != nil {
		return s.store.ReplaceReminders(ctx, nil, e.RecurrenceID, userID, rs)
	}
	id := e.ID
	return s.store.ReplaceReminders(ctx, &id, nil, userID, rs)
}

func (s *Server) listReminders(ctx context.Context, e domain.Event, userID int64) ([]domain.Reminder, error) {
	var out []domain.Reminder
	var err error
	if e.RecurrenceID != nil {
		out, err = s.store.ListReminders(ctx, nil, e.RecurrenceID, userID)
	} else {
		id := e.ID
		out, err = s.store.ListReminders(ctx, &id, nil, userID)
	}
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.Reminder{}
	}
	return out, nil
}

// scopeParams reads ?scope= and ?date=. Both are optional here and required by the
// events service exactly when the event turns out to belong to a series — which is the
// only place that knows.
func scopeParams(r *http.Request) (domain.EditScope, domain.Date, error) {
	q := r.URL.Query()
	scope := domain.EditScope(strings.TrimSpace(q.Get("scope")))
	if scope != "" && !scope.Valid() {
		return "", domain.Date{}, invalidf("scope must be this, upcoming or all")
	}
	date, err := optionalDate(q.Get("date"))
	if err != nil {
		return "", domain.Date{}, err
	}
	return scope, date, nil
}

func optionalDate(raw string) (domain.Date, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return domain.Date{}, nil
	}
	d, err := domain.ParseDate(raw)
	if err != nil {
		return domain.Date{}, invalidf("date must be YYYY-MM-DD")
	}
	return d, nil
}

type searchResult struct {
	Event          domain.Event `json:"event"`
	NextOccurrence *domain.Date `json:"next_occurrence"`
}

// handleSearch finds events by text, accent- and case-insensitively. A recurring series
// appears once, with the next date it will happen.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx := r.Context()
	user := userOf(ctx)

	cals, err := s.callerCalendars(ctx, user.ID, q.Get("calendar_id"))
	if err != nil {
		fail(w, r, err)
		return
	}
	participant, err := optionalInt64(q.Get("participant"), "participant")
	if err != nil {
		fail(w, r, err)
		return
	}
	labelID, err := optionalInt64(q.Get("label_id"), "label_id")
	if err != nil {
		fail(w, r, err)
		return
	}
	text := q.Get("q")
	if len([]rune(text)) > maxNameLen*2 {
		fail(w, r, invalidf("that search is too long"))
		return
	}

	found, err := s.store.SearchEvents(ctx, calendarIDs(cals), text, participant, labelID)
	if err != nil {
		fail(w, r, err)
		return
	}

	today := domain.DateIn(s.clock.Now(), s.cfg.FamilyTZ)
	results := make([]searchResult, 0, len(found))
	for _, e := range found {
		next, err := s.nextOccurrence(ctx, e, today)
		if err != nil {
			fail(w, r, err)
			return
		}
		results = append(results, searchResult{Event: e, NextOccurrence: next})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"results": results})
}

// nextOccurrence is the date a search result will next happen: for a series the next
// expansion from today, for a one-off its own date, past or future.
func (s *Server) nextOccurrence(ctx context.Context, e domain.Event, today domain.Date) (*domain.Date, error) {
	if e.RecurrenceID == nil {
		var d domain.Date
		if e.AllDay {
			d = e.StartDate
		} else {
			d = domain.DateIn(e.StartsAt, s.cfg.FamilyTZ)
		}
		if d.IsZero() {
			return nil, nil
		}
		return &d, nil
	}
	rec, err := s.store.RecurrenceByID(ctx, *e.RecurrenceID)
	if err != nil {
		return nil, err
	}
	// Next is exclusive, so ask from yesterday: an event happening today is still the
	// next one as far as anybody searching is concerned.
	if d, ok := recur.Next(rec, today.AddDays(-1)); ok {
		return &d, nil
	}
	return nil, nil
}

func optionalInt64(raw, name string) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, invalidf("%s must be an integer", name)
	}
	return &v, nil
}
