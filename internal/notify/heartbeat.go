package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"agenda/internal/domain"
	"agenda/internal/mailer"
)

// MetaHeartbeatSent records the last day the operator's summary went out, so a
// restart cannot send it twice and a missed day is not sent late.
const MetaHeartbeatSent = "notify.heartbeat_sent"

// The daily note to whoever runs the server.
//
// A household server has no pager and nobody watching a dashboard, so this mail —
// and, more usefully, its absence — is the monitoring. Everything it reports is
// something that fails silently: a backup timer that was never installed, a mail
// relay that stopped accepting messages, a scheduler that wedged, a disk quietly
// filling up. None of those announce themselves.
//
// It is deliberately sent even when everything is fine. A message that only arrives
// when something is wrong teaches you nothing when it does not arrive.
func (n *Notifier) maybeSendHeartbeat(ctx context.Context) error {
	if n.ownerEmail == "" || n.heartbeatAt == "" || n.mail == nil {
		return nil
	}
	h, m, ok := parseHM(n.heartbeatAt)
	if !ok {
		return nil
	}

	now := n.now()
	today := domain.DateIn(now, n.loc)
	if now.Before(today.At(h, m, n.loc)) {
		return nil // the slot has not arrived yet today
	}

	last, err := n.st.GetMeta(ctx, MetaHeartbeatSent)
	if err != nil {
		return err
	}
	if last == today.String() {
		return nil
	}

	body, err := n.heartbeatBody(ctx, today)
	if err != nil {
		return err
	}
	subject := n.cat.T(domain.LangEN, "mail.subject.heartbeat", nil)
	if err := n.mail.Send(ctx, mailer.Message{To: n.ownerEmail, Subject: subject, Text: body}); err != nil {
		// Leave the marker unset: if the mail path is broken the operator has not
		// been told, and tomorrow's attempt should not think today's succeeded.
		return fmt.Errorf("send the daily heartbeat: %w", err)
	}
	if err := n.st.SetMeta(ctx, MetaHeartbeatSent, today.String()); err != nil {
		return err
	}
	slog.Info("daily heartbeat sent", "to", n.ownerEmail)
	return nil
}

func (n *Notifier) heartbeatBody(ctx context.Context, day domain.Date) (string, error) {
	h := n.Health(ctx)

	var b strings.Builder
	fmt.Fprintf(&b, "Agenda — %s\n\n", day)

	status := "everything looks fine"
	var problems []string

	if !h.ClockOK {
		problems = append(problems, "the system clock is implausible; the scheduler is not running")
	}
	// The heartbeat rides inside a tick, so on the very first pass after a restart
	// no tick has been recorded yet. That is startup, not a fault.
	if !h.LastTick.IsZero() {
		if age := n.now().Sub(h.LastTick); age > 10*n.tick {
			problems = append(problems, fmt.Sprintf("the scheduler last completed a pass %s ago", age.Round(time.Minute)))
		}
	}
	if h.MailFailures > 0 {
		problems = append(problems, fmt.Sprintf("%d consecutive email failures", h.MailFailures))
	}
	for service, count := range h.PushFailures {
		problems = append(problems, fmt.Sprintf("%d push failures to %s", count, service))
	}

	backupAt, _ := n.st.GetMeta(ctx, "last_backup_at")
	backupResult, _ := n.st.GetMeta(ctx, "last_backup_result")
	switch {
	case backupAt == "":
		problems = append(problems, "no backup has ever been recorded — is the backup timer installed?")
	case backupResult != "" && backupResult != "ok":
		problems = append(problems, "the last backup FAILED: "+backupResult)
	default:
		if t, err := time.Parse(time.RFC3339, backupAt); err == nil {
			if age := n.now().Sub(t); age > 48*time.Hour {
				problems = append(problems, fmt.Sprintf("the last successful backup was %s ago", age.Round(time.Hour)))
			}
		}
	}

	if len(problems) > 0 {
		status = "NEEDS ATTENTION"
	}
	fmt.Fprintf(&b, "Status: %s\n", status)
	for _, p := range problems {
		fmt.Fprintf(&b, "  - %s\n", p)
	}

	fmt.Fprintf(&b, "\nNotifications queued: %d", h.QueueDepth)
	if h.Overdue > 0 {
		fmt.Fprintf(&b, " (%d overdue)", h.Overdue)
	}
	fmt.Fprintf(&b, "\nLast backup: %s", describeBackup(backupAt, backupResult, n.now()))
	if !h.LastTick.IsZero() {
		fmt.Fprintf(&b, "\nLast scheduler pass: %s ago", n.now().Sub(h.LastTick).Round(time.Second))
	} else {
		b.WriteString("\nLast scheduler pass: this is the first since starting up")
	}
	if !h.CatchUp.At.IsZero() {
		fmt.Fprintf(&b, "\nLast restart recovery: %s", h.CatchUp.String())
	}
	b.WriteString("\n\nThis note arrives once a day. If it stops arriving, something is wrong\n")
	b.WriteString("with the server or its mail path — which is the point of sending it.\n")
	return b.String(), nil
}

func describeBackup(at, result string, now time.Time) string {
	if at == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return at
	}
	if result == "" || result == "ok" {
		return fmt.Sprintf("%s ago, verified", now.Sub(t).Round(time.Minute))
	}
	return fmt.Sprintf("%s ago, FAILED: %s", now.Sub(t).Round(time.Minute), result)
}
