package httpapi

import (
	"context"
	"net/http"
	"path/filepath"
	"time"
)

// healthResponse is what /healthz answers. It carries no secrets and needs no
// authentication: the deploy play polls it, and a check that requires a session is a
// check nobody runs.
type healthResponse struct {
	Status     string         `json:"status"`
	AppVersion string         `json:"app_version"`
	Time       time.Time      `json:"time"`
	Checks     map[string]any `json:"checks"`
}

// Thresholds. A family server has no pager: the point of these is that the daily
// heartbeat mail and this endpoint agree on what "wrong" means.
const (
	backupStaleAfter = 48 * time.Hour
	mailFailureLimit = 3
	diskFreeMinRatio = 0.10
	diskFreeMinBytes = 200 << 20
)

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.clock.Now()
	checks := map[string]any{}
	degraded := false

	// Database.
	if err := s.store.Ping(ctx); err != nil {
		checks["database"] = map[string]any{"ok": false, "error": err.Error()}
		degraded = true
	} else {
		checks["database"] = map[string]any{"ok": true}
	}

	checks["scheduler"] = s.schedulerCheck(ctx, now, &degraded)
	checks["backup"] = s.backupCheck(ctx, now, &degraded)

	// Mail. A silent parallel channel that has been failing for a week is the failure
	// mode this counter exists to make loud.
	failures := 0
	if s.mailer != nil {
		failures = s.mailer.Failures()
	}
	if failures >= mailFailureLimit {
		degraded = true
	}
	checks["mail"] = map[string]any{
		"ok":                    failures < mailFailureLimit,
		"consecutive_failures":  failures,
		"configured":            s.mailer != nil,
		"failure_alert_at":      mailFailureLimit,
		"dev_file_sink_enabled": s.cfg.Dev,
	}

	checks["disk"] = s.diskCheck(&degraded)
	checks["push"] = s.pushCheck(ctx, now)

	status := "ok"
	code := http.StatusOK
	if degraded {
		status, code = "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, r, code, healthResponse{
		Status:     status,
		AppVersion: s.version,
		Time:       now,
		Checks:     checks,
	})
}

// schedulerCheck judges the notification pipeline. A heartbeat that has stopped means
// reminders have stopped, which is the whole failure this application exists to prevent —
// so it is worth a 503 even though every HTTP request still works.
func (s *Server) schedulerCheck(ctx context.Context, now time.Time, degraded *bool) map[string]any {
	check := map[string]any{}

	tick := s.cfg.SchedulerTick
	var beat time.Time
	var known bool

	if reporter, ok := s.notifier.(SchedulerHealth); ok {
		if interval := reporter.TickInterval(); interval > 0 {
			tick = interval
		}
		beat = reporter.Heartbeat()
		known = true
		check["source"] = "scheduler"
	} else {
		// No notifier in this process: fall back to the meta table, which a scheduler
		// running elsewhere can write.
		raw, err := s.store.GetMeta(ctx, MetaSchedulerHeartbeat)
		if err != nil {
			*degraded = true
			return map[string]any{"ok": false, "error": err.Error()}
		}
		check["source"] = "meta"
		if raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				*degraded = true
				return map[string]any{"ok": false, "error": "unparsable heartbeat"}
			}
			beat, known = parsed, true
		}
	}

	tolerance := 5 * tick
	if tolerance < 5*time.Minute {
		tolerance = 5 * time.Minute
	}
	check["tick_seconds"] = int64(tick.Seconds())

	switch {
	case !known || beat.IsZero():
		// Nothing has ticked yet. Normal for the first seconds after a start, alarming
		// after that.
		ok := now.Sub(s.startedAt) < tolerance
		check["heartbeat"] = "never"
		check["ok"] = ok
		if !ok {
			*degraded = true
		}
	default:
		age := now.Sub(beat)
		check["heartbeat_age_seconds"] = int64(age.Seconds())
		check["ok"] = age <= tolerance
		if age > tolerance {
			*degraded = true
		}
	}

	// Queue depth comes from the database rather than from a counter in memory: a stale
	// number here would be worse than none, and this one survives a restart.
	horizon := s.cfg.PlanHorizon
	if horizon <= 0 {
		horizon = 48 * time.Hour
	}
	if pending, err := s.store.ListUnsentBefore(ctx, now.Add(horizon)); err == nil {
		overdue := 0
		for _, q := range pending {
			if q.DueAt.Before(now) {
				overdue++
			}
		}
		check["queue_depth"] = len(pending)
		// An overdue count that does not come back to zero means delivery is failing.
		check["overdue"] = overdue
	}
	return check
}

// backupCheck reports the age and result of the last snapshot. A server that has never
// been backed up is not degraded — it may be five minutes old — but one whose last
// backup failed, or is two days stale, is.
func (s *Server) backupCheck(ctx context.Context, now time.Time, degraded *bool) map[string]any {
	result, err := s.store.GetMeta(ctx, MetaLastBackupResult)
	if err != nil {
		*degraded = true
		return map[string]any{"ok": false, "error": err.Error()}
	}
	rawAt, err := s.store.GetMeta(ctx, MetaLastBackupAt)
	if err != nil {
		*degraded = true
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if rawAt == "" {
		return map[string]any{"ok": true, "last": "never", "dir": s.cfg.BackupDir}
	}

	check := map[string]any{"result": result, "dir": s.cfg.BackupDir}
	at, err := time.Parse(time.RFC3339, rawAt)
	if err != nil {
		check["ok"] = false
		check["error"] = "unparsable backup timestamp"
		*degraded = true
		return check
	}
	age := now.Sub(at)
	check["age_seconds"] = int64(age.Seconds())
	ok := age <= backupStaleAfter && (result == "" || result == "ok")
	check["ok"] = ok
	if !ok {
		*degraded = true
	}
	return check
}

// diskCheck reports free space where the database lives. SQLite on a full disk is the
// classic way a small server stops accepting writes, and the failure is silent until
// somebody tries to save an event.
func (s *Server) diskCheck(degraded *bool) map[string]any {
	dir := filepath.Dir(s.cfg.DataPath)
	if dir == "" || dir == "." {
		dir = "."
	}
	total, free, err := diskUsage(dir)
	if err != nil {
		return map[string]any{"ok": true, "unavailable": err.Error(), "path": dir}
	}
	ratio := 0.0
	if total > 0 {
		ratio = float64(free) / float64(total)
	}
	ok := free >= diskFreeMinBytes && ratio >= diskFreeMinRatio
	if !ok {
		*degraded = true
	}
	return map[string]any{
		"ok":              ok,
		"path":            dir,
		"total_bytes":     total,
		"free_bytes":      free,
		"free_percent":    int(ratio * 100),
		"free_min_bytes":  int64(diskFreeMinBytes),
		"free_min_ratio":  diskFreeMinRatio,
		"database_exists": s.cfg.DataPath != "",
	}
}

// pushCheck counts devices, how many have gone quiet, and how many are failing — the
// signal that says a push service has changed its behaviour and the sender needs a
// patch. It never reports an endpoint: one is a capability to notify somebody's phone.
//
// It does not report the service either, though it used to. This endpoint needs no
// session, and the host is the part of an endpoint a member chooses, so a per-host
// failure count answered "did the delivery to the host I registered succeed?" to
// anybody who asked. Aggregating keeps the question monitoring asks — is push working?
// — and drops the one an attacker asks. Which service is failing is a real diagnostic,
// and it still goes out every day in the operator's heartbeat mail (internal/notify),
// which has a recipient rather than a URL.
func (s *Server) pushCheck(ctx context.Context, now time.Time) map[string]any {
	subs, err := s.store.ListAllPushSubscriptions(ctx)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	stale, failing := 0, 0
	for _, sub := range subs {
		if sub.LastConfirmedAt.IsZero() || now.Sub(sub.LastConfirmedAt) > pushStaleAfter {
			stale++
		}
		if sub.Failures > 0 {
			failing++
		}
	}
	// Stale subscriptions are a client-side repair prompt, not a server fault: email is
	// forced on for those users, so nothing is missed.
	return map[string]any{"ok": true, "devices": len(subs), "stale": stale, "failing": failing}
}
