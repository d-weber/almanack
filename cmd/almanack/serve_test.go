package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"almanack/internal/config"
)

// sd_notify is a datagram to whatever NOTIFY_SOCKET names, so a test can be systemd:
// bind the socket, point the variable at it, and read what the process told the service
// manager — and, more to the point here, in what order.
type sdRecorder struct {
	t    *testing.T
	conn *net.UnixConn
}

func newSDRecorder(t *testing.T) *sdRecorder {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes, which a test name plus
	// t.TempDir() can get close to; the short name keeps well clear.
	path := filepath.Join(t.TempDir(), "sd.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("bind the notify socket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	return &sdRecorder{t: t, conn: conn}
}

// received drains what has been sent so far. A datagram is queued on the receiving
// socket by the time the sender's Write returns, so a caller that has already observed
// the effect it is testing — runServe returning, the callback returning — is not racing
// delivery, and the deadline below only bounds the empty case.
func (r *sdRecorder) received() []string {
	r.t.Helper()
	if err := r.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		r.t.Fatalf("set a read deadline on the notify socket: %v", err)
	}
	var got []string
	buf := make([]byte, 4096)
	for {
		n, err := r.conn.Read(buf)
		if err != nil {
			return got
		}
		got = append(got, string(buf[:n]))
	}
}

// waitFor blocks until one particular state arrives.
func (r *sdRecorder) waitFor(state string, timeout time.Duration) {
	r.t.Helper()
	if err := r.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		r.t.Fatalf("set a read deadline on the notify socket: %v", err)
	}
	buf := make([]byte, 4096)
	for {
		n, err := r.conn.Read(buf)
		if err != nil {
			r.t.Fatalf("waiting for %q from sd_notify: %v", state, err)
		}
		if strings.Contains(string(buf[:n]), state) {
			return
		}
	}
}

// serveConfig is the smallest configuration runServe will start on: development mode, so
// mail goes to a directory and no VAPID keys are needed, and everything under a temp dir.
func serveConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	return config.Config{
		Dev:           true,
		ListenAddr:    "127.0.0.1:0",
		BaseURL:       "http://localhost:8080",
		DataPath:      filepath.Join(dir, "almanack.db"),
		BackupDir:     filepath.Join(dir, "backups"),
		MailDir:       filepath.Join(dir, "mail"),
		TZName:        "Europe/Paris",
		FamilyTZ:      testTZ(t),
		SchedulerTick: 30 * time.Second,
		PlanHorizon:   48 * time.Hour,
	}
}

// READY=1 is what makes `systemctl restart` return, and what the install checklist reads
// as "the service is up". It used to be sent before anything had bound the port: the
// listener was opened in a goroutine afterwards, so a busy address — a second copy of
// the unit, or a proxy that had taken 8080 — produced a unit systemd marked active and
// then watched die, which is the exact distinction Type=notify was chosen for.
func TestServeDoesNotSignalReadyWhenItCannotBind(t *testing.T) {
	sd := newSDRecorder(t)

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy an address: %v", err)
	}
	defer busy.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := serveConfig(t)
	cfg.ListenAddr = busy.Addr().String()

	if err := runServe(ctx, cfg); err == nil {
		t.Fatal("runServe returned no error although its address was already in use")
	}
	for _, state := range sd.received() {
		if strings.Contains(state, "READY=1") {
			t.Errorf("READY=1 was sent although nothing ever bound %s: systemd would report a dead unit as active", cfg.ListenAddr)
		}
	}
}

// The other half of the same ordering: on the ordinary path readiness must still be
// signalled, and the process must still stop cleanly when its context is cancelled.
func TestServeSignalsReadyAndStopsCleanly(t *testing.T) {
	sd := newSDRecorder(t)
	cfg := serveConfig(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg) }()

	sd.waitFor("READY=1", 30*time.Second)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runServe did not return after its context was cancelled")
	}
}

// The watchdog ping used to be throttled to once per half of WatchdogSec — but it is
// only ever evaluated when a scheduler tick completes, so the real spacing was that half
// plus a whole tick, against a systemd deadline of the whole WatchdogSec. An operator who
// set ALMANACK_TICK=90s to reduce load turned a healthy server into a restart loop. A
// datagram per tick costs nothing, so there is no throttle left to get this wrong.
func TestWatchdogPingsOnEveryTick(t *testing.T) {
	sd := newSDRecorder(t)
	t.Setenv("WATCHDOG_USEC", "120000000") // WatchdogSec=120s

	ping := watchdog(30 * time.Second)
	if ping == nil {
		t.Fatal("no watchdog callback although WATCHDOG_USEC is set")
	}
	for range 3 {
		ping()
	}

	pings := 0
	for _, state := range sd.received() {
		if state == "WATCHDOG=1" {
			pings++
		}
	}
	if pings != 3 {
		t.Errorf("three completed ticks produced %d watchdog pings, want 3", pings)
	}
}

// Nothing to feed and nothing to warn about when the unit has no WatchdogSec.
func TestWatchdogIsAbsentWithoutAUnit(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if watchdog(30*time.Second) != nil {
		t.Error("a watchdog callback was returned outside a unit with WatchdogSec")
	}
}

// TestWatchdogWarnsWithTooLittleRoom pins where the warning fires.
//
// Pinging once per tick makes the spacing one tick only if a tick is instant. It is not:
// the ping is emitted after Tick returns, and a tick contending for the database has
// been observed taking seconds. So the margin that matters is systemd's own halving of
// WATCHDOG_USEC, and warning only when the tick reached the whole deadline let
// ALMANACK_TICK=119s against the shipped WatchdogSec=120s pass in silence and then
// restart-loop the first time a tick ran long.
func TestWatchdogWarnsWithTooLittleRoom(t *testing.T) {
	cases := []struct {
		name string
		tick time.Duration
		warn bool
	}{
		{"the shipped defaults are quiet", 30 * time.Second, false},
		{"right up to half is quiet", 60 * time.Second, false},
		{"a second past half warns", 61 * time.Second, true},
		{"the case that used to slip through", 119 * time.Second, true},
		{"a tick as long as the deadline warns", 120 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newSDRecorder(t)
			t.Setenv("WATCHDOG_USEC", "120000000") // WatchdogSec=120s

			var log bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			if watchdog(tc.tick) == nil {
				t.Fatal("no watchdog callback although WATCHDOG_USEC is set")
			}
			if warned := strings.Contains(log.String(), "ALMANACK_TICK"); warned != tc.warn {
				t.Errorf("tick %s under WatchdogSec=120s warned=%v, want %v; log was %q",
					tc.tick, warned, tc.warn, log.String())
			}
		})
	}
}
