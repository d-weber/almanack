package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"almanack/internal/config"
	"almanack/internal/domain"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (e *env) createEvent(body map[string]any) domain.Event {
	e.t.Helper()
	var out struct {
		Event domain.Event `json:"event"`
	}
	e.post("/api/v1/events", body).expect(http.StatusCreated).decode(&out)
	return out.Event
}

func (e *env) listEvents(from, to string) eventsResponse {
	e.t.Helper()
	var out eventsResponse
	e.get(fmt.Sprintf("/api/v1/events?from=%s&to=%s", from, to)).expect(http.StatusOK).decode(&out)
	return out
}

// titlesByDate indexes a listing by occurrence date, which is how every assertion below
// wants to read it.
func titlesByDate(occs []occurrenceView) map[string]string {
	out := map[string]string{}
	for _, o := range occs {
		out[o.OccurrenceDate.String()] = o.Title
	}
	return out
}

func dates(occs []occurrenceView) []string {
	out := make([]string, 0, len(occs))
	for _, o := range occs {
		out = append(out, o.OccurrenceDate.String())
	}
	sort.Strings(out)
	return out
}

// weeklySeries creates "Piscine" every week from 2026-08-04, and returns the template.
func (e *env) weeklySeries(cal domain.Calendar, labelID int64) domain.Event {
	e.t.Helper()
	start := domain.MustParseDate("2026-08-04")
	return e.createEvent(map[string]any{
		"calendar_id": cal.ID,
		"title":       "Piscine",
		"all_day":     false,
		"starts_at":   "2026-08-04T14:30:00Z",
		"ends_at":     "2026-08-04T15:15:00Z",
		"label_id":    labelID,
		"recurrence": map[string]any{
			"freq":       "weekly",
			"interval":   1,
			"by_weekday": []int{int(start.Weekday())},
			"until":      "2026-12-31",
		},
	})
}

// ---------------------------------------------------------------------------
// Reading the calendar
// ---------------------------------------------------------------------------

func TestCreateAndListEvents(t *testing.T) {
	e := newEnv(t)
	user, cal := e.family()
	labels := e.labels(cal.ID)

	created := e.createEvent(map[string]any{
		"calendar_id":  cal.ID,
		"title":        "Dentiste Léo",
		"starts_at":    "2026-08-04T14:30:00Z",
		"ends_at":      "2026-08-04T15:15:00Z",
		"location":     "Cabinet du centre",
		"url":          "https://example.org/rdv",
		"notes":        "Apporter la carte vitale",
		"label_id":     labels[3].ID,
		"participants": []int64{user.ID},
		"reminders":    []map[string]any{{"offset_minutes": 1440}},
	})
	if created.ID == 0 || created.Title != "Dentiste Léo" {
		t.Fatalf("created = %+v", created)
	}

	list := e.listEvents("2026-08-01", "2026-08-31")
	if len(list.Occurrences) != 1 {
		t.Fatalf("occurrences = %+v", list.Occurrences)
	}
	occ := list.Occurrences[0]
	if occ.EventID != created.ID || occ.OccurrenceDate.String() != "2026-08-04" {
		t.Errorf("occurrence = %+v", occ)
	}
	// The colours are denormalized so the month grid never has to join anything.
	if occ.LabelName != labels[3].Name || occ.LabelColor != labels[3].Color {
		t.Errorf("label denormalization: name=%q colour=%q, want %q/%q",
			occ.LabelName, occ.LabelColor, labels[3].Name, labels[3].Color)
	}
	if occ.CalendarColor != cal.Color {
		t.Errorf("calendar_color = %q, want %q", occ.CalendarColor, cal.Color)
	}
	if occ.StartsAt == nil || !occ.StartsAt.Equal(time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)) {
		t.Errorf("starts_at = %v", occ.StartsAt)
	}
	if len(occ.Participants) != 1 || occ.Participants[0] != user.ID {
		t.Errorf("participants = %v", occ.Participants)
	}

	// The detail view carries the caller's own reminders.
	var detail eventDetail
	e.get(fmt.Sprintf("/api/v1/events/%d?date=2026-08-04", created.ID)).
		expect(http.StatusOK).decode(&detail)
	if len(detail.MyReminders) != 1 || detail.MyReminders[0].OffsetMinutes == nil ||
		*detail.MyReminders[0].OffsetMinutes != 1440 {
		t.Errorf("my_reminders = %+v", detail.MyReminders)
	}
	if detail.Recurrence != nil {
		t.Errorf("a one-off event reported a recurrence: %+v", detail.Recurrence)
	}

	// Reminders are replaceable on their own.
	var replaced struct {
		Reminders []domain.Reminder `json:"reminders"`
	}
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", created.ID), map[string]any{
		"reminders": []map[string]any{{"offset_minutes": 30}, {"offset_minutes": 60}},
	}).expect(http.StatusOK).decode(&replaced)
	if len(replaced.Reminders) != 2 {
		t.Fatalf("reminders after replace = %+v", replaced.Reminders)
	}
}

func TestAllDayEventKeepsItsDates(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	created := e.createEvent(map[string]any{
		"calendar_id": cal.ID,
		"title":       "Vacances",
		"all_day":     true,
		"start_date":  "2026-08-10",
		"end_date":    "2026-08-16",
		"label_id":    labels[7].ID,
		"reminders":   []map[string]any{{"days_before": 1, "at_time_local": "09:00"}},
	})

	list := e.listEvents("2026-08-01", "2026-08-31")
	if len(list.Occurrences) != 1 {
		t.Fatalf("occurrences = %+v", list.Occurrences)
	}
	occ := list.Occurrences[0]
	if occ.StartDate.String() != "2026-08-10" || occ.EndDate.String() != "2026-08-16" {
		t.Errorf("all-day dates = %s..%s", occ.StartDate, occ.EndDate)
	}
	if occ.StartsAt != nil || occ.EndsAt != nil {
		t.Errorf("an all-day event carries instants: %v..%v", occ.StartsAt, occ.EndsAt)
	}
	if !occ.AllDay || occ.EventID != created.ID {
		t.Errorf("occurrence = %+v", occ)
	}

	// A timed reminder on an all-day event is refused with a message that says which
	// of the two forms was expected.
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", created.ID), map[string]any{
		"reminders": []map[string]any{{"offset_minutes": 30}},
	}).expect(http.StatusBadRequest)
}

// remindersOf reads back one event's reminders for the caller, as their shapes and
// sorted. Sorted because the stored list is a set and says so: saving the same
// reminders in another order does not store that order (#65), so an assertion that
// depended on the order would be asserting something the app does not promise.
func (e *env) remindersOf(eventID int64, date string) []string {
	e.t.Helper()
	var detail eventDetail
	e.get(fmt.Sprintf("/api/v1/events/%d?date=%s", eventID, date)).
		expect(http.StatusOK).decode(&detail)
	out := make([]string, 0, len(detail.MyReminders))
	for _, r := range detail.MyReminders {
		out = append(out, r.Shape())
	}
	sort.Strings(out)
	return out
}

// timedReminders is n distinct offsets, and doubled writes every entry of a list twice.
func timedReminders(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, map[string]any{"offset_minutes": i})
	}
	return out
}

func doubled(list []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, 2*len(list))
	for _, r := range list {
		out = append(out, r, r)
	}
	return out
}

// A reminder list arriving over the wire is a set, and a bounded one.
//
// Neither half is reachable by clicking: the editor will not offer a shape the list is
// already holding, and its whole menu is ten timed shapes and five all-day ones. Both
// therefore arrive only from a hand-built request, and both used to be stored as sent —
// a duplicate as two rows falling due at the same instant, queued under two different
// references and pushing the same sentence twice for every occurrence until somebody
// edited the list by hand, and a 2 MiB body as on the order of a hundred thousand
// reminders re-expanded per occurrence on every planning pass (#70).
//
// Driven through the handlers rather than through parseReminders, because the fault was
// that the API accepted these. All three doors are walked: creating an event, editing
// one, and saving the reminder list on its own.
func TestARemindersListIsASetAndIsBounded(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	// Creating an event. Two of one shape are one warning, and the third is its own.
	timed := e.createEvent(map[string]any{
		"calendar_id": cal.ID,
		"title":       "Dentiste",
		"starts_at":   "2026-08-04T14:30:00Z",
		"ends_at":     "2026-08-04T15:15:00Z",
		"label_id":    labels[3].ID,
		"reminders": []map[string]any{
			{"offset_minutes": 30}, {"offset_minutes": 30}, {"offset_minutes": 60},
		},
	})
	if got := e.remindersOf(timed.ID, "2026-08-04"); !slices.Equal(got, []string{"m30", "m60"}) {
		t.Errorf("creating an event with 30, 30 and 60 minutes stored %v, want one row per shape", got)
	}

	// Saving the list on its own. Distinct shapes all survive; the repeat does not.
	var replaced struct {
		Reminders []domain.Reminder `json:"reminders"`
	}
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", timed.ID), map[string]any{
		"reminders": []map[string]any{
			{"offset_minutes": 60}, {"offset_minutes": 30}, {"offset_minutes": 60}, {"offset_minutes": 0},
		},
	}).expect(http.StatusOK).decode(&replaced)
	if len(replaced.Reminders) != 3 {
		t.Errorf("PUT answered with %d reminders for three shapes: %+v", len(replaced.Reminders), replaced.Reminders)
	}
	if got := e.remindersOf(timed.ID, "2026-08-04"); !slices.Equal(got, []string{"m0", "m30", "m60"}) {
		t.Errorf("saving 60, 30, 60 and 0 stored %v", got)
	}

	// Editing an event carries a reminder list too, and it is the same list.
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/events/%d", timed.ID), map[string]any{
		"calendar_id": cal.ID,
		"title":       "Dentiste Léo",
		"starts_at":   "2026-08-04T14:30:00Z",
		"ends_at":     "2026-08-04T15:15:00Z",
		"label_id":    labels[3].ID,
		"reminders":   []map[string]any{{"offset_minutes": 15}, {"offset_minutes": 15}},
	}).expect(http.StatusOK)
	if got := e.remindersOf(timed.ID, "2026-08-04"); !slices.Equal(got, []string{"m15"}) {
		t.Errorf("editing an event with 15 and 15 minutes stored %v", got)
	}

	// The all-day shape is two fields, and both of them count: the same day at another
	// time is a different warning, and so is the same time on another day. Only the
	// pair repeated verbatim is the same reminder twice.
	allDay := e.createEvent(map[string]any{
		"calendar_id": cal.ID,
		"title":       "Vacances",
		"all_day":     true,
		"start_date":  "2026-08-10",
		"end_date":    "2026-08-16",
		"label_id":    labels[7].ID,
		"reminders": []map[string]any{
			{"days_before": 1, "at_time_local": "09:00"},
			{"days_before": 1, "at_time_local": "09:00"},
		},
	})
	if got := e.remindersOf(allDay.ID, "2026-08-10"); !slices.Equal(got, []string{"d1@09:00"}) {
		t.Errorf("creating an all-day event with the same warning twice stored %v", got)
	}
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", allDay.ID), map[string]any{
		"reminders": []map[string]any{
			{"days_before": 1, "at_time_local": "09:00"},
			{"days_before": 1, "at_time_local": "20:00"},
			{"days_before": 1, "at_time_local": "09:00"},
			{"days_before": 2, "at_time_local": "09:00"},
			{"days_before": 0, "at_time_local": "09:00"},
		},
	}).expect(http.StatusOK)
	want := []string{"d0@09:00", "d1@09:00", "d1@20:00", "d2@09:00"}
	if got := e.remindersOf(allDay.ID, "2026-08-10"); !slices.Equal(got, want) {
		t.Errorf("all-day reminders stored %v, want %v", got, want)
	}

	// The cap, at the boundary, and written as the numbers rather than as maxReminders.
	// Twenty is a decision about what a household could plausibly ask for — twice the
	// fifteen shapes the editor's menu can produce — and it is exactly the thing a
	// careless edit changes. A boundary phrased in terms of the constant moves with it
	// and holds nothing: raising the cap to ten thousand would leave this section green
	// and every word of it true. Raising it deliberately is allowed and lands here, in
	// the commit that raises it.
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", timed.ID), map[string]any{
		"reminders": timedReminders(20),
	}).expect(http.StatusOK)
	atTheCap := e.remindersOf(timed.ID, "2026-08-04")
	if len(atTheCap) != 20 {
		t.Fatalf("a list of exactly 20 stored %d of them, and maxReminders is %d: twenty is the "+
			"number the editor's menu and this bound were chosen against", len(atTheCap), maxReminders)
	}

	// Twenty-one is refused, with the code the client shows a message for — and refused
	// before anything is written, so what is stored is still the list that was accepted.
	res := e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", timed.ID), map[string]any{
		"reminders": timedReminders(21),
	}).expect(http.StatusBadRequest)
	if code := res.errorCode(); code != codeInvalid {
		t.Errorf("a list of 21 was refused as %q, want %q", code, codeInvalid)
	}
	if got := e.remindersOf(timed.ID, "2026-08-04"); !slices.Equal(got, atTheCap) {
		t.Errorf("a refused list changed what was stored: %v, want the 20 that were accepted", got)
	}

	// Creating an event with too many is refused the same way, and the event with it:
	// the list is parsed before anything is written.
	e.post("/api/v1/events", map[string]any{
		"calendar_id": cal.ID,
		"title":       "Trop",
		"starts_at":   "2026-08-05T14:30:00Z",
		"ends_at":     "2026-08-05T15:15:00Z",
		"label_id":    labels[3].ID,
		"reminders":   timedReminders(21),
	}).expect(http.StatusBadRequest)
	if occs := e.listEvents("2026-08-05", "2026-08-05").Occurrences; len(occs) != 0 {
		t.Errorf("the event was created anyway: %+v", occs)
	}

	// The cap counts warnings, not lines: a list that says the same twenty things twice
	// is asking for twenty, and a hundred thousand copies of one is asking for one.
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", timed.ID), map[string]any{
		"reminders": doubled(timedReminders(20)),
	}).expect(http.StatusOK)
	if got := e.remindersOf(timed.ID, "2026-08-04"); !slices.Equal(got, atTheCap) {
		t.Errorf("20 shapes sent twice each stored %d rows", len(got))
	}
}

// A household that saved a duplicate through the API before there was a rule against it
// still has both rows: no migration goes looking, and nothing rewrites what is stored.
// What the rule must not do is strand them — their event has to open, and a save has to
// go through. It does, and it heals: the editor sends back the list it was given, that
// list folds to one, and reconciliation drops the row that is no longer in it.
func TestADuplicateSavedBeforeTheRuleStillOpensAndHealsOnTheNextSave(t *testing.T) {
	e := newEnv(t)
	user, cal := e.family()
	labels := e.labels(cal.ID)

	created := e.createEvent(map[string]any{
		"calendar_id": cal.ID,
		"title":       "Dentiste",
		"starts_at":   "2026-08-04T14:30:00Z",
		"ends_at":     "2026-08-04T15:15:00Z",
		"label_id":    labels[3].ID,
	})
	// Written through the store, which is now the only way to produce the row: the
	// store stores exactly the list it is handed, and it is the API that has the rule.
	thirty := 30
	if err := e.store.ReplaceReminders(t.Context(), &created.ID, nil, user.ID,
		[]domain.Reminder{{OffsetMinutes: &thirty}, {OffsetMinutes: &thirty}}); err != nil {
		t.Fatalf("seed a duplicate: %v", err)
	}
	if got := e.remindersOf(created.ID, "2026-08-04"); len(got) != 2 {
		t.Fatalf("the duplicate was not stored, so this proves nothing: %v", got)
	}

	// What the editor sends is what it loaded, duplicate and all.
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", created.ID), map[string]any{
		"reminders": []map[string]any{{"offset_minutes": 30}, {"offset_minutes": 30}},
	}).expect(http.StatusOK)
	if got := e.remindersOf(created.ID, "2026-08-04"); !slices.Equal(got, []string{"m30"}) {
		t.Errorf("saving the duplicate list back stored %v, want the one warning it asks for", got)
	}
}

func TestHolidaysAreLocalizedAndCanShareADate(t *testing.T) {
	e := newEnv(t)
	e.family()

	list := e.listEvents("2026-08-01", "2026-08-31")
	found := false
	for _, h := range list.Holidays {
		if h.Date.String() == "2026-08-15" {
			found = true
			if h.Name != "Assomption" {
				t.Errorf("15 August is %q, want the French catalog's Assomption", h.Name)
			}
		}
	}
	if !found {
		t.Fatalf("15 August is missing from %+v", list.Holidays)
	}

	// In English the same date comes back through the same catalog.
	e.do(http.MethodPatch, "/api/v1/me", map[string]any{"lang": "en"}).expect(http.StatusOK)
	list = e.listEvents("2026-08-01", "2026-08-31")
	for _, h := range list.Holidays {
		if h.Date.String() == "2026-08-15" && h.Name == "Assomption" {
			t.Errorf("the holiday name was not localized: %+v", h)
		}
	}

	// Two holidays can fall on one day — Ascension lands on 1 May when Easter is
	// 23 March — and both must be reported.
	list = e.listEvents("2008-04-25", "2008-05-05")
	onMayDay := 0
	for _, h := range list.Holidays {
		if h.Date.String() == "2008-05-01" {
			onMayDay++
		}
	}
	if onMayDay != 2 {
		t.Errorf("1 May 2008 carries %d holidays, want 2 (Labour Day and Ascension): %+v",
			onMayDay, list.Holidays)
	}
}

func TestEventsRangeValidation(t *testing.T) {
	e := newEnv(t)
	e.family()

	for _, query := range []string{
		"",
		"?from=2026-08-01",
		"?from=nonsense&to=2026-08-31",
		"?from=2026-08-31&to=2026-08-01",
		"?from=2020-01-01&to=2030-01-01",
	} {
		e.get("/api/v1/events" + query).expect(http.StatusBadRequest)
	}
}

// TestHostileTitleIsData is the XSS regression: a title that is an attack in HTML must
// come back as data, from an endpoint that is never HTML.
func TestHostileTitleIsData(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	const hostile = `<img src=x onerror=alert(1)>`
	created := e.createEvent(map[string]any{
		"calendar_id": cal.ID,
		"title":       hostile,
		"starts_at":   "2026-08-04T14:30:00Z",
		"ends_at":     "2026-08-04T15:15:00Z",
		"notes":       `</script><script>alert(2)</script>`,
		"label_id":    labels[0].ID,
	})

	res := e.get("/api/v1/events?from=2026-08-01&to=2026-08-31").expect(http.StatusOK)
	if ct := res.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q; the API must never answer HTML", ct)
	}
	if ct := res.header.Get("X-Content-Type-Options"); ct != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", ct)
	}
	raw := string(res.body)
	if strings.Contains(raw, "<img") || strings.Contains(raw, "<script") {
		t.Fatalf("raw markup survived into the response body: %s", truncate(raw, 400))
	}
	if !strings.Contains(raw, `\u003cimg`) {
		t.Errorf("the title does not look JSON-escaped: %s", truncate(raw, 400))
	}

	// Escaped on the wire, identical after decoding: it is data, not markup.
	var list eventsResponse
	res.decode(&list)
	if len(list.Occurrences) != 1 || list.Occurrences[0].Title != hostile {
		t.Fatalf("round-tripped title = %q, want %q", list.Occurrences[0].Title, hostile)
	}
	if list.Occurrences[0].EventID != created.ID {
		t.Errorf("event id = %d, want %d", list.Occurrences[0].EventID, created.ID)
	}

	// The same is true of the detail read and of search.
	var detail eventDetail
	e.get(fmt.Sprintf("/api/v1/events/%d?date=2026-08-04", created.ID)).
		expect(http.StatusOK).decode(&detail)
	if detail.Occurrence.Title != hostile {
		t.Errorf("detail title = %q", detail.Occurrence.Title)
	}
	search := e.get("/api/v1/search?q=img").expect(http.StatusOK)
	if strings.Contains(string(search.body), "<img") {
		t.Errorf("search leaked raw markup: %s", truncate(string(search.body), 300))
	}
}

// An event's link is the one free-text field in this application that becomes an href,
// and the browser's URL parser removes ASCII tab, LF and CR from anywhere in a URL
// before it decides what the scheme is. So a link holding one is not the link that is
// printed on the screen, and this refuses to store it on the way in. The guardrail is
// web/js/dom.js, which strips them before deciding whether a scheme may be followed
// (e2e/safe-href.spec.js); this is the second lock on the same door.
func TestALinkCannotHideAControlCharacter(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	event := func(link string) map[string]any {
		return map[string]any{
			"calendar_id": cal.ID,
			"title":       "Rendez-vous",
			"starts_at":   "2026-08-04T14:30:00Z",
			"ends_at":     "2026-08-04T15:15:00Z",
			"label_id":    labels[0].ID,
			"url":         link,
		}
	}

	for _, tc := range []struct {
		name string
		link string
		want int
	}{
		{"an ordinary link", "https://example.org/rdv", http.StatusCreated},
		{"no link at all", "", http.StatusCreated},
		{"a tab inside the path", "https://example.org/r\tdv", http.StatusBadRequest},
		{"a newline inside the path", "https://example.org/r\ndv", http.StatusBadRequest},
		{"a tab hiding in the scheme", "java\tscript:alert(1)", http.StatusBadRequest},
		{"a newline hiding in the scheme", "java\nscript:alert(1)", http.StatusBadRequest},
		{"a carriage return hiding in the scheme", "java\rscript:alert(1)", http.StatusBadRequest},
		{"a leading NUL", "\x00javascript:alert(1)", http.StatusBadRequest},
		{"a leading tab", "\tjavascript:alert(1)", http.StatusBadRequest},
		{"a scheme that is simply not http", "javascript:alert(1)", http.StatusBadRequest},
	} {
		// Checked rather than asserted, so that one row getting through does not hide
		// which of the others do: what is let past is the whole diagnosis.
		res := e.do(http.MethodPost, "/api/v1/events", event(tc.link))
		if res.status != tc.want {
			t.Errorf("%s (%q): status = %d, want %d (body: %s)",
				tc.name, tc.link, res.status, tc.want, truncate(string(res.body), 200))
		}
	}

	// The edit path takes the same body through the same validator, and an event given a
	// hidden character on a later save must be refused for the same reason.
	created := e.createEvent(event("https://example.org/rdv"))
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/events/%d", created.ID), event("https://example.org/r\tdv")).
		expect(http.StatusBadRequest)
}

// The rule above applies on write and nowhere else. A calendar written by an older
// binary may hold a link with a tab in it — 0.2.0 accepted one, since cleanText tolerated
// tab and newline — and upgrading must not make that event unreadable. So the row goes in
// underneath the HTTP layer, exactly as an old release left it, and is read back through
// every path the app has for reading an event.
func TestALinkStoredBeforeTheRuleTightenedStillReads(t *testing.T) {
	e := newEnv(t)
	user, cal := e.family()
	labels := e.labels(cal.ID)

	const legacy = "https://example.org/r\tdv"
	stored, err := e.store.CreateEvent(e.t.Context(), domain.Event{
		CalendarID: cal.ID,
		Title:      "Rendez-vous d'avant",
		StartsAt:   time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC),
		EndsAt:     time.Date(2026, 8, 4, 15, 15, 0, 0, time.UTC),
		URL:        legacy,
		LabelID:    labels[0].ID,
		CreatedBy:  user.ID,
		UpdatedBy:  user.ID,
	}, nil)
	if err != nil {
		t.Fatalf("write the legacy row: %v", err)
	}

	list := e.listEvents("2026-08-01", "2026-08-31")
	if len(list.Occurrences) != 1 || list.Occurrences[0].URL != legacy {
		t.Fatalf("listed url = %q, want %q", list.Occurrences[0].URL, legacy)
	}

	var detail eventDetail
	e.get(fmt.Sprintf("/api/v1/events/%d?date=2026-08-04", stored.ID)).
		expect(http.StatusOK).decode(&detail)
	if detail.Occurrence.URL != legacy {
		t.Errorf("detail url = %q, want %q", detail.Occurrence.URL, legacy)
	}
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

func TestNonMemberIsRefused(t *testing.T) {
	e := newEnv(t)
	_, mine := e.family()
	labels := e.labels(mine.ID)

	// An event of ours, to be looked for from the outside.
	ours := e.createEvent(map[string]any{
		"calendar_id": mine.ID,
		"title":       "Dentiste",
		"starts_at":   "2026-08-04T14:30:00Z",
		"ends_at":     "2026-08-04T15:15:00Z",
		"label_id":    labels[0].ID,
	})

	// A second family, with no overlap at all.
	stranger := e.createUser("voisin@example.org", "Voisin")
	theirs := e.createCalendar(stranger, "Voisins")
	strangerClient := e.login(e.newClient(), stranger.Email)

	// Their calendar is invisible to us: 403 on the calendar itself…
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/calendars/%d", theirs.ID),
		map[string]string{"name": "Chez nous"}).expect(http.StatusForbidden)
	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/calendars/%d", theirs.ID), nil).
		expect(http.StatusForbidden)
	e.post(fmt.Sprintf("/api/v1/calendars/%d/invites", theirs.ID), nil).
		expect(http.StatusForbidden)
	e.get(fmt.Sprintf("/api/v1/calendars/%d/invites", theirs.ID)).
		expect(http.StatusForbidden)

	// …and 403 on its events, consistently: the same code everywhere, so a client can
	// tell "not yours" from "gone".
	strangerEvent := func(path string, method string, body any) {
		t.Helper()
		res := e.request(strangerClient, method, path, body)
		if res.status != http.StatusForbidden {
			t.Errorf("%s %s from a non-member: status = %d, want 403", method, path, res.status)
		}
	}
	strangerEvent(fmt.Sprintf("/api/v1/events/%d?date=2026-08-04", ours.ID), http.MethodGet, nil)
	strangerEvent(fmt.Sprintf("/api/v1/events/%d", ours.ID), http.MethodPatch, map[string]any{
		"title": "Volé", "starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
		"label_id": labels[0].ID,
	})
	strangerEvent(fmt.Sprintf("/api/v1/events/%d", ours.ID), http.MethodDelete, nil)
	strangerEvent(fmt.Sprintf("/api/v1/events/%d/reminders", ours.ID), http.MethodPut,
		map[string]any{"reminders": []map[string]any{}})

	// Creating an event in a calendar you do not belong to is refused too.
	res := e.request(strangerClient, http.MethodPost, "/api/v1/events", map[string]any{
		"calendar_id": mine.ID, "title": "Intrusion", "label_id": labels[0].ID,
		"starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
	})
	res.expect(http.StatusForbidden)

	// And their range read simply does not contain our events, even when they ask for
	// our calendar by id.
	var list eventsResponse
	e.request(strangerClient, http.MethodGet,
		fmt.Sprintf("/api/v1/events?from=2026-08-01&to=2026-08-31&calendar_ids=%d", mine.ID), nil).
		expect(http.StatusOK).decode(&list)
	if len(list.Occurrences) != 0 {
		t.Errorf("a non-member read %d occurrences of another family's calendar", len(list.Occurrences))
	}
}

func TestOnlyTheCreatorRemovesMembers(t *testing.T) {
	e := newEnv(t)
	owner, cal := e.family()
	other := e.createUser("papa@example.org", "Papa")
	if err := e.store.AddMember(t.Context(), cal.ID, other.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	otherClient := e.login(e.newClient(), other.Email)

	// A member cannot remove another member…
	e.request(otherClient, http.MethodDelete,
		fmt.Sprintf("/api/v1/calendars/%d/members/%d", cal.ID, owner.ID), nil).
		expect(http.StatusForbidden)

	// …and the creator cannot be removed at all: they leave instead.
	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/calendars/%d/members/%d", cal.ID, owner.ID), nil).
		expect(http.StatusConflict)

	// The creator may remove anybody else.
	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/calendars/%d/members/%d", cal.ID, other.ID), nil).
		expect(http.StatusNoContent)
	e.request(otherClient, http.MethodGet, "/api/v1/me", nil).expect(http.StatusOK)
}

func TestLeaveTransfersTheCalendar(t *testing.T) {
	e := newEnv(t)
	owner, cal := e.family()
	other := e.createUser("papa@example.org", "Papa")
	if err := e.store.AddMember(t.Context(), cal.ID, other.ID); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Deleting a calendar somebody else is still in is refused.
	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/calendars/%d", cal.ID), nil).
		expect(http.StatusConflict)

	e.post(fmt.Sprintf("/api/v1/calendars/%d/leave", cal.ID), nil).expect(http.StatusNoContent)

	reloaded, err := e.store.CalendarByID(t.Context(), cal.ID)
	if err != nil {
		t.Fatalf("the calendar was deleted when its creator left: %v", err)
	}
	if reloaded.CreatorID != other.ID {
		t.Errorf("creator = %d after the original creator left, want %d", reloaded.CreatorID, other.ID)
	}
	if member, _ := e.store.IsMember(t.Context(), cal.ID, owner.ID); member {
		t.Errorf("the caller is still a member after leaving")
	}

	// The last member out takes the calendar with them.
	otherClient := e.login(e.newClient(), other.Email)
	e.request(otherClient, http.MethodPost, fmt.Sprintf("/api/v1/calendars/%d/leave", cal.ID), nil).
		expect(http.StatusNoContent)
	if _, err := e.store.CalendarByID(t.Context(), cal.ID); err == nil {
		t.Errorf("an empty calendar was left behind")
	}
}

// A calendar's members all leaving at the same moment used to strand it. Counting the
// members and acting on the count were separate transactions, so every request read a
// count above one, every one took the "somebody is still here" branch, and every
// membership went — leaving a calendar with no members, which no query returns to
// anybody and which nothing left in the application can reach, its events included.
//
// Whichever request is last must find itself alone and take the calendar with it. Which
// one that is does not matter and is not asserted; the two states this leaves are "gone"
// and "still has a member", and the bug is the third.
func TestMembersLeavingAtOnceDoNotStrandTheCalendar(t *testing.T) {
	e := newEnv(t)
	owner, cal := e.family()

	members := []domain.User{owner}
	clients := []*http.Client{e.client}
	for _, name := range []string{"papa", "leo", "mamie"} {
		u := e.createUser(name+"@example.org", name)
		if err := e.store.AddMember(t.Context(), cal.ID, u.ID); err != nil {
			t.Fatalf("add member %s: %v", name, err)
		}
		members = append(members, u)
		clients = append(clients, e.login(e.newClient(), u.Email))
	}

	path := fmt.Sprintf("/api/v1/calendars/%d/leave", cal.ID)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Any answer is legitimate — a request that loses the race for the lock may
			// find the calendar already gone — so the status is not what is asserted.
			e.request(c, http.MethodPost, path, nil)
		}()
	}
	close(start)
	wg.Wait()

	_, err := e.store.CalendarByID(t.Context(), cal.ID)
	if err == nil {
		// It survived, so somebody must still be in it.
		count, err := e.store.CountMembers(t.Context(), cal.ID)
		if err != nil {
			t.Fatalf("CountMembers: %v", err)
		}
		if count == 0 {
			t.Error("the calendar survived with no members: it is unreachable, and so is everything in it")
		}
		return
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CalendarByID: %v", err)
	}
	// It was deleted, which is the other correct outcome: nobody is left in it.
	for _, u := range members {
		if member, _ := e.store.IsMember(t.Context(), cal.ID, u.ID); member {
			t.Errorf("user %d is a member of a calendar that was deleted", u.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Scoped edits — the part that has to round-trip exactly
// ---------------------------------------------------------------------------

func TestScopedEditThisOccurrenceOnly(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)
	series := e.weeklySeries(cal, labels[4].ID)

	list := e.listEvents("2026-08-01", "2026-08-31")
	if got, want := dates(list.Occurrences), []string{"2026-08-04", "2026-08-11", "2026-08-18", "2026-08-25"}; !equalStrings(got, want) {
		t.Fatalf("weekly expansion = %v, want %v", got, want)
	}

	// Move one occurrence's title and time; everything else must be untouched.
	e.do(http.MethodPatch,
		fmt.Sprintf("/api/v1/events/%d?scope=this&date=2026-08-11", series.ID),
		map[string]any{
			"title":     "Piscine (annulée, remplacée par le cinéma)",
			"starts_at": "2026-08-11T16:00:00Z", "ends_at": "2026-08-11T17:30:00Z",
			"label_id": labels[5].ID,
		}).expect(http.StatusOK)

	list = e.listEvents("2026-08-01", "2026-08-31")
	titles := titlesByDate(list.Occurrences)
	if len(titles) != 4 {
		t.Fatalf("occurrences after the edit = %v", titles)
	}
	if titles["2026-08-11"] != "Piscine (annulée, remplacée par le cinéma)" {
		t.Errorf("the edited occurrence = %q", titles["2026-08-11"])
	}
	for _, date := range []string{"2026-08-04", "2026-08-18", "2026-08-25"} {
		if titles[date] != "Piscine" {
			t.Errorf("%s = %q, want the series title untouched", date, titles[date])
		}
	}

	for _, occ := range list.Occurrences {
		if occ.OccurrenceDate.String() != "2026-08-11" {
			continue
		}
		if !occ.IsOverride {
			t.Errorf("the edited occurrence is not marked is_override")
		}
		if occ.SeriesEventID == nil || *occ.SeriesEventID != series.ID {
			t.Errorf("series_event_id = %v, want %d", occ.SeriesEventID, series.ID)
		}
		if occ.LabelName != labels[5].Name {
			t.Errorf("the override kept the old label: %q", occ.LabelName)
		}
	}

	// Cancelling one occurrence removes exactly one.
	e.do(http.MethodDelete,
		fmt.Sprintf("/api/v1/events/%d?scope=this&date=2026-08-18", series.ID), nil).
		expect(http.StatusNoContent)
	list = e.listEvents("2026-08-01", "2026-08-31")
	if got, want := dates(list.Occurrences), []string{"2026-08-04", "2026-08-11", "2026-08-25"}; !equalStrings(got, want) {
		t.Fatalf("after cancelling one occurrence = %v, want %v", got, want)
	}
}

// TestEditedOccurrenceReportsTheRemindersThatWillFire is the reader's half of the rule
// in docs/architecture.md: an edited occurrence inherits its series' reminders until
// somebody changes them on that occurrence, so `my_reminders` has to report whichever
// of the two will actually fire. The list must also be the same whichever way the
// occurrence is named — by the copy's id, or by the series' id and the date — because
// the editor shows it and a save writes it back.
func TestEditedOccurrenceReportsTheRemindersThatWillFire(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)
	series := e.weeklySeries(cal, labels[4].ID)

	e.do(http.MethodPatch,
		fmt.Sprintf("/api/v1/events/%d?scope=this&date=2026-08-11", series.ID),
		map[string]any{
			"title": "Piscine (déplacée)", "label_id": labels[4].ID,
			"starts_at": "2026-08-11T16:00:00Z", "ends_at": "2026-08-11T17:00:00Z",
		}).expect(http.StatusOK)

	var copyID int64
	for _, occ := range e.listEvents("2026-08-01", "2026-08-31").Occurrences {
		if occ.OccurrenceDate.String() == "2026-08-11" {
			copyID = occ.EventID
		}
	}
	if copyID == 0 || copyID == series.ID {
		t.Fatalf("the edited occurrence is addressed by %d; want the override copy's own id", copyID)
	}

	remindersFor := func(path string) []domain.Reminder {
		t.Helper()
		var detail eventDetail
		e.get(path).expect(http.StatusOK).decode(&detail)
		return detail.MyReminders
	}
	offsetOf := func(rs []domain.Reminder) string {
		if len(rs) != 1 || rs[0].OffsetMinutes == nil {
			return fmt.Sprintf("%+v", rs)
		}
		return fmt.Sprintf("%d minutes before", *rs[0].OffsetMinutes)
	}
	byCopy := fmt.Sprintf("/api/v1/events/%d?date=2026-08-11", copyID)
	bySeries := fmt.Sprintf("/api/v1/events/%d?date=2026-08-11", series.ID)

	// The reminder is set on the series *after* the occurrence was edited, which is the
	// case a copy taken at the moment of the edit can never account for.
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", series.ID), map[string]any{
		"reminders": []map[string]any{{"offset_minutes": 30}},
	}).expect(http.StatusOK)

	for _, path := range []string{byCopy, bySeries} {
		if got := offsetOf(remindersFor(path)); got != "30 minutes before" {
			t.Errorf("%s reports my_reminders = %s, want the series' 30 minutes:"+
				" an edited occurrence inherits until somebody changes them on it", path, got)
		}
	}

	// Changing them on this one occurrence, the way the editor does, replaces the
	// inherited list for that date and nothing else.
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", copyID), map[string]any{
		"reminders": []map[string]any{{"offset_minutes": 120}},
	}).expect(http.StatusOK)

	for _, path := range []string{byCopy, bySeries} {
		if got := offsetOf(remindersFor(path)); got != "120 minutes before" {
			t.Errorf("%s reports my_reminders = %s after two hours were set on that occurrence", path, got)
		}
	}
	if got := offsetOf(remindersFor(fmt.Sprintf("/api/v1/events/%d?date=2026-08-18", series.ID))); got != "30 minutes before" {
		t.Errorf("an untouched occurrence reports my_reminders = %s, want the series' 30 minutes", got)
	}

	// And taking it off this one occurrence — an empty list, which is the only way to
	// say "no reminder, just for this one" — is remembered as a choice rather than read
	// back as "nothing has been set here", which would inherit the series' again.
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", copyID), map[string]any{
		"reminders": []map[string]any{},
	}).expect(http.StatusOK)

	for _, path := range []string{byCopy, bySeries} {
		if rs := remindersFor(path); len(rs) != 0 {
			t.Errorf("%s still reports my_reminders = %+v after the reminder was removed from that occurrence", path, rs)
		}
	}
	// Every other lesson keeps it.
	if got := offsetOf(remindersFor(fmt.Sprintf("/api/v1/events/%d?date=2026-08-18", series.ID))); got != "30 minutes before" {
		t.Errorf("an untouched occurrence reports my_reminders = %s, want the series' one", got)
	}
}

// TestSecondEditOfTheSameOccurrence walks the route the client actually takes. An edit
// to one occurrence answers with a standalone copy of the event, and every later
// request about that occurrence is addressed to the copy's id — so this is the path a
// family takes the moment they change their mind twice, and it used to lose the edit.
func TestSecondEditOfTheSameOccurrence(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)
	series := e.weeklySeries(cal, labels[4].ID)

	// First edit: 11 August moves to Thursday the 13th.
	e.do(http.MethodPatch,
		fmt.Sprintf("/api/v1/events/%d?scope=this&date=2026-08-11", series.ID),
		map[string]any{
			"title": "Piscine (déplacée)", "label_id": labels[4].ID,
			"starts_at": "2026-08-13T16:00:00Z", "ends_at": "2026-08-13T17:00:00Z",
		}).expect(http.StatusOK)

	// The id the client now holds for that occurrence is the copy's, not the series'.
	var copyID int64
	for _, occ := range e.listEvents("2026-08-01", "2026-08-31").Occurrences {
		if occ.OccurrenceDate.String() == "2026-08-11" {
			copyID = occ.EventID
		}
	}
	if copyID == 0 || copyID == series.ID {
		t.Fatalf("the edited occurrence is addressed by %d; want the override copy's own id", copyID)
	}

	// Opening it must still find the series: this is what decides whether the client
	// asks "this / this and following / the whole series" before saving.
	var detail eventDetail
	e.get(fmt.Sprintf("/api/v1/events/%d?date=2026-08-11", copyID)).expect(http.StatusOK).decode(&detail)
	if detail.Recurrence == nil {
		t.Error("recurrence = null for an edited occurrence; the client will treat it as a one-off")
	}
	if !detail.Occurrence.IsOverride {
		t.Error("is_override = false for an edited occurrence")
	}
	if got := detail.Occurrence.OccurrenceDate.String(); got != "2026-08-11" {
		t.Errorf("occurrence_date = %s, want the date in the series", got)
	}
	if detail.Occurrence.SeriesEventID == nil || *detail.Occurrence.SeriesEventID != series.ID {
		t.Errorf("series_event_id = %v, want %d", detail.Occurrence.SeriesEventID, series.ID)
	}

	// Second edit, addressed to the copy.
	e.do(http.MethodPatch,
		fmt.Sprintf("/api/v1/events/%d?scope=this&date=2026-08-11", copyID),
		map[string]any{
			"title": "Piscine (re-déplacée)", "label_id": labels[4].ID,
			"starts_at": "2026-08-14T16:00:00Z", "ends_at": "2026-08-14T17:00:00Z",
		}).expect(http.StatusOK)

	list := e.listEvents("2026-08-01", "2026-08-31")
	titles := titlesByDate(list.Occurrences)
	if len(titles) != 4 {
		t.Fatalf("occurrences after the second edit = %v", titles)
	}
	if titles["2026-08-11"] != "Piscine (re-déplacée)" {
		t.Errorf("the twice-edited occurrence = %q", titles["2026-08-11"])
	}

	// And deleting it leaves it deleted, rather than restoring the series' original.
	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/events/%d?scope=this&date=2026-08-11", copyID), nil).
		expect(http.StatusNoContent)
	list = e.listEvents("2026-08-01", "2026-08-31")
	if got, want := dates(list.Occurrences), []string{"2026-08-04", "2026-08-18", "2026-08-25"}; !equalStrings(got, want) {
		t.Fatalf("after deleting the edited occurrence = %v, want %v", got, want)
	}
}

func TestScopedEditUpcomingSplitsTheSeries(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)
	series := e.weeklySeries(cal, labels[4].ID)

	e.do(http.MethodPatch,
		fmt.Sprintf("/api/v1/events/%d?scope=upcoming&date=2026-08-18", series.ID),
		map[string]any{
			"title":     "Piscine (nouvel horaire)",
			"starts_at": "2026-08-18T15:30:00Z", "ends_at": "2026-08-18T16:30:00Z",
			"label_id": labels[4].ID,
			"recurrence": map[string]any{
				"freq": "weekly", "interval": 1,
				"by_weekday": []int{int(domain.MustParseDate("2026-08-18").Weekday())},
				"until":      "2026-12-31",
			},
		}).expect(http.StatusOK)

	list := e.listEvents("2026-08-01", "2026-08-31")
	titles := titlesByDate(list.Occurrences)
	if got, want := dates(list.Occurrences), []string{"2026-08-04", "2026-08-11", "2026-08-18", "2026-08-25"}; !equalStrings(got, want) {
		t.Fatalf("after the split = %v, want %v", got, want)
	}
	for _, date := range []string{"2026-08-04", "2026-08-11"} {
		if titles[date] != "Piscine" {
			t.Errorf("%s = %q, want the first half untouched", date, titles[date])
		}
	}
	for _, date := range []string{"2026-08-18", "2026-08-25"} {
		if titles[date] != "Piscine (nouvel horaire)" {
			t.Errorf("%s = %q, want the new series title", date, titles[date])
		}
	}

	// The two halves are genuinely different series.
	var first, second int64
	for _, occ := range list.Occurrences {
		switch occ.OccurrenceDate.String() {
		case "2026-08-04":
			first = occ.EventID
		case "2026-08-25":
			second = occ.EventID
		}
	}
	if first == second || first == 0 || second == 0 {
		t.Fatalf("the split did not produce two series: %d and %d", first, second)
	}
}

// TestRepeatCannotBeAddedOrRemovedOverTheWire pins the status code the editor relies on:
// both transitions used to answer 200 and store nothing, so a family who ticked "repeat
// weekly" was told their change was saved and it never was.
func TestRepeatCannotBeAddedOrRemovedOverTheWire(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	plain := e.createEvent(map[string]any{
		"calendar_id": cal.ID, "title": "Dentiste", "all_day": false,
		"starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
		"label_id": labels[4].ID,
	})
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/events/%d", plain.ID), map[string]any{
		"title": "Dentiste", "label_id": labels[4].ID,
		"starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
		"recurrence": map[string]any{"freq": "weekly", "interval": 1, "by_weekday": []int{2}},
	}).expect(http.StatusBadRequest)

	series := e.weeklySeries(cal, labels[4].ID)
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/events/%d?scope=all", series.ID), map[string]any{
		"title": "Piscine", "label_id": labels[4].ID,
		"starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
		"recurrence": nil,
	}).expect(http.StatusBadRequest)

	// Neither refusal touched anything: the one-off is still a one-off and the series
	// still expands.
	list := e.listEvents("2026-08-01", "2026-08-31")
	if got, want := dates(list.Occurrences), []string{"2026-08-04", "2026-08-04", "2026-08-11", "2026-08-18", "2026-08-25"}; !equalStrings(got, want) {
		t.Fatalf("after both refusals = %v, want %v", got, want)
	}
}

func TestScopedDeleteAll(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)
	series := e.weeklySeries(cal, labels[4].ID)

	// Give it an override and a reminder first: deleting the series takes both.
	e.do(http.MethodPatch,
		fmt.Sprintf("/api/v1/events/%d?scope=this&date=2026-08-11", series.ID),
		map[string]any{
			"title": "Piscine (exception)", "label_id": labels[4].ID,
			"starts_at": "2026-08-11T14:30:00Z", "ends_at": "2026-08-11T15:15:00Z",
		}).expect(http.StatusOK)
	e.do(http.MethodPut, fmt.Sprintf("/api/v1/events/%d/reminders", series.ID), map[string]any{
		"reminders": []map[string]any{{"offset_minutes": 60}},
	}).expect(http.StatusOK)

	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/events/%d?scope=all", series.ID), nil).
		expect(http.StatusNoContent)

	list := e.listEvents("2026-08-01", "2026-12-31")
	if len(list.Occurrences) != 0 {
		t.Fatalf("occurrences after deleting the whole series = %v", dates(list.Occurrences))
	}
	e.get(fmt.Sprintf("/api/v1/events/%d?date=2026-08-04", series.ID)).expect(http.StatusNotFound)

	reminders, err := e.store.ListAllReminders(t.Context())
	if err != nil {
		t.Fatalf("list reminders: %v", err)
	}
	if len(reminders) != 0 {
		t.Errorf("reminders survived the series: %+v", reminders)
	}
}

func TestScopeIsValidated(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)
	series := e.weeklySeries(cal, labels[4].ID)

	body := map[string]any{
		"title": "Piscine", "label_id": labels[4].ID,
		"starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
	}
	// An unknown scope is refused before anything is touched.
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/events/%d?scope=sometimes&date=2026-08-11", series.ID), body).
		expect(http.StatusBadRequest)
	// A recurring event needs a scope at all.
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/events/%d", series.ID), body).
		expect(http.StatusBadRequest)
	// And "this" needs the date that identifies the occurrence.
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/events/%d?scope=this", series.ID), body).
		expect(http.StatusBadRequest)
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func TestSearchIsAccentInsensitiveAndScoped(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	e.createEvent(map[string]any{
		"calendar_id": cal.ID, "title": "Rentrée à l'École", "label_id": labels[1].ID,
		"all_day": true, "start_date": "2026-09-01", "end_date": "2026-09-01",
	})
	series := e.weeklySeries(cal, labels[4].ID)

	var found struct {
		Results []searchResult `json:"results"`
	}
	e.get("/api/v1/search?q=ecole").expect(http.StatusOK).decode(&found)
	if len(found.Results) != 1 || !strings.Contains(found.Results[0].Event.Title, "École") {
		t.Fatalf("search for \"ecole\" = %+v", found.Results)
	}
	if found.Results[0].NextOccurrence == nil || found.Results[0].NextOccurrence.String() != "2026-09-01" {
		t.Errorf("next_occurrence = %v", found.Results[0].NextOccurrence)
	}

	// A series appears once, with the next date it will happen.
	e.get("/api/v1/search?q=piscine").expect(http.StatusOK).decode(&found)
	if len(found.Results) != 1 || found.Results[0].Event.ID != series.ID {
		t.Fatalf("search for a series = %+v", found.Results)
	}
	if found.Results[0].NextOccurrence == nil || found.Results[0].NextOccurrence.String() != "2026-08-04" {
		t.Errorf("the series' next occurrence = %v", found.Results[0].NextOccurrence)
	}

	// A stranger searching the same words finds nothing.
	stranger := e.createUser("voisin@example.org", "Voisin")
	e.createCalendar(stranger, "Voisins")
	strangerClient := e.login(e.newClient(), stranger.Email)
	e.request(strangerClient, http.MethodGet, "/api/v1/search?q=piscine", nil).
		expect(http.StatusOK).decode(&found)
	if len(found.Results) != 0 {
		t.Errorf("a stranger's search returned %d results", len(found.Results))
	}
}

// TestSearchOffersADateAFinishedSeriesActuallyHappenedOn is the regression for #69.
//
// A result has to link somewhere, and for a series that has run out there is no next
// occurrence to link to. The browser used to fall back to the event's start date, on the
// assumption that a series begins on one of its own occurrences. It need not: DTStart is
// only the interval anchor, and the editor is happy to build a weekly rule that excludes
// the weekday it starts on. The date then reaches GET /events/{id} as ?date=, which
// answers 404 for a day the rule does not land on — so tapping the result said "Not
// found." about an event that is still there.
//
// This is the all-day shape deliberately. The sibling fault in #64 was a timezone one and
// could only reach timed events; this one has no instant in it anywhere, which is what
// makes it a different bug rather than more of the same.
func TestSearchOffersADateAFinishedSeriesActuallyHappenedOn(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	// Anchored on Monday 5 January, happening on Tuesdays, stopped on 27 January — all
	// of it behind the fake clock's July. The occurrences are the 6th, 13th, 20th and
	// 27th, and the anchor is not among them.
	series := e.createEvent(map[string]any{
		"calendar_id": cal.ID, "title": "Atelier poterie", "label_id": labels[1].ID,
		"all_day": true, "start_date": "2026-01-05", "end_date": "2026-01-05",
		"recurrence": map[string]any{
			"freq": "weekly", "interval": 1,
			"by_weekday": []int{int(time.Tuesday)}, "until": "2026-01-27",
		},
	})

	// The anchor is what the browser used to link to, and the API refuses it outright.
	// Asserted rather than assumed: if this ever starts answering 200 the test below is
	// no longer about anything.
	e.get(fmt.Sprintf("/api/v1/events/%d?date=2026-01-05", series.ID)).expect(http.StatusNotFound)

	var found struct {
		Results []searchResult `json:"results"`
	}
	e.get("/api/v1/search?q=poterie").expect(http.StatusOK).decode(&found)
	if len(found.Results) != 1 || found.Results[0].Event.ID != series.ID {
		t.Fatalf("search for a finished series = %+v", found.Results)
	}
	got := found.Results[0]
	if got.NextOccurrence != nil {
		t.Errorf("next_occurrence = %v, want null: the series ended in January", got.NextOccurrence)
	}
	if got.OccurrenceDate == nil {
		t.Fatal("occurrence_date is null, so the row has nowhere to link and the family sees an error")
	}
	if s := got.OccurrenceDate.String(); s != "2026-01-27" {
		t.Errorf("occurrence_date = %s, want 2026-01-27, the last Tuesday the series ran", s)
	}
	if got.OccurrenceDate.Equal(got.Event.StartDate) {
		t.Errorf("occurrence_date = %s is the anchor, which is not an occurrence of this rule", got.OccurrenceDate)
	}

	// The whole point: the date the search hands over opens the event.
	e.get(fmt.Sprintf("/api/v1/events/%d?date=%s", series.ID, got.OccurrenceDate)).expect(http.StatusOK)
}

// TestSearchLinksToTheNextOccurrenceWhileASeriesStillRuns keeps the common case honest:
// occurrence_date is not a "past events only" field, it is the day every row links to,
// and while there is a next occurrence it is that one.
func TestSearchLinksToTheNextOccurrenceWhileASeriesStillRuns(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	oneOff := e.createEvent(map[string]any{
		"calendar_id": cal.ID, "title": "Rentrée à l'École", "label_id": labels[1].ID,
		"all_day": true, "start_date": "2026-09-01", "end_date": "2026-09-01",
	})
	series := e.weeklySeries(cal, labels[4].ID)

	var found struct {
		Results []searchResult `json:"results"`
	}
	for _, tc := range []struct{ query, want string }{
		{"ecole", "2026-09-01"},
		{"piscine", "2026-08-04"},
	} {
		e.get("/api/v1/search?q=" + tc.query).expect(http.StatusOK).decode(&found)
		if len(found.Results) != 1 {
			t.Fatalf("search for %q = %+v", tc.query, found.Results)
		}
		got := found.Results[0]
		if got.OccurrenceDate == nil || got.OccurrenceDate.String() != tc.want {
			t.Errorf("search for %q: occurrence_date = %v, want %s", tc.query, got.OccurrenceDate, tc.want)
		}
		if got.NextOccurrence == nil || !got.NextOccurrence.Equal(*got.OccurrenceDate) {
			t.Errorf("search for %q: next_occurrence %v and occurrence_date %v disagree while the event still runs",
				tc.query, got.NextOccurrence, got.OccurrenceDate)
		}
	}

	// Both dates open, which is what the two fields promise.
	e.get(fmt.Sprintf("/api/v1/events/%d?date=2026-09-01", oneOff.ID)).expect(http.StatusOK)
	e.get(fmt.Sprintf("/api/v1/events/%d?date=2026-08-04", series.ID)).expect(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Avatars
// ---------------------------------------------------------------------------

func TestAvatarUploadAndServe(t *testing.T) {
	e := newEnv(t)
	user, _ := e.family()

	var img bytes.Buffer
	source := image.NewRGBA(image.Rect(0, 0, 300, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 300; x++ {
			source.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xFF})
		}
	}
	if err := png.Encode(&img, source); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	res := e.do(http.MethodPut, "/api/v1/me/avatar", nil, func(r *http.Request) {
		setRawBody(r, img.Bytes(), "image/png")
	}).expect(http.StatusOK)
	var uploaded struct {
		HasAvatar bool `json:"has_avatar"`
	}
	res.decode(&uploaded)
	if !uploaded.HasAvatar {
		t.Fatalf("upload response = %s", res.body)
	}

	// What comes back is a JPEG this server produced, not the bytes that were sent.
	served := e.get(fmt.Sprintf("/api/v1/users/%d/avatar", user.ID)).expect(http.StatusOK)
	if ct := served.header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("avatar Content-Type = %q", ct)
	}
	if ct := served.header.Get("X-Content-Type-Options"); ct != "nosniff" {
		t.Errorf("avatar X-Content-Type-Options = %q", ct)
	}
	if !bytes.HasPrefix(served.body, []byte{0xFF, 0xD8, 0xFF}) {
		t.Errorf("the served avatar is not JPEG: % x", served.body[:min(4, len(served.body))])
	}
	etag := served.header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the avatar")
	}
	e.do(http.MethodGet, fmt.Sprintf("/api/v1/users/%d/avatar", user.ID), nil, func(r *http.Request) {
		r.Header.Set("If-None-Match", etag)
	}).expect(http.StatusNotModified)

	// /me reports it, so the UI knows to draw the image rather than initials.
	var me meResponse
	e.get("/api/v1/me").expect(http.StatusOK).decode(&me)
	if !me.User.HasAvatar {
		t.Errorf("has_avatar = false after an upload")
	}

	e.do(http.MethodDelete, "/api/v1/me/avatar", nil).expect(http.StatusNoContent)
	e.get(fmt.Sprintf("/api/v1/users/%d/avatar", user.ID)).expect(http.StatusNotFound)
}

func TestAvatarRejectsOversizedAndUndecodableUploads(t *testing.T) {
	e := newEnv(t)
	e.family()

	// Just over the 1 MiB ceiling.
	oversized := bytes.Repeat([]byte{0x89}, 1<<20+512)
	res := e.do(http.MethodPut, "/api/v1/me/avatar", nil, func(r *http.Request) {
		setRawBody(r, oversized, "image/png")
	})
	res.expect(http.StatusBadRequest)
	if code := res.errorCode(); code != codeInvalid {
		t.Errorf("oversized avatar error code = %q, want %q", code, codeInvalid)
	}

	// Something that is not an image at all: imgproc's errors wrap domain.ErrInvalid,
	// so they map to 400 through the ordinary path.
	e.do(http.MethodPut, "/api/v1/me/avatar", nil, func(r *http.Request) {
		setRawBody(r, []byte("not an image"), "image/png")
	}).expect(http.StatusBadRequest)
}

// setRawBody replaces a request's JSON body with raw bytes, the way the client uploads
// an avatar.
func setRawBody(r *http.Request, data []byte, contentType string) {
	r.Body = readCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.Header.Set("Content-Type", contentType)
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

func readCloser(r *bytes.Reader) nopCloser { return nopCloser{r} }

// ---------------------------------------------------------------------------
// Preferences, push and activity
// ---------------------------------------------------------------------------

func TestPrefsAndPushHealth(t *testing.T) {
	// This test is about preferences and the repair banner, not about which hosts a
	// subscription may point at, so it names its fake push service in the allowlist
	// rather than borrowing a real vendor's hostname.
	e := newEnv(t, func(c *config.Config) { c.PushHosts = []string{"push.example.org"} })
	user, _ := e.family()

	var prefs prefsView
	e.get("/api/v1/prefs").expect(http.StatusOK).decode(&prefs)
	if !prefs.DigestEnabled || prefs.DigestTime != "07:30" {
		t.Fatalf("default prefs = %+v", prefs.NotificationPrefs)
	}
	if prefs.PushHealth.Devices != 0 || !prefs.PushHealth.Stale {
		t.Errorf("push_health with no devices = %+v", prefs.PushHealth)
	}

	e.do(http.MethodPatch, "/api/v1/prefs", map[string]any{
		"digest_time": "08:15", "email_digest": true, "digest_on_empty": true,
	}).expect(http.StatusOK).decode(&prefs)
	if prefs.DigestTime != "08:15" || !prefs.EmailDigest || !prefs.DigestOnEmpty {
		t.Fatalf("patched prefs = %+v", prefs.NotificationPrefs)
	}
	e.do(http.MethodPatch, "/api/v1/prefs", map[string]any{"digest_time": "8h15"}).
		expect(http.StatusBadRequest)

	// Subscribing counts as a confirmation, so the repair banner clears immediately.
	const endpoint = "https://push.example.org/subscription/abc123"
	e.post("/api/v1/push/subscription", map[string]string{
		"endpoint": endpoint, "p256dh": "BEl-key", "auth": "auth-secret", "ua_label": "iPhone de Maman",
	}).expect(http.StatusNoContent)

	e.get("/api/v1/prefs").expect(http.StatusOK).decode(&prefs)
	if prefs.PushHealth.Devices != 1 || prefs.PushHealth.Stale {
		t.Fatalf("push_health after subscribing = %+v", prefs.PushHealth)
	}

	// A fortnight without a confirmation is what "stale" means.
	e.clk.Advance(pushStaleAfter + time.Hour)
	e.get("/api/v1/prefs").expect(http.StatusOK).decode(&prefs)
	if !prefs.PushHealth.Stale {
		t.Errorf("push_health.stale = false after %v without a confirmation", pushStaleAfter)
	}
	e.post("/api/v1/push/confirm", map[string]string{"endpoint": endpoint}).expect(http.StatusNoContent)
	e.get("/api/v1/prefs").expect(http.StatusOK).decode(&prefs)
	if prefs.PushHealth.Stale {
		t.Errorf("a confirmation did not clear the stale flag")
	}

	// The test send queues something for every device.
	var sent struct {
		Sent int `json:"sent"`
	}
	e.post("/api/v1/push/test", nil).expect(http.StatusOK).decode(&sent)
	if sent.Sent != 1 {
		t.Errorf("push test reported %d devices, want 1", sent.Sent)
	}

	e.do(http.MethodDelete, "/api/v1/push/subscription", map[string]string{"endpoint": endpoint}).
		expect(http.StatusNoContent)
	subs, err := e.store.ListPushSubscriptions(t.Context(), user.ID)
	if err != nil || len(subs) != 0 {
		t.Errorf("subscriptions after unsubscribing = %d (%v)", len(subs), err)
	}

	// An endpoint that is not an https URL is refused: it is a capability, not a label.
	e.post("/api/v1/push/subscription", map[string]string{
		"endpoint": "javascript:alert(1)", "p256dh": "k", "auth": "a",
	}).expect(http.StatusBadRequest)
}

// A subscription endpoint is the only URL in this application that a member
// supplies and the server later dereferences, which is what makes it worth
// checking against a list of push services rather than merely parsing. Refusing at
// registration is deliberate: the alternative is a subscription that stores
// cleanly and then silently never delivers anything.
func TestPushSubscriptionHostMustBeAKnownPushService(t *testing.T) {
	e := newEnv(t) // no ALMANACK_PUSH_HOSTS, so the built-in list applies
	user, _ := e.family()

	const firefox = "https://updates.push.services.mozilla.com/wpush/v2/gAAAAA"
	e.post("/api/v1/push/subscription", map[string]string{
		"endpoint": firefox, "p256dh": "BEl-key", "auth": "auth-secret", "ua_label": "Firefox",
	}).expect(http.StatusNoContent)

	refused := []struct {
		name     string
		endpoint string
	}{
		{"a service on this machine", "https://127.0.0.1:9200/_search"},
		{"a machine on this network", "https://10.0.0.5/push"},
		{"the cloud metadata service", "https://169.254.169.254/latest/meta-data/"},
		{"somewhere else entirely", "https://attacker.example.org/push/abc"},
		// url.Parse reads everything before the @ as userinfo: this is a request to
		// 127.0.0.1 wearing a push service's name.
		{"userinfo wearing a push service's name", "https://fcm.googleapis.com@127.0.0.1/push"},
	}
	for _, tc := range refused {
		res := e.post("/api/v1/push/subscription", map[string]string{
			"endpoint": tc.endpoint, "p256dh": "BEl-key", "auth": "auth-secret",
		})
		if res.status != http.StatusBadRequest {
			t.Errorf("%s (%s): status = %d, want 400", tc.name, tc.endpoint, res.status)
			continue
		}
		if code := res.errorCode(); code != codeInvalid {
			t.Errorf("%s: error code = %q, want %q", tc.name, code, codeInvalid)
		}
	}

	subs, err := e.store.ListPushSubscriptions(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	if len(subs) != 1 || subs[0].Endpoint != firefox {
		t.Fatalf("stored subscriptions = %+v, want only the push service", subs)
	}

	// The way out, for a self-hosted push service or a browser that has started
	// handing out endpoints somewhere new. It is the operator's decision to make,
	// which is why it is a setting and not a special case in the code.
	open := newEnv(t, func(c *config.Config) { c.PushHosts = []string{"*"} })
	open.family()
	open.post("/api/v1/push/subscription", map[string]string{
		"endpoint": "https://push.example.org/subscription/abc", "p256dh": "BEl-key", "auth": "auth-secret",
	}).expect(http.StatusNoContent)
}

// Narrowing the allowlist must not strand a device that is already registered:
// confirming and unsubscribing are how the browser tidies up after itself, and
// neither one causes this server to dial anything.
func TestPushSubscriptionOutsideTheAllowlistCanStillBeRemoved(t *testing.T) {
	e := newEnv(t)
	user, _ := e.family()

	// Registered before the allowlist existed, which is what an upgrade looks like.
	const endpoint = "https://push.example.org/subscription/abc123"
	if err := e.store.UpsertPushSubscription(t.Context(), domain.PushSubscription{
		UserID: user.ID, Endpoint: endpoint, P256DH: "BEl-key", Auth: "auth-secret", UALabel: "iPhone",
	}); err != nil {
		t.Fatalf("upsert subscription: %v", err)
	}

	e.post("/api/v1/push/confirm", map[string]string{"endpoint": endpoint}).expect(http.StatusNoContent)
	e.do(http.MethodDelete, "/api/v1/push/subscription", map[string]string{"endpoint": endpoint}).
		expect(http.StatusNoContent)

	subs, err := e.store.ListPushSubscriptions(t.Context(), user.ID)
	if err != nil || len(subs) != 0 {
		t.Fatalf("subscriptions after unsubscribing = %+v (%v)", subs, err)
	}
}

func TestActivityFeed(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	created := e.createEvent(map[string]any{
		"calendar_id": cal.ID, "title": "Dentiste", "label_id": labels[0].ID,
		"starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
	})
	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/events/%d", created.ID), nil).
		expect(http.StatusNoContent)

	var feed struct {
		Activity []domain.Activity `json:"activity"`
	}
	e.get("/api/v1/activity?limit=10").expect(http.StatusOK).decode(&feed)
	if len(feed.Activity) != 2 {
		t.Fatalf("activity = %+v", feed.Activity)
	}
	if feed.Activity[0].Action != domain.ActionEventDeleted {
		t.Errorf("newest entry = %q, want the deletion", feed.Activity[0].Action)
	}
	// The title is denormalized, so it survives the event it describes.
	if feed.Activity[0].Title != "Dentiste" {
		t.Errorf("deleted entry title = %q", feed.Activity[0].Title)
	}
	e.get("/api/v1/activity?limit=0").expect(http.StatusBadRequest)
	e.get("/api/v1/activity?before_id=yesterday").expect(http.StatusBadRequest)

	// before= is the instant cursor this endpoint shipped with. It is still answered,
	// so a client written against the older documentation is not broken by the fix.
	var legacy struct {
		Activity []domain.Activity `json:"activity"`
	}
	after := e.clk.Now().Add(time.Hour).Format(time.RFC3339)
	e.get("/api/v1/activity?limit=10&before=" + after).expect(http.StatusOK).decode(&legacy)
	if len(legacy.Activity) != 2 {
		t.Errorf("the instant cursor returned %+v, want both entries", legacy.Activity)
	}
	e.get("/api/v1/activity?before=yesterday").expect(http.StatusBadRequest)
}

// TestActivityFeedPagesThroughASharedSecond: the clock is frozen for the whole test,
// so the creation and the deletion carry the same instant. Paging on the instant
// stepped over everything that shared the boundary second — the entry was gone from
// the feed for good. The cursor is an entry id, which is unique.
func TestActivityFeedPagesThroughASharedSecond(t *testing.T) {
	e := newEnv(t)
	_, cal := e.family()
	labels := e.labels(cal.ID)

	created := e.createEvent(map[string]any{
		"calendar_id": cal.ID, "title": "Dentiste", "label_id": labels[0].ID,
		"starts_at": "2026-08-04T14:30:00Z", "ends_at": "2026-08-04T15:15:00Z",
	})
	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/events/%d", created.ID), nil).
		expect(http.StatusNoContent)

	// One entry per page, exactly as the infinite scroll asks for them.
	var seen []domain.ActivityAction
	cursor := ""
	for range 3 {
		var page struct {
			Activity []domain.Activity `json:"activity"`
		}
		e.get("/api/v1/activity?limit=1" + cursor).expect(http.StatusOK).decode(&page)
		if len(page.Activity) == 0 {
			break
		}
		seen = append(seen, page.Activity[0].Action)
		cursor = fmt.Sprintf("&before_id=%d", page.Activity[len(page.Activity)-1].ID)
	}
	want := []domain.ActivityAction{domain.ActionEventDeleted, domain.ActionEventCreated}
	if !slices.Equal(seen, want) {
		t.Fatalf("paging one at a time saw %v, want %v", seen, want)
	}
}

// ---------------------------------------------------------------------------
// Calendars and labels
// ---------------------------------------------------------------------------

func TestCalendarAndLabelEditing(t *testing.T) {
	e := newEnv(t)
	_, existing := e.family()

	var created calendarView
	e.post("/api/v1/calendars", map[string]string{"name": "Travail", "color": "#27ae60"}).
		expect(http.StatusCreated).decode(&created)
	if created.Name != "Travail" || len(created.Labels) != domain.LabelsPerCalendar {
		t.Fatalf("created calendar = %+v", created)
	}
	if len(created.Members) != 1 {
		t.Errorf("the creator did not join their own calendar: %+v", created.Members)
	}

	// Labels are renamed and recoloured, never created or deleted.
	var label labelView
	e.do(http.MethodPatch,
		fmt.Sprintf("/api/v1/calendars/%d/labels/%d", created.ID, created.Labels[0].ID),
		map[string]any{"name": "Réunions", "color": "#123456", "position": 3}).
		expect(http.StatusOK).decode(&label)
	if label.Name != "Réunions" || label.Color != "#123456" || label.Position != 3 {
		t.Fatalf("patched label = %+v", label)
	}

	// A label from another calendar is not reachable through this one.
	e.do(http.MethodPatch,
		fmt.Sprintf("/api/v1/calendars/%d/labels/%d", created.ID, e.labels(existing.ID)[0].ID),
		map[string]any{"name": "Nope"}).expect(http.StatusNotFound)

	// Muting is per person and per calendar.
	var view calendarView
	e.do(http.MethodPatch, fmt.Sprintf("/api/v1/calendars/%d/membership", created.ID),
		map[string]any{"muted": true, "participating_only": true}).
		expect(http.StatusOK).decode(&view)
	if !view.Membership.Muted || !view.Membership.ParticipatingOnly {
		t.Fatalf("membership = %+v", view.Membership)
	}

	// A calendar with only its creator in it can be deleted.
	e.do(http.MethodDelete, fmt.Sprintf("/api/v1/calendars/%d", created.ID), nil).
		expect(http.StatusNoContent)

	for _, bad := range []map[string]string{
		{"name": "Sans couleur"},
		{"name": "", "color": "#ffffff"},
		{"name": "Mauvaise couleur", "color": "chartreuse"},
	} {
		e.post("/api/v1/calendars", bad).expect(http.StatusBadRequest)
	}
}

func TestUnknownJSONFieldIsRejected(t *testing.T) {
	e := newEnv(t)
	e.family()
	// "participant" instead of "participants" should fail now rather than silently do
	// nothing later.
	res := e.post("/api/v1/calendars", map[string]any{
		"name": "Travail", "color": "#27ae60", "colour": "#27ae60",
	})
	res.expect(http.StatusBadRequest)
	if code := res.errorCode(); code != codeInvalid {
		t.Errorf("error code = %q", code)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
