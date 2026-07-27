package notify

import (
	"context"
	"testing"
	"time"

	"almanack/internal/domain"
)

// Regressions found by an adversarial review of the built system. Each of these
// failed before the fix in the same commit, and each describes a way the family
// would have been told the wrong thing — or nothing at all.

// A reminder whose slot has already passed used to be dropped by the planner,
// which meant an appointment added shortly before it starts produced no
// notification on that tick or on any later one. The planner now queues it and
// lets delivery decide, which is where the "late warning beats no warning" rule
// already lived.
func TestReminderForAnImminentEventIsStillPlanned(t *testing.T) {
	now := time.Date(2027, 6, 7, 6, 0, 0, 0, time.UTC) // 08:00 in Paris
	e := newEnv(t, now)
	ctx := context.Background()

	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	// An appointment twenty minutes away, with a reminder that should have gone
	// out ten minutes ago.
	ev := e.timedEvent(cal, alice.ID, "Dentiste", 2027, time.June, 7, 8, 20, time.Hour, nil)
	e.reminderMinutes(ev, alice.ID, 30)

	if err := e.n.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if got := len(e.push.received()); got != 1 {
		t.Fatalf("an appointment 20 minutes away produced %d notifications, want 1"+
			" (the reminder slot was already past, which is exactly when it matters most)", got)
	}
}

// The same slot, but for an event that has already started, must NOT be
// resurrected — the planner's new leniency is bounded by the event itself.
func TestReminderForAnEventAlreadyStartedIsNotPlanned(t *testing.T) {
	now := time.Date(2027, 6, 7, 8, 0, 0, 0, time.UTC) // 10:00 Paris
	e := newEnv(t, now)
	ctx := context.Background()

	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	ev := e.timedEvent(cal, alice.ID, "Déjà commencé", 2027, time.June, 7, 9, 0, time.Hour, nil)
	e.reminderMinutes(ev, alice.ID, 30)

	if err := e.n.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := len(e.push.received()); got != 0 {
		t.Errorf("an event that started an hour ago produced %d notifications, want 0", got)
	}
}

// Replacing a reminder used to leave the old one queued, so changing "60 minutes
// before" to "30 minutes before" produced two notifications, and deleting a
// reminder still fired it.
func TestReplacingAReminderRemovesTheQueuedOne(t *testing.T) {
	now := time.Date(2027, 6, 7, 6, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	ctx := context.Background()

	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.noDigests()

	ev := e.timedEvent(cal, alice.ID, "Dentiste", 2027, time.June, 7, 17, 0, time.Hour, nil)
	e.reminderMinutes(ev, alice.ID, 60)
	if err := e.n.Plan(ctx); err != nil {
		t.Fatalf("plan: %v", err)
	}

	// The family changes its mind: half an hour is enough.
	e.setReminders(ev, alice.ID, []domain.Reminder{
		{EventID: &ev.ID, UserID: alice.ID, OffsetMinutes: ptrInt(30)},
	})
	if err := e.n.Plan(ctx); err != nil {
		t.Fatalf("re-plan: %v", err)
	}

	var pending int
	for _, row := range e.queue() {
		if row.Kind == domain.KindReminder && row.SentAt.IsZero() && row.Skipped == "" {
			pending++
		}
	}
	if pending != 1 {
		t.Errorf("%d reminders queued for one appointment after changing 60 minutes to 30, want 1", pending)
	}
}

// Switching the daily digest off left the next two days' digests in the outbox,
// so it arrived anyway; moving its time queued a second one.
func TestChangingDigestPreferencesClearsTheQueuedOnes(t *testing.T) {
	now := time.Date(2027, 6, 7, 6, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	ctx := context.Background()

	alice := e.user("alice")
	cal := e.calendar("Famille", alice.ID)
	e.subscribe(alice.ID, "iphone")
	e.timedEvent(cal, alice.ID, "Piscine", 2027, time.June, 8, 17, 30, time.Hour, nil)

	e.setPrefs(domain.NotificationPrefs{
		UserID: alice.ID, DigestEnabled: true, DigestTime: "07:30", SummaryTime: "20:00",
	})
	if err := e.n.Plan(ctx); err != nil {
		t.Fatalf("plan: %v", err)
	}

	// Off it goes.
	e.setPrefs(domain.NotificationPrefs{
		UserID: alice.ID, DigestEnabled: false, DigestTime: "07:30", SummaryTime: "20:00",
	})
	if err := e.n.Plan(ctx); err != nil {
		t.Fatalf("re-plan: %v", err)
	}

	for _, row := range e.queue() {
		if row.Kind == domain.KindDigest && row.SentAt.IsZero() && row.Skipped == "" {
			t.Errorf("a digest is still queued for %s after the digest was switched off", row.DueAt)
		}
	}
}

func ptrInt(v int) *int { return &v }
