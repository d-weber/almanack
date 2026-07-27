package main

import (
	"context"
	"fmt"
	"time"

	"almanack/internal/auth"
	"almanack/internal/clock"
	"almanack/internal/config"
	"almanack/internal/domain"
	"almanack/internal/events"
	"almanack/internal/store"
)

// The seeder builds a plausible family week so that `make seed && make dev` lands on
// a calendar with something in it. It deliberately includes the awkward cases —
// a multi-day all-day holiday, a weekly series with one occurrence moved and another
// cancelled, a last-day-of-month rule, a birthday that recurs yearly — because those
// are the ones worth looking at when you change rendering or expansion code.

const seedPassword = "password"

type seedUser struct {
	name  string
	email string
	color string
	lang  domain.Language
	admin bool
}

func runSeed(ctx context.Context, cfg config.Config, force bool) error {
	st, err := store.Open(cfg.DataPath, cfg.FamilyTZ, clock.Real{})
	if err != nil {
		return err
	}
	defer st.Close()

	existing, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	if existing > 0 && !force {
		return fmt.Errorf("database already has %d user(s); refusing to seed over it (use --force, or `make seed` which starts from an empty file)", existing)
	}

	svc := events.New(st, cfg.FamilyTZ, clock.Real{})
	loc := cfg.FamilyTZ
	today := domain.DateIn(time.Now(), loc)

	// --- people -------------------------------------------------------------
	// Gran reads the app in French. One person on the other language is the point:
	// the interface is per-user, and a family that does not share one is the case
	// this project was built for.
	people := []seedUser{
		{name: "Mum", email: "mum@example.org", color: "#c0392b", lang: domain.LangEN, admin: true},
		{name: "Dad", email: "dad@example.org", color: "#2980b9", lang: domain.LangEN},
		{name: "Leo", email: "leo@example.org", color: "#27ae60", lang: domain.LangEN},
		{name: "Gran", email: "gran@example.org", color: "#8e44ad", lang: domain.LangFR},
	}
	hash, err := auth.HashPassword(seedPassword)
	if err != nil {
		return err
	}
	ids := map[string]int64{}
	for _, p := range people {
		u, err := st.CreateUser(ctx, domain.User{
			Email:       p.email,
			DisplayName: p.name,
			Color:       p.color,
			Lang:        p.lang,
			WeekStart:   time.Monday,
			TimeFormat:  "24h",
			IsAdmin:     p.admin,
			CreatedAt:   time.Now().UTC(),
		}, hash)
		if err != nil {
			return fmt.Errorf("create %s: %w", p.name, err)
		}
		ids[p.name] = u.ID
		if err := st.UpdatePrefs(ctx, domain.NotificationPrefs{
			UserID: u.ID, DigestEnabled: true, DigestTime: "07:30",
			SummaryTime: "20:00", EmailReminders: true, ActivityPush: true,
		}); err != nil {
			return err
		}
	}

	// --- calendars ----------------------------------------------------------
	type calSpec struct {
		name    string
		color   string
		creator string
		members []string
	}
	calIDs := map[string]int64{}
	labels := map[string][]domain.Label{}
	for _, c := range []calSpec{
		{"Family", "#3b7ddd", "Mum", []string{"Dad", "Leo", "Gran"}},
		{"Parents", "#7d3bdd", "Mum", []string{"Dad"}},
		{"Kids' activities", "#2fa84f", "Dad", []string{"Mum", "Leo"}},
	} {
		cal, err := st.CreateCalendar(ctx, domain.Calendar{
			Name: c.name, Color: c.color, CreatorID: ids[c.creator], CreatedAt: time.Now().UTC(),
		})
		if err != nil {
			return fmt.Errorf("create calendar %s: %w", c.name, err)
		}
		for _, m := range c.members {
			if err := st.AddMember(ctx, cal.ID, ids[m]); err != nil {
				return err
			}
		}
		ls, err := st.ListLabels(ctx, cal.ID)
		if err != nil {
			return err
		}
		calIDs[c.name] = cal.ID
		labels[c.name] = ls
	}

	label := func(cal string, i int) int64 {
		ls := labels[cal]
		if len(ls) == 0 {
			return 0
		}
		return ls[i%len(ls)].ID
	}
	at := func(d domain.Date, hour, min int) time.Time {
		return d.At(hour, min, loc).UTC()
	}

	// --- events -------------------------------------------------------------
	family, parents, kids := "Family", "Parents", "Kids' activities"

	// A timed appointment today, concerning two people.
	dentist, err := svc.Create(ctx, ids["Mum"], events.Input{
		CalendarID: calIDs[family], Title: "Leo's dentist",
		StartsAt: at(today, 16, 30), EndsAt: at(today, 17, 15),
		Location: "Bridge Street Dental", LabelID: label(family, 0),
		Participants: []int64{ids["Mum"], ids["Leo"]},
	})
	if err != nil {
		return fmt.Errorf("seed dentist: %w", err)
	}
	if err := st.ReplaceReminders(ctx, &dentist.ID, nil, ids["Mum"], []domain.Reminder{
		{EventID: &dentist.ID, UserID: ids["Mum"], OffsetMinutes: ptr(1440)},
	}); err != nil {
		return err
	}

	if _, err := svc.Create(ctx, ids["Dad"], events.Input{
		CalendarID: calIDs[family], Title: "Parents' evening",
		StartsAt: at(today.AddDays(1), 18, 0), EndsAt: at(today.AddDays(1), 19, 0),
		Location: "Elm Park School", LabelID: label(family, 1),
		Participants: []int64{ids["Mum"], ids["Dad"]},
	}); err != nil {
		return err
	}

	// A multi-day all-day event: the case that breaks calendars which store all-day
	// events as midnight instants.
	if _, err := svc.Create(ctx, ids["Mum"], events.Input{
		CalendarID: calIDs[family], Title: "Seaside holiday", AllDay: true,
		StartDate: today.AddDays(10), EndDate: today.AddDays(16),
		Location: "Whitstable", LabelID: label(family, 2),
		Participants: []int64{ids["Mum"], ids["Dad"], ids["Leo"], ids["Gran"]},
	}); err != nil {
		return err
	}

	// A weekly series, with one occurrence moved and a later one cancelled.
	nextTuesday := nextWeekday(today, time.Tuesday)
	swimming, err := svc.Create(ctx, ids["Dad"], events.Input{
		CalendarID: calIDs[kids], Title: "Swimming",
		StartsAt: at(nextTuesday, 17, 30), EndsAt: at(nextTuesday, 18, 30),
		Location: "Leisure centre", LabelID: label(kids, 3),
		Participants: []int64{ids["Leo"], ids["Dad"]},
		Recurrence:   &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday}},
	})
	if err != nil {
		return fmt.Errorf("seed swimming: %w", err)
	}
	if swimming.RecurrenceID != nil {
		if err := st.ReplaceReminders(ctx, nil, swimming.RecurrenceID, ids["Dad"], []domain.Reminder{
			{RecurrenceID: swimming.RecurrenceID, UserID: ids["Dad"], OffsetMinutes: ptr(30)},
		}); err != nil {
			return err
		}
	}
	movedDate := nextTuesday.AddDays(7)
	if _, err := svc.Update(ctx, ids["Dad"], swimming.ID, domain.ScopeThis, movedDate, events.Input{
		Title:    "Swimming (later than usual)",
		StartsAt: at(movedDate, 19, 0), EndsAt: at(movedDate, 20, 0),
		Location: "Leisure centre", LabelID: label(kids, 3),
		Participants: []int64{ids["Leo"], ids["Dad"]},
	}); err != nil {
		return fmt.Errorf("seed moved occurrence: %w", err)
	}
	if err := svc.Delete(ctx, ids["Dad"], swimming.ID, domain.ScopeThis, nextTuesday.AddDays(21)); err != nil {
		return fmt.Errorf("seed cancelled occurrence: %w", err)
	}

	// A yearly birthday: the Feb-29 clamp lives behind this shape.
	birthday := domain.NewDate(today.Year, time.March, 12)
	if _, err := svc.Create(ctx, ids["Mum"], events.Input{
		CalendarID: calIDs[family], Title: "Leo's birthday", AllDay: true,
		StartDate: birthday, EndDate: birthday, LabelID: label(family, 4),
		Participants: []int64{ids["Leo"]},
		Recurrence:   &domain.Recurrence{Freq: domain.FreqYearly, Interval: 1},
	}); err != nil {
		return err
	}

	// Last day of the month, every month — the rule a naive "same day number"
	// implementation gets wrong in February.
	lastDay := domain.LastDayOfMonth(today.Year, today.Month)
	if _, err := svc.Create(ctx, ids["Mum"], events.Input{
		CalendarID: calIDs[parents], Title: "Household accounts", AllDay: true,
		StartDate: lastDay, EndDate: lastDay, LabelID: label(parents, 5),
		Participants: []int64{ids["Mum"], ids["Dad"]},
		Recurrence:   &domain.Recurrence{Freq: domain.FreqMonthly, Interval: 1, MonthLastDay: true},
	}); err != nil {
		return err
	}

	saturday := nextWeekday(today, time.Saturday)
	if _, err := svc.Create(ctx, ids["Dad"], events.Input{
		CalendarID: calIDs[parents], Title: "Cinema",
		StartsAt: at(saturday, 20, 30), EndsAt: at(saturday, 22, 45),
		LabelID: label(parents, 6), Participants: []int64{ids["Mum"], ids["Dad"]},
	}); err != nil {
		return err
	}

	// Every second Wednesday: interval anchoring, visible over a month.
	wednesday := nextWeekday(today, time.Wednesday)
	if _, err := svc.Create(ctx, ids["Mum"], events.Input{
		CalendarID: calIDs[kids], Title: "Guitar lesson",
		StartsAt: at(wednesday, 14, 0), EndsAt: at(wednesday, 15, 0),
		LabelID: label(kids, 7), Participants: []int64{ids["Leo"]},
		Recurrence: &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 2, ByWeekday: []time.Weekday{time.Wednesday}},
	}); err != nil {
		return err
	}

	fmt.Printf(`Seeded a demo family in %s

  Sign in with any of these, password %q:
    mum@example.org    (Mum, admin)
    dad@example.org    (Dad)
    leo@example.org    (Leo)
    gran@example.org   (Gran, French UI)

  Calendars: Family, Parents, Kids' activities
  Includes a weekly series with one occurrence moved and one cancelled,
  a multi-day holiday, a yearly birthday and a last-day-of-month rule.
`, cfg.DataPath, seedPassword)
	return nil
}

// nextWeekday returns the next occurrence of wd strictly after d.
func nextWeekday(d domain.Date, wd time.Weekday) domain.Date {
	delta := (int(wd) - int(d.Weekday()) + 7) % 7
	if delta == 0 {
		delta = 7
	}
	return d.AddDays(delta)
}

func ptr[T any](v T) *T { return &v }
