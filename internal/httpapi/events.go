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

// maxReminders bounds one person's reminder list for one event, counted after the
// duplicates have been folded together.
//
// Nobody clicking can reach it: the editor's whole menu is ten timed shapes and five
// all-day ones, and it does not offer one the list already holds. The API is
// deliberately wider than that menu — any offset up to thirty days, any of the sixty
// days before at any minute of the clock — so the cap is set at twice what the menu can
// produce, which leaves room for the widest thing a household could plausibly ask for
// by hand and cannot express by clicking: a warning each morning of the fortnight
// before a departure, with a few more on the day itself. Past that it is not a
// household warning itself about an appointment, and the cost is not one request:
// every entry is expanded again for every occurrence on every planning pass, for as
// long as the event exists. Twenty is saved, twenty-one is refused.
const maxReminders = 20

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

// parseReminders turns a request's reminder list into the set of warnings it asks for.
//
// A set, because a reminder is its shape and nothing besides (domain.Reminder.Shape),
// so a list holding one twice asks for one warning written twice rather than for two
// warnings. It used to be stored as two: two rows, two queued notifications under
// different references falling due at the same instant, and the same sentence pushed
// twice for every occurrence, for good, until somebody edited the list by hand (#70).
// The second entry is folded into the first rather than refused, because it says
// nothing the first did not — the request is stored in full, not in part. That is also
// how the participant list arriving in this same body has always been treated, one
// layer down in internal/store, and one endpoint should not answer the same redundancy
// two ways. Refusing it would have had a cost besides: a household that saved a
// duplicate through the API before this existed would find that list rejected on the
// way back in, so the only way to edit an event would be to first spot which of two
// identical lines to delete. Folding heals it on the next save instead.
//
// The length is a different matter and is refused rather than trimmed: dropping the
// twenty-first warning would store less than was asked for, silently, which is the one
// thing folding a duplicate does not do.
func parseReminders(list []reminderRequest, allDay bool, userID int64) ([]domain.Reminder, error) {
	// Sized by the cap rather than by the request: a 2 MiB body holds on the order of a
	// hundred thousand reminders, and neither of these should be allocated for a list
	// this is going to refuse.
	out := make([]domain.Reminder, 0, min(len(list), maxReminders+1))
	seen := make(map[string]bool, maxReminders+1)
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
		shape := rem.Shape()
		if seen[shape] {
			continue
		}
		seen[shape] = true
		out = append(out, rem)
		// Checked here rather than on len(list), so that a list saying the same thing
		// a hundred times is the one warning it asks for rather than a refusal, and so
		// that a long one is refused on the entry that crosses the line instead of
		// after the whole body has been walked.
		if len(out) > maxReminders {
			return nil, invalidf("an event may not have more than %d reminders", maxReminders)
		}
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

	// The reminders reported are the ones that will actually fire for *this*
	// occurrence, so that the editor shows what the planner will do rather than a
	// second opinion. An edited occurrence inherits its series' reminders until
	// somebody changes them on that occurrence (docs/architecture.md), so the copy's
	// own list is reported once there is one — including an empty one, which is how
	// "no reminder, just for this one" is said — and the series' until then. Asking by
	// the series id and the date is the same occurrence as asking by the copy's id, so
	// the two spellings answer alike; reading them off the event named in the path is
	// what made them disagree.
	remindersOf := seriesEvent
	if occ.IsOverride {
		detached, err := s.store.RemindersDetached(ctx, occ.Event.ID, user.ID)
		if err != nil {
			fail(w, r, err)
			return
		}
		if detached {
			remindersOf = occ.Event
		}
	}
	reminders, err := s.listReminders(ctx, remindersOf, user.ID)
	if err != nil {
		fail(w, r, err)
		return
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

// searchResult carries two dates because a result is asked two different questions. The
// screen shows when the event will next happen, which for a series that has run out is
// nothing at all; the row still has to link somewhere, and that is a date the event
// really occurred on. Conflating them is how a finished series came to link to its own
// anchor — see resultDates.
type searchResult struct {
	Event          domain.Event `json:"event"`
	NextOccurrence *domain.Date `json:"next_occurrence"`
	OccurrenceDate *domain.Date `json:"occurrence_date"`
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
		next, occurrence, err := s.resultDates(ctx, e, today)
		if err != nil {
			fail(w, r, err)
			return
		}
		results = append(results, searchResult{Event: e, NextOccurrence: next, OccurrenceDate: occurrence})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"results": results})
}

// resultDates answers the two questions a search result carries.
//
// next is the date the event will happen again — for a series the next expansion from
// today, for a one-off its own date, past or future — and is nil for a series that has
// run out, which is what "no date beside this row" means on screen.
//
// occurrence is the date the row links to, and it is the same date whenever there is
// one. When there is not, it is the series' final occurrence, which is both a date the
// event genuinely happened on and the one somebody searching for a finished activity
// means. Deriving it here rather than in the browser is the point: the client used to
// fall back to the event's start date, and a series' anchor need not be an occurrence of
// its own rule — a weekly series anchored on a Monday with by_weekday of Tuesday starts
// the day after DTStart, and the editor will make one. The date becomes the ?date= of a
// GET /events/{id}, which answers 404 for a date the series does not land on, so a guess
// that misses does not draw the wrong day: it reports the event as missing.
//
// Both are nil only for a rule with no occurrence anywhere, which no date could open.
//
// It costs one recurrence row per series result, as it always has; recur.Last is reached
// only by a finished series and adds no query, because the rule is already in hand.
func (s *Server) resultDates(ctx context.Context, e domain.Event, today domain.Date) (next, occurrence *domain.Date, err error) {
	if e.RecurrenceID == nil {
		var d domain.Date
		if e.AllDay {
			d = e.StartDate
		} else {
			d = domain.DateIn(e.StartsAt, s.cfg.FamilyTZ)
		}
		if d.IsZero() {
			return nil, nil, nil
		}
		return &d, &d, nil
	}
	rec, err := s.store.RecurrenceByID(ctx, *e.RecurrenceID)
	if err != nil {
		return nil, nil, err
	}
	// Next is exclusive, so ask from yesterday: an event happening today is still the
	// next one as far as anybody searching is concerned.
	if d, ok := recur.Next(rec, today.AddDays(-1)); ok {
		return &d, &d, nil
	}
	if d, ok := recur.Last(rec); ok {
		return nil, &d, nil
	}
	return nil, nil, nil
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
