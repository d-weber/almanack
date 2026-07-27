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

	// A second day with something on it, beside today rather than always after it.
	evening := parentsEvening(today)
	if _, err := svc.Create(ctx, ids["Dad"], events.Input{
		CalendarID: calIDs[family], Title: "Parents' evening",
		StartsAt: at(evening, 18, 0), EndsAt: at(evening, 19, 0),
		Location: "Elm Park School", LabelID: label(family, 1),
		Participants: []int64{ids["Mum"], ids["Dad"]},
	}); err != nil {
		return err
	}

	// A multi-day all-day event: the case that breaks calendars which store all-day
	// events as midnight instants.
	holidayStart, holidayEnd := seasideHoliday(today)
	if _, err := svc.Create(ctx, ids["Mum"], events.Input{
		CalendarID: calIDs[family], Title: "Seaside holiday", AllDay: true,
		StartDate: holidayStart, EndDate: holidayEnd,
		Location: "Whitstable", LabelID: label(family, 2),
		Participants: []int64{ids["Mum"], ids["Dad"], ids["Leo"], ids["Gran"]},
	}); err != nil {
		return err
	}

	// A weekly series, with one occurrence moved and a later one cancelled.
	firstSwim, movedDate, cancelledDate := swimmingSeries(today)
	swimming, err := svc.Create(ctx, ids["Dad"], events.Input{
		CalendarID: calIDs[kids], Title: "Swimming",
		StartsAt: at(firstSwim, 17, 30), EndsAt: at(firstSwim, 18, 30),
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
	if _, err := svc.Update(ctx, ids["Dad"], swimming.ID, domain.ScopeThis, movedDate, events.Input{
		Title:    "Swimming (later than usual)",
		StartsAt: at(movedDate, 19, 0), EndsAt: at(movedDate, 20, 0),
		Location: "Leisure centre", LabelID: label(kids, 3),
		Participants: []int64{ids["Leo"], ids["Dad"]},
	}); err != nil {
		return fmt.Errorf("seed moved occurrence: %w", err)
	}
	if err := svc.Delete(ctx, ids["Dad"], swimming.ID, domain.ScopeThis, cancelledDate); err != nil {
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

	// A one-off evening out, kept close to today rather than pinned to the month the way
	// the two series are.
	saturday := cinemaNight(today)
	if _, err := svc.Create(ctx, ids["Dad"], events.Input{
		CalendarID: calIDs[parents], Title: "Cinema",
		StartsAt: at(saturday, 20, 30), EndsAt: at(saturday, 22, 45),
		LabelID: label(parents, 6), Participants: []int64{ids["Mum"], ids["Dad"]},
	}); err != nil {
		return err
	}

	// Every second Wednesday: interval anchoring, visible over a month.
	wednesday := guitarSeries(today)
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

// swimmingSeries returns the three dates the demo's weekly series is built from: the
// Tuesday it starts on, the occurrence moved to the evening, and the one cancelled.
//
// They are counted from the first Tuesday of the seeded month rather than from the
// seeded day, because the month view is the screen this demo opens on and all three
// are the point of the series. Its grid runs from the start of the week holding the
// 1st to the end of the week holding the last day, less a trailing row lying wholly
// outside the month (monthGrid, web/js/dates.js) — so every day of the month is on it
// and days either side of the month are on it only sometimes. Counting from the 1st
// bounds the three at the 7th, the 14th and the 28th, which the shortest February
// still has. Counting from the seeded day did not bound them at all: the next Tuesday
// after today is up to seven days out, so seeding on a Tuesday put the first
// occurrence a week ahead and off the end of a five-row grid, taking the moved one
// and the cancelled one with it, and the demo opened on a series with nothing to show
// for itself. The last-day-of-month rule below is anchored to the month for the same
// reason.
func swimmingSeries(today domain.Date) (start, moved, cancelled domain.Date) {
	// The first of any weekday falls on the 1st to the 7th, so this always exists.
	start, _ = domain.NthWeekdayOfMonth(today.Year, today.Month, time.Tuesday, 1)
	return start, start.AddDays(7), start.AddDays(21)
}

// guitarSeries returns the Wednesday the demo's fortnightly series starts on: the first
// of the seeded month, for the reason swimmingSeries gives above.
//
// A series is held to a harder standard than a single event, because one chip on a grid
// is not a series. What this one is in the seed to show is interval anchoring — that
// "every second Wednesday" skips the weeks in between — and that needs two occurrences on
// the month the app opens on. From the first Wednesday they fall on the 7th at the latest
// and the 21st at the latest, both days of the month and so both on its grid. From the
// next Wednesday after today, the first could already be in the following month, which
// near the end of a month is what it was.
func guitarSeries(today domain.Date) domain.Date {
	first, _ := domain.NthWeekdayOfMonth(today.Year, today.Month, time.Wednesday, 1)
	return first
}

// cinemaNight returns the day the demo's one-off evening out is on: the coming Saturday,
// counting today, and never the one belonging to the next month.
//
// It is deliberately not pinned to a fixed Saturday of the month the way the two series
// are. A one-off is supposed to sit near the day the demo was made — "what is coming up"
// is half of what the opening screen is for — and a cinema outing pinned to the first
// Saturday would be three weeks stale by the end of a month. What it does have to avoid
// is the far end: nextWeekday() reads "today is already Saturday" as next week, so this
// was up to seven days out, and seven days out can be past the end of a five-row grid
// (see swimmingSeries). Counting today is both the realistic answer and the shorter one,
// and a Saturday that has landed in the next month steps back a week rather than being
// dropped — on the last days of a month the outing is then a few days behind rather than
// a few days ahead, which is a thing calendars hold, unlike an event nobody can see.
//
// The step back is always inside the seeded month: the coming Saturday only leaves the
// month when today is within six days of its end, and a week before that is the 15th at
// the earliest.
func cinemaNight(today domain.Date) domain.Date {
	saturday := today
	if saturday.Weekday() != time.Saturday {
		saturday = nextWeekday(today, time.Saturday)
	}
	if saturday.Year != today.Year || saturday.Month != today.Month {
		saturday = saturday.AddDays(-7)
	}
	return saturday
}

// seasideHoliday returns the week the demo's multi-day all-day event covers: the second
// Saturday of the seeded month, and the six days after it.
//
// A span is held to both of its ends, which is what makes this one different from the
// three fixed before it. A bar is on the screen the app opens on only if the day it
// begins and the day it finishes are both days of the seeded month, and seven days need
// seven consecutive ones: a week fits only if it starts on or before the 22nd of a
// 28-day February, the 25th of a 31-day month. Counted from the seeded day it respected
// neither end — it ran from today + 10 to today + 16 — so on a third of the days the seed
// could be run the whole week was in the following month, and the demo opened on no
// holiday at all, which is the one thing the seeder's own summary promises of that
// screen.
//
// It is pinned to the month rather than kept near today, which is the opposite of the
// choice cinemaNight makes, because a week-long span leaves no room for the choice.
// "Ahead of today" exists only while today is more than six days from the end of the
// month; past that the nearest week that fits is whichever one ends on the last day, so
// clamping the old anchor rather than replacing it would have jammed the holiday against
// the month's edge on more than half of the days the seed can be run — a placement that
// reads as an artefact of seeding rather than as a holiday anyone booked. What this event
// is in the seed for is the awkward rendering case rather than what is coming up: three
// other seeded events sit within a week of today, and a rendering case is served by being
// plainly and wholly on the screen, not by being recent.
//
// The second Saturday falls on the 8th to the 14th, so the week ends on the 14th to the
// 20th — clear of both ends of every month, February included. Saturday because that is
// the day a week by the sea starts on, and because seven days from one are drawn in two
// week rows for both of the week starts the settings screen offers: a seven-day span sits
// in a single row exactly when it begins on the reader's first day of the week, and one
// row does not show the continuation the bar layout in web/js/views/month.js exists for.
func seasideHoliday(today domain.Date) (start, end domain.Date) {
	// The second of any weekday falls on the 8th to the 14th, so this always exists.
	start, _ = domain.NthWeekdayOfMonth(today.Year, today.Month, time.Saturday, 2)
	return start, start.AddDays(6)
}

// parentsEvening returns the day the demo's school evening is on: tomorrow, or yesterday
// when tomorrow belongs to the next month.
//
// It is the last of the seeded dates that was counted blindly from the day the seed ran,
// and the mildest case of it: one day out leaves the month only on the last day of one,
// and leaves the grid only when that day is also the last day of a week — three days in
// 2026 and 2027 for a reader whose week begins on a Monday, three others for a reader
// whose week begins on a Sunday. Mild is still invisible on the days it happens, and the
// remedy is the one cinemaNight already uses: step back rather than cross. That keeps the
// appointment beside today, which is the whole of what it is doing in the seed, and
// inside the month, which is the set of days every reader's grid holds. The day before
// the last day of a month is the 27th at the earliest, so the step back never leaves it.
func parentsEvening(today domain.Date) domain.Date {
	tomorrow := today.AddDays(1)
	if tomorrow.Year != today.Year || tomorrow.Month != today.Month {
		return today.AddDays(-1)
	}
	return tomorrow
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
