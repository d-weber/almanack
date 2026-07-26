package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// clockStepThreshold is the notification rules in docs/architecture.md: a jump of more than five minutes
// between ticks means the system clock was corrected, not that time passed. The
// response is to re-plan before delivering anything, which is what a tick does
// anyway — this only logs it, so that a mass delivery has an explanation next to
// it in the journal.
const clockStepThreshold = 5 * time.Minute

// CatchUpSummary is what the boot catch-up did. The caller logs it and folds it
// into the ops heartbeat: on a family server nothing fails loudly by itself, and
// "delivered 3, skipped 11" after a week of downtime is the difference between a
// working recovery and a silent one.
type CatchUpSummary struct {
	// At is when the catch-up ran, zero if it has not.
	At time.Time `json:"at,omitzero"`
	// Gap is how long the planner had been idle — the materialization hole the
	// backfill had to fill.
	Gap time.Duration `json:"gap"`
	// Backfilled counts rows the gap plan materialized that would otherwise never
	// have existed at all.
	Backfilled int `json:"backfilled"`
	// Delivered counts overdue rows sent late.
	Delivered int `json:"delivered"`
	// Skipped counts overdue rows retired by policy: reminders for events that
	// have already happened, digests whose slot passed hours ago.
	Skipped int `json:"skipped"`
	// Truncated is true when the gap exceeded maxBackfill and the backfill
	// started later than the last planned horizon.
	Truncated bool `json:"truncated,omitempty"`
}

// String renders the one line the ops heartbeat carries.
func (s CatchUpSummary) String() string {
	if s.At.IsZero() {
		return "catch-up: not run"
	}
	return fmt.Sprintf("catch-up: gap %s · backfilled %d · delivered %d · skipped %d",
		s.Gap.Round(time.Minute), s.Backfilled, s.Delivered, s.Skipped)
}

// CatchUp runs the boot policy of the notification rules in docs/architecture.md. It must be called once at
// startup, before the first tick; Run does it if the caller has not.
//
//  1. Backfill the materialization gap from the last planned horizon to
//     now + horizon. This is the step nothing else can substitute for: a week of
//     downtime leaves reminders that were never turned into queue rows at all,
//     and no amount of "deliver the overdue rows" logic can deliver a row that
//     does not exist.
//  2. Deliver each overdue row whose event is still ahead, and stale-skip the
//     rest with a reason.
//  3. Digests: today's goes out if its slot passed less than four hours ago;
//     older ones are dropped rather than pushing yesterday's plan.
//
// Steps 2 and 3 are the ordinary delivery path: the staleness policy lives in
// dispatch.go so that a row which falls behind for some other reason is judged by
// exactly the same rules.
func (n *Notifier) CatchUp(ctx context.Context) (CatchUpSummary, error) {
	now := n.now()
	sum := CatchUpSummary{At: now}
	if err := n.checkClock(now); err != nil {
		return sum, err
	}

	last, err := n.plannedThrough(ctx)
	if err != nil {
		return sum, fmt.Errorf("catch-up: %w", err)
	}
	to := now.Add(n.horizon)
	from := last
	if from.IsZero() || from.After(now) {
		// A fresh database, or a horizon that still reaches into the future:
		// there is no hole to fill.
		from = now
	}
	sum.Gap = now.Sub(from)
	if sum.Gap > maxBackfill {
		from = now.Add(-maxBackfill)
		sum.Truncated = true
	}

	before, err := n.pendingCount(ctx, to)
	if err != nil {
		return sum, fmt.Errorf("catch-up: %w", err)
	}
	planErr := n.plan(ctx, from, to)
	after, err := n.pendingCount(ctx, to)
	if err != nil {
		return sum, fmt.Errorf("catch-up: %w", err)
	}
	if after > before {
		sum.Backfilled = after - before
	}

	counts, drainErr := n.drain(ctx)
	sum.Delivered = counts.Delivered
	sum.Skipped = counts.Skipped

	n.mu.Lock()
	n.caughtUp = true
	n.catchUp = sum
	n.mu.Unlock()

	slog.Info("boot catch-up complete", "gap", sum.Gap.Round(time.Second),
		"backfilled", sum.Backfilled, "delivered", sum.Delivered, "skipped", sum.Skipped,
		"truncated", sum.Truncated)

	if err := errors.Join(planErr, drainErr); err != nil {
		return sum, fmt.Errorf("catch-up: %w", err)
	}
	return sum, nil
}

// pendingCount counts queue rows that are neither sent nor skipped and fall
// before t. Comparing it either side of the backfill is how the summary knows how
// many rows the gap plan created: EnqueueNotification is an INSERT OR IGNORE and
// deliberately reports nothing.
func (n *Notifier) pendingCount(ctx context.Context, t time.Time) (int, error) {
	rows, err := n.st.ListUnsentBefore(ctx, t)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// Tick is one planning-and-delivery pass. Dev mode drives it from POST /dev/tick
// and tests drive it directly, which is how the whole pipeline is exercised
// without waiting thirty seconds or a day.
//
// Planning happens before delivery, always. After a clock correction that is what
// turns a jump into a re-plan instead of a mass delivery of everything the new
// "now" makes due.
func (n *Notifier) Tick(ctx context.Context) error {
	now := n.now()
	if err := n.checkClock(now); err != nil {
		n.noteTick(time.Time{}, err)
		slog.Error("scheduler refuses to run", "error", err)
		return err
	}
	n.noteStep(now)

	err := errors.Join(n.Plan(ctx), n.Dispatch(ctx))
	n.noteTick(now, err)
	return err
}

// noteStep logs a clock correction between ticks.
func (n *Notifier) noteStep(now time.Time) {
	n.mu.Lock()
	prev := n.lastTickReal
	n.lastTickReal = now
	n.mu.Unlock()

	if prev.IsZero() {
		return
	}
	step := now.Sub(prev)
	if step < 0 {
		step = -step
	}
	if step > n.tick+clockStepThreshold {
		slog.Warn("the clock stepped between ticks; re-planning before delivery",
			"from", prev.Format(time.RFC3339), "to", now.Format(time.RFC3339), "step", step.Round(time.Second))
	}
}

// Run is the scheduler goroutine: plan, dispatch, repeat, until the context is
// cancelled.
//
// watchdog may be nil; when set it is called after every pass, which is what
// systemd's WatchdogSec pings. It is called even when the pass returned an error:
// the watchdog answers "is this goroutine alive", and a failing tick that is
// still ticking is a different fault from a wedged one — /healthz reports that
// one through Health.LastError.
//
// Concurrency stays boring on purpose (CONVENTIONS §3): one goroutine and one
// ticker. The ticker runs on real time, not on clock.Clock, because it is a
// pacing device rather than a source of truth; tests call Tick directly.
func (n *Notifier) Run(ctx context.Context, watchdog func()) error {
	if err := n.checkClock(n.now()); err != nil {
		return err
	}

	n.mu.Lock()
	done := n.caughtUp
	n.mu.Unlock()
	if !done {
		// The boot policy is not optional, so a caller who forgets it still gets
		// it. A caller that wants the summary calls CatchUp itself, first.
		if _, err := n.CatchUp(ctx); err != nil {
			slog.Error("boot catch-up failed; continuing with the normal schedule", "error", err)
		}
	}

	t := time.NewTicker(n.tick)
	defer t.Stop()
	for {
		if err := n.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Error("scheduler tick", "error", err)
		}
		if watchdog != nil {
			watchdog()
		}
		select {
		case <-ctx.Done():
			slog.Info("scheduler stopped")
			return nil
		case <-t.C:
		}
	}
}
