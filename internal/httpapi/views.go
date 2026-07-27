package httpapi

import (
	"context"
	"fmt"
	"time"

	"almanack/internal/domain"
)

// The wire shapes of docs/api.md. They are written out here rather than
// reusing the domain structs directly wherever the contract names a different field
// (Occurrence.event_id, for instance) or hides one: the document is normative, and the
// frontend is built against it in parallel.

type labelView struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Position int    `json:"position"`
}

type memberView struct {
	UserID      int64  `json:"user_id"`
	DisplayName string `json:"display_name"`
	Color       string `json:"color"`
	HasAvatar   bool   `json:"has_avatar"`
}

type membershipView struct {
	Muted             bool `json:"muted"`
	ParticipatingOnly bool `json:"participating_only"`
}

type calendarView struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	Color      string         `json:"color"`
	CreatorID  int64          `json:"creator_id"`
	HasImage   bool           `json:"has_image"`
	Membership membershipView `json:"membership"`
	Members    []memberView   `json:"members"`
	Labels     []labelView    `json:"labels"`
}

// occurrenceView is one materialized instance. label_name, label_color and
// calendar_color are denormalized onto it because the month grid draws thousands of
// these and must not have to join anything to know what colour they are.
type occurrenceView struct {
	EventID        int64       `json:"event_id"`
	CalendarID     int64       `json:"calendar_id"`
	CalendarColor  string      `json:"calendar_color"`
	Title          string      `json:"title"`
	AllDay         bool        `json:"all_day"`
	StartsAt       *time.Time  `json:"starts_at"`
	EndsAt         *time.Time  `json:"ends_at"`
	StartDate      domain.Date `json:"start_date"`
	EndDate        domain.Date `json:"end_date"`
	OccurrenceDate domain.Date `json:"occurrence_date"`
	Location       string      `json:"location"`
	URL            string      `json:"url"`
	Notes          string      `json:"notes"`
	LabelID        int64       `json:"label_id"`
	LabelColor     string      `json:"label_color"`
	LabelName      string      `json:"label_name"`
	Participants   []int64     `json:"participants"`
	RecurrenceID   *int64      `json:"recurrence_id"`
	SeriesEventID  *int64      `json:"series_event_id"`
	IsOverride     bool        `json:"is_override"`
	CreatedBy      int64       `json:"created_by"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

type holidayView struct {
	Date domain.Date `json:"date"`
	Name string      `json:"name"`
}

// decorator resolves the denormalized colours for a set of calendars. It is built once
// per request so that a month of occurrences costs two queries, not two per event.
type decorator struct {
	calendars map[int64]domain.Calendar
	labels    map[int64]domain.Label
}

func (s *Server) newDecorator(ctx context.Context, cals []domain.Calendar) (*decorator, error) {
	d := &decorator{
		calendars: make(map[int64]domain.Calendar, len(cals)),
		labels:    map[int64]domain.Label{},
	}
	for _, c := range cals {
		d.calendars[c.ID] = c
		labels, err := s.store.ListLabels(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, l := range labels {
			d.labels[l.ID] = l
		}
	}
	return d, nil
}

func (d *decorator) occurrence(occ domain.Occurrence) occurrenceView {
	v := occurrenceView{
		EventID:        occ.ID,
		CalendarID:     occ.CalendarID,
		CalendarColor:  d.calendars[occ.CalendarID].Color,
		Title:          occ.Title,
		AllDay:         occ.AllDay,
		StartDate:      occ.StartDate,
		EndDate:        occ.EndDate,
		OccurrenceDate: occ.OccurrenceDate,
		Location:       occ.Location,
		URL:            occ.URL,
		Notes:          occ.Notes,
		LabelID:        occ.LabelID,
		Participants:   occ.Participants,
		RecurrenceID:   occ.RecurrenceID,
		SeriesEventID:  occ.SeriesEventID,
		IsOverride:     occ.IsOverride,
		CreatedBy:      occ.CreatedBy,
		UpdatedAt:      occ.UpdatedAt,
	}
	if !occ.AllDay {
		starts, ends := occ.StartsAt, occ.EndsAt
		v.StartsAt, v.EndsAt = &starts, &ends
	}
	if l, ok := d.labels[occ.LabelID]; ok {
		v.LabelColor, v.LabelName = l.Color, l.Name
	}
	if v.Participants == nil {
		v.Participants = []int64{}
	}
	// An override that belongs to a series reports the series it came from; the
	// contract's series_event_id is null for a plain event.
	if v.SeriesEventID == nil && occ.RecurrenceID != nil {
		id := occ.ID
		v.SeriesEventID = &id
	}
	return v
}

// calendarViews assembles what /me returns per calendar: the calendar, the caller's own
// membership flags, every member with what it takes to draw their avatar, and the ten
// labels.
func (s *Server) calendarViews(ctx context.Context, userID int64) ([]calendarView, error) {
	cals, err := s.store.ListCalendarsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]domain.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}

	out := make([]calendarView, 0, len(cals))
	for _, c := range cals {
		view, err := s.calendarView(ctx, c, userID, byID)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, nil
}

func (s *Server) calendarView(ctx context.Context, c domain.Calendar, userID int64, byID map[int64]domain.User) (calendarView, error) {
	if byID == nil {
		users, err := s.store.ListUsers(ctx)
		if err != nil {
			return calendarView{}, err
		}
		byID = make(map[int64]domain.User, len(users))
		for _, u := range users {
			byID[u.ID] = u
		}
	}

	members, err := s.store.ListMembers(ctx, c.ID)
	if err != nil {
		return calendarView{}, err
	}
	labels, err := s.store.ListLabels(ctx, c.ID)
	if err != nil {
		return calendarView{}, err
	}

	view := calendarView{
		ID:        c.ID,
		Name:      c.Name,
		Color:     c.Color,
		CreatorID: c.CreatorID,
		HasImage:  c.HasImage,
		Members:   make([]memberView, 0, len(members)),
		Labels:    make([]labelView, 0, len(labels)),
	}
	for _, m := range members {
		if m.UserID == userID {
			view.Membership = membershipView{Muted: m.Muted, ParticipatingOnly: m.ParticipatingOnly}
		}
		u, ok := byID[m.UserID]
		if !ok {
			continue
		}
		view.Members = append(view.Members, memberView{
			UserID:      u.ID,
			DisplayName: u.DisplayName,
			Color:       u.Color,
			HasAvatar:   u.HasAvatar,
		})
	}
	for _, l := range labels {
		view.Labels = append(view.Labels, labelView{ID: l.ID, Name: l.Name, Color: l.Color, Position: l.Position})
	}
	return view, nil
}

// pushHealth is the client's signal to show the "repair notifications" banner. A
// subscription that no device has confirmed in a fortnight is the signature of a
// silently revoked iOS subscription: the push service keeps returning success for those,
// so nothing else can detect them.
type pushHealth struct {
	Devices         int       `json:"devices"`
	Stale           bool      `json:"stale"`
	LastConfirmedAt time.Time `json:"last_confirmed_at,omitzero"`
}

// pushStaleAfter matches the client's repair-banner threshold.
const pushStaleAfter = 14 * 24 * time.Hour

type prefsView struct {
	domain.NotificationPrefs
	PushHealth pushHealth `json:"push_health"`
}

func (s *Server) prefsView(ctx context.Context, userID int64) (prefsView, error) {
	prefs, err := s.store.Prefs(ctx, userID)
	if err != nil {
		return prefsView{}, err
	}
	subs, err := s.store.ListPushSubscriptions(ctx, userID)
	if err != nil {
		return prefsView{}, err
	}
	health := pushHealth{Devices: len(subs)}
	for _, sub := range subs {
		if sub.LastConfirmedAt.After(health.LastConfirmedAt) {
			health.LastConfirmedAt = sub.LastConfirmedAt
		}
	}
	health.Stale = health.LastConfirmedAt.IsZero() ||
		s.clock.Now().Sub(health.LastConfirmedAt) > pushStaleAfter
	return prefsView{NotificationPrefs: prefs, PushHealth: health}, nil
}

// requireMember is the one authorization rule this application has. Everything else is
// deliberately flat: any member of a calendar may edit anything in it.
func (s *Server) requireMember(ctx context.Context, calendarID, userID int64) error {
	member, err := s.store.IsMember(ctx, calendarID, userID)
	if err != nil {
		return err
	}
	if !member {
		return fmt.Errorf("%w: not a member of calendar %d", domain.ErrForbidden, calendarID)
	}
	return nil
}
