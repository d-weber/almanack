package events

import (
	"context"
	"testing"
	"time"

	"almanack/internal/domain"
)

// The tests in this file all start from the same place: a weekly series with one
// occurrence already edited. That edit produces a standalone copy of the event, and
// the API hands the family that copy's id — so the *second* thing anyone does to that
// occurrence arrives addressed to the copy rather than to the series. Everything below
// is what has to keep working when it does.

type editedOccurrence struct {
	f      *fixture
	series domain.Event
	copy   domain.Event
	date   domain.Date
}

// editedFixture builds "Piscine" every Tuesday from 7 April 2026, then moves the
// 14 April occurrence to Thursday the 16th at 19:00. Moving it to another day matters:
// it makes the copy's own start date differ from the occurrence date that identifies
// it, so a test cannot pass by confusing the two.
func editedFixture(t *testing.T) editedOccurrence {
	t.Helper()
	f := newFixture(t)
	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	date := domain.MustParseDate("2026-04-14")
	copyEvent, err := f.svc.Update(context.Background(), f.maman, series.ID, domain.ScopeThis, date, Input{
		Title: "Piscine (déplacée)", StartsAt: f.at("2026-04-16", 19, 0), EndsAt: f.at("2026-04-16", 20, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	})
	if err != nil {
		t.Fatalf("edit one occurrence: %v", err)
	}
	if copyEvent.ID == series.ID {
		t.Fatalf("editing one occurrence returned the series template itself")
	}
	return editedOccurrence{f: f, series: series, copy: copyEvent, date: date}
}

// TestDeletingAnEditedOccurrenceKeepsItDeleted is the headline symptom of issue #1.
// Deleting the copy row cascades the override away, which restores the occurrence to
// its original series time — so "delete this occurrence" made it reappear.
func TestDeletingAnEditedOccurrenceKeepsItDeleted(t *testing.T) {
	e := editedFixture(t)

	if err := e.f.svc.Delete(context.Background(), e.f.maman, e.copy.ID, domain.ScopeThis, e.date); err != nil {
		t.Fatalf("delete the edited occurrence: %v", err)
	}

	occ := e.f.occurrences(t, "2026-04-01", "2026-04-30")
	for _, o := range occ {
		if o.OccurrenceDate.Equal(e.date) {
			t.Errorf("the deleted occurrence came back as %q on %s", o.Title, o.StartsAt)
		}
	}
	if len(occ) != 3 {
		t.Errorf("got %d occurrences, want 3 (April has four Tuesdays, one deleted)", len(occ))
	}
	if _, err := e.f.st.EventByID(context.Background(), e.copy.ID); err == nil {
		t.Error("the edited copy outlived the occurrence it stood for")
	}
}

// TestReEditingAnEditedOccurrenceStaysAnOccurrence: the second edit must land on the
// same override rather than turning the copy into a free-floating event.
func TestReEditingAnEditedOccurrenceStaysAnOccurrence(t *testing.T) {
	e := editedFixture(t)
	ctx := context.Background()

	again, err := e.f.svc.Update(ctx, e.f.maman, e.copy.ID, domain.ScopeThis, e.date, Input{
		Title: "Piscine (re-déplacée)", StartsAt: e.f.at("2026-04-17", 18, 0), EndsAt: e.f.at("2026-04-17", 19, 0),
		LabelID: e.f.labels[0].ID, Participants: []int64{e.f.maman},
	})
	if err != nil {
		t.Fatalf("re-edit the occurrence: %v", err)
	}
	if again.ID != e.copy.ID {
		t.Errorf("the second edit created event %d instead of updating the copy %d", again.ID, e.copy.ID)
	}

	occ := e.f.occurrences(t, "2026-04-01", "2026-04-30")
	if len(occ) != 4 {
		t.Fatalf("got %d occurrences, want the series' four April Tuesdays", len(occ))
	}
	var edited *domain.Occurrence
	for i := range occ {
		if occ[i].OccurrenceDate.Equal(e.date) {
			edited = &occ[i]
		}
	}
	if edited == nil {
		t.Fatalf("the re-edited occurrence vanished: %v", occ)
	}
	if edited.Title != "Piscine (re-déplacée)" {
		t.Errorf("occurrence title = %q, want the second edit", edited.Title)
	}
	if !edited.IsOverride || edited.SeriesEventID == nil || *edited.SeriesEventID != e.series.ID {
		t.Errorf("the re-edited occurrence lost its series: is_override=%v series=%v",
			edited.IsOverride, edited.SeriesEventID)
	}
}

// TestReadingAnEditedOccurrenceByItsCopyIDFindsTheSeries is what makes the second edit
// possible at all: the client decides whether to ask "this / this and following / the
// whole series" from what this read reports.
func TestReadingAnEditedOccurrenceByItsCopyIDFindsTheSeries(t *testing.T) {
	e := editedFixture(t)

	occ, err := e.f.svc.Occurrence(context.Background(), e.copy.ID, e.date)
	if err != nil {
		t.Fatalf("read the edited occurrence: %v", err)
	}
	if !occ.OccurrenceDate.Equal(e.date) {
		t.Errorf("occurrence_date = %s, want the date in the series (%s), not the copy's own start",
			occ.OccurrenceDate, e.date)
	}
	if !occ.IsOverride {
		t.Error("is_override = false; the client will treat an edited occurrence as a plain event")
	}
	if occ.SeriesEventID == nil || *occ.SeriesEventID != e.series.ID {
		t.Errorf("series_event_id = %v, want the series template %d", occ.SeriesEventID, e.series.ID)
	}

	// The date is optional here: the copy exists for exactly one occurrence, so its
	// own override row says which. A client that has lost the date still gets a
	// usable answer rather than a plain-event one.
	bare, err := e.f.svc.Occurrence(context.Background(), e.copy.ID, domain.Date{})
	if err != nil {
		t.Fatalf("read the edited occurrence without a date: %v", err)
	}
	if !bare.OccurrenceDate.Equal(e.date) || !bare.IsOverride {
		t.Errorf("without a date: occurrence_date=%s is_override=%v", bare.OccurrenceDate, bare.IsOverride)
	}
}

// TestEditingTheWholeSeriesFromAnEditedOccurrence: once the client can see the series
// again it will offer the scope question, and "all" must reach the template rather than
// renaming the one copy the request happened to be addressed to.
func TestEditingTheWholeSeriesFromAnEditedOccurrence(t *testing.T) {
	e := editedFixture(t)
	ctx := context.Background()

	if _, err := e.f.svc.Update(ctx, e.f.maman, e.copy.ID, domain.ScopeAll, e.date, Input{
		Title: "Natation", StartsAt: e.f.at("2026-04-07", 17, 30), EndsAt: e.f.at("2026-04-07", 18, 30),
		LabelID: e.f.labels[0].ID, Participants: []int64{e.f.maman},
		Recurrence: &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday}},
	}); err != nil {
		t.Fatalf("edit the whole series: %v", err)
	}

	for _, o := range e.f.occurrences(t, "2026-04-01", "2026-04-30") {
		if o.OccurrenceDate.Equal(e.date) {
			// Deliberate single-occurrence edits survive a series edit; that rule is
			// tested on its own, and it must not change just because the request came
			// in addressed to the copy.
			if o.Title != "Piscine (déplacée)" {
				t.Errorf("the edited occurrence was overwritten by the series edit: %q", o.Title)
			}
			continue
		}
		if o.Title != "Natation" {
			t.Errorf("%s = %q, want the new series title", o.OccurrenceDate, o.Title)
		}
	}
}

// TestDeletingTheWholeSeriesFromAnEditedOccurrence is the same reachability problem on
// the delete path: "delete every occurrence" must not quietly delete only the one copy.
func TestDeletingTheWholeSeriesFromAnEditedOccurrence(t *testing.T) {
	e := editedFixture(t)
	ctx := context.Background()

	if err := e.f.svc.Delete(ctx, e.f.maman, e.copy.ID, domain.ScopeAll, e.date); err != nil {
		t.Fatalf("delete the whole series: %v", err)
	}
	if occ := e.f.occurrences(t, "2026-04-01", "2026-06-30"); len(occ) != 0 {
		t.Errorf("%d occurrences survived deleting the series: %v", len(occ), occ)
	}
	if _, err := e.f.st.EventByID(ctx, e.series.ID); err == nil {
		t.Error("the series template survived a delete-all addressed to one of its overrides")
	}
}

// TestEditingAnEditedOccurrencePrunesItsQueuedReminder: notifications are queued
// against the *series* event id, so pruning by the copy's id leaves the family being
// reminded at the time the occurrence used to be at.
func TestEditingAnEditedOccurrencePrunesItsQueuedReminder(t *testing.T) {
	e := editedFixture(t)
	ctx := context.Background()

	queue := func(t *testing.T) domain.QueuedNotification {
		t.Helper()
		q := domain.QueuedNotification{
			UserID:    e.f.maman,
			Kind:      domain.KindReminder,
			SourceRef: ReminderSourceRef(e.series.ID, e.date, 1),
			Payload:   `{}`,
			DueAt:     e.f.at("2026-04-16", 18, 0),
		}
		if err := e.f.st.EnqueueNotification(ctx, q); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		return q
	}
	survives := func(t *testing.T, ref string) bool {
		t.Helper()
		pending, err := e.f.st.ListUnsentBefore(ctx, e.f.at("2026-04-30", 0, 0))
		if err != nil {
			t.Fatalf("list unsent: %v", err)
		}
		for _, p := range pending {
			if p.SourceRef == ref {
				return true
			}
		}
		return false
	}

	q := queue(t)
	if _, err := e.f.svc.Update(ctx, e.f.maman, e.copy.ID, domain.ScopeThis, e.date, Input{
		Title: "Piscine (re-déplacée)", StartsAt: e.f.at("2026-04-17", 18, 0), EndsAt: e.f.at("2026-04-17", 19, 0),
		LabelID: e.f.labels[0].ID, Participants: []int64{e.f.maman},
	}); err != nil {
		t.Fatalf("re-edit: %v", err)
	}
	if survives(t, q.SourceRef) {
		t.Error("a reminder queued for the occurrence's old time survived re-editing it")
	}

	q = queue(t)
	if err := e.f.svc.Delete(ctx, e.f.maman, e.copy.ID, domain.ScopeThis, e.date); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if survives(t, q.SourceRef) {
		t.Error("a reminder for a deleted occurrence survived deleting it")
	}
}

// TestAnEditedOccurrenceAddressedWithoutAScopeIsStillThatOccurrence: an older client —
// or the delete confirmation that never asked, because it thought this was a plain
// event — sends no scope at all. The only occurrence a copy can possibly mean is its
// own, so that request is answered rather than refused.
func TestAnEditedOccurrenceAddressedWithoutAScopeIsStillThatOccurrence(t *testing.T) {
	e := editedFixture(t)
	ctx := context.Background()

	if err := e.f.svc.Delete(ctx, e.f.maman, e.copy.ID, "", domain.Date{}); err != nil {
		t.Fatalf("delete with no scope: %v", err)
	}
	occ := e.f.occurrences(t, "2026-04-01", "2026-04-30")
	if len(occ) != 3 {
		t.Fatalf("got %d occurrences, want 3", len(occ))
	}
	for _, o := range occ {
		if o.OccurrenceDate.Equal(e.date) {
			t.Errorf("the occurrence deleted without a scope came back as %q", o.Title)
		}
	}
}

// TestDetachedOverrideCopyIsAnOrdinaryEventAgain: when a split moves the series out
// from under an override, the copy is deliberately left behind as a standalone event.
// Nothing must then try to resolve it back to a series it no longer belongs to.
func TestDetachedOverrideCopyIsAnOrdinaryEventAgain(t *testing.T) {
	e := editedFixture(t)
	ctx := context.Background()

	// Delete the override row without deleting the copy — exactly what
	// detachUnreachableOverrides does after a split that moved the pattern.
	rec := e.series.RecurrenceID
	if rec == nil {
		t.Fatalf("the series has no recurrence")
	}
	if err := e.f.st.DeleteOverride(ctx, *rec, e.date); err != nil {
		t.Fatalf("detach the override: %v", err)
	}

	occ, err := e.f.svc.Occurrence(ctx, e.copy.ID, domain.Date{})
	if err != nil {
		t.Fatalf("read the detached copy: %v", err)
	}
	if occ.IsOverride || occ.SeriesEventID != nil {
		t.Errorf("a detached copy still claims a series: is_override=%v series=%v", occ.IsOverride, occ.SeriesEventID)
	}
	if err := e.f.svc.Delete(ctx, e.f.maman, e.copy.ID, "", domain.Date{}); err != nil {
		t.Fatalf("delete the detached copy: %v", err)
	}
	if _, err := e.f.st.EventByID(ctx, e.copy.ID); err == nil {
		t.Error("deleting a detached copy left it behind")
	}
}

// TestEditingAnOccurrenceWritesNobodysReminders is the writing half of the rule that an
// edited occurrence *inherits* its series' reminders until somebody changes them on that
// occurrence. Moving a lesson must therefore leave every member's reminders exactly where
// they were: on the series, where a later change to them still reaches this occurrence.
//
// Writing a copy of them onto the copy instead is what this replaces, and it looks
// harmless until you notice what it makes "no rows on the copy" mean — both "cleared on
// purpose" and "written before you set that reminder". A reminder added to the series
// afterwards then never reaches the occurrence, and nobody is told.
func TestEditingAnOccurrenceWritesNobodysReminders(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	series := f.timed(t, "Piscine", "2026-04-07", 17, 30, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
	})
	thirty, day := 30, 1
	if err := f.st.ReplaceReminders(ctx, nil, series.RecurrenceID, f.maman,
		[]domain.Reminder{{OffsetMinutes: &thirty}}); err != nil {
		t.Fatalf("reminders for Maman: %v", err)
	}
	if err := f.st.ReplaceReminders(ctx, nil, series.RecurrenceID, f.papa,
		[]domain.Reminder{{DaysBefore: &day, AtTimeLocal: "09:00"}}); err != nil {
		t.Fatalf("reminders for Papa: %v", err)
	}

	date := domain.MustParseDate("2026-04-14")
	copyEvent, err := f.svc.Update(ctx, f.maman, series.ID, domain.ScopeThis, date, Input{
		Title: "Piscine (déplacée)", StartsAt: f.at("2026-04-14", 19, 0), EndsAt: f.at("2026-04-14", 20, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	})
	if err != nil {
		t.Fatalf("move one occurrence: %v", err)
	}

	for _, who := range []struct {
		name string
		id   int64
	}{{"Maman", f.maman}, {"Papa", f.papa}} {
		own, err := f.st.ListReminders(ctx, &copyEvent.ID, nil, who.id)
		if err != nil {
			t.Fatalf("list %s's reminders on the copy: %v", who.name, err)
		}
		if len(own) != 0 {
			t.Errorf("%s has %d reminders on the edited occurrence, want none of her own:"+
				" the occurrence inherits the series' until somebody changes them on it", who.name, len(own))
		}
		detached, err := f.st.RemindersDetached(ctx, copyEvent.ID, who.id)
		if err != nil {
			t.Fatalf("detachment for %s: %v", who.name, err)
		}
		if detached {
			t.Errorf("%s counts as having set her own reminders on an occurrence somebody"+
				" merely moved; nothing she did says that", who.name)
		}
	}

	// The series keeps everyone's, which is what the occurrence goes on reading.
	rest, err := f.st.ListReminders(ctx, nil, series.RecurrenceID, f.maman)
	if err != nil {
		t.Fatalf("list Maman's reminders on the series: %v", err)
	}
	if len(rest) != 1 {
		t.Errorf("the series has %d of Maman's reminders after one occurrence was edited, want 1", len(rest))
	}

	// Changing them on the occurrence is a deliberate act, and it detaches that member
	// there and only there. Maman wants two hours' warning for this one lesson.
	twoHours := 120
	if err := f.st.ReplaceReminders(ctx, &copyEvent.ID, nil, f.maman,
		[]domain.Reminder{{OffsetMinutes: &twoHours}}); err != nil {
		t.Fatalf("set Maman's reminder on the occurrence: %v", err)
	}
	if detached, err := f.st.RemindersDetached(ctx, copyEvent.ID, f.maman); err != nil || !detached {
		t.Errorf("after Maman saved a list on the occurrence, detached = %v, %v; want true", detached, err)
	}
	if detached, err := f.st.RemindersDetached(ctx, copyEvent.ID, f.papa); err != nil || detached {
		t.Errorf("Maman's choice for this lesson detached Papa too: detached = %v, %v", detached, err)
	}

	// And a second edit of the same occurrence still writes nobody's reminders: it is
	// an edit to the event, not to anyone's mind about being reminded.
	if _, err := f.svc.Update(ctx, f.maman, copyEvent.ID, domain.ScopeThis, date, Input{
		Title: "Piscine (re-déplacée)", StartsAt: f.at("2026-04-14", 20, 0), EndsAt: f.at("2026-04-14", 21, 0),
		LabelID: f.labels[0].ID, Participants: []int64{f.maman},
	}); err != nil {
		t.Fatalf("re-edit the occurrence: %v", err)
	}
	again, err := f.st.ListReminders(ctx, &copyEvent.ID, nil, f.maman)
	if err != nil {
		t.Fatalf("list Maman's reminders after the second edit: %v", err)
	}
	if len(again) != 1 || again[0].OffsetMinutes == nil || *again[0].OffsetMinutes != 120 {
		t.Errorf("Maman's reminders on the occurrence after a second edit = %+v, want the two hours she set", again)
	}
	if papas, err := f.st.ListReminders(ctx, &copyEvent.ID, nil, f.papa); err != nil {
		t.Fatalf("list Papa's reminders after the second edit: %v", err)
	} else if len(papas) != 0 {
		t.Errorf("Papa gained %d reminders on the occurrence from an edit he did not make", len(papas))
	}
}
