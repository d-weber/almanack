package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"almanack/internal/clock"
	"almanack/internal/config"
	"almanack/internal/events"
	"almanack/internal/httpapi"
	"almanack/internal/i18n"
	"almanack/internal/mailer"
	"almanack/internal/notify"
	"almanack/internal/store"
	"almanack/internal/webpush"
)

// runServe wires everything together and runs until the context is cancelled.
//
// The ordering here is deliberate. Migrations finish and the listener is bound before
// READY is signalled, so a deployment health check can tell "still migrating" from
// "dead", and a unit systemd has marked active is one that is answering rather than one
// about to exit on a busy port. The scheduler's catch-up runs before the first tick, so
// an outage cannot leave a hole in the reminders. And the watchdog is fed by the
// scheduler rather than by a timer of its own, so a wedged scheduler restarts the
// process instead of leaving a server that answers HTTP cheerfully while sending nothing.
func runServe(ctx context.Context, cfg config.Config) error {
	slog.Info("starting", "version", version, "config", cfg.Redacted())

	clk := newClock(cfg)

	// Opening the store applies migrations and refuses to start against a schema
	// newer than this binary understands.
	st, err := store.Open(cfg.DataPath, cfg.FamilyTZ, clk)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	catalog, err := i18n.Load()
	if err != nil {
		return fmt.Errorf("load translations: %w", err)
	}

	mail, err := buildMailer(cfg)
	if err != nil {
		return err
	}

	var sender *webpush.Sender
	if cfg.VAPIDPublic != "" && cfg.VAPIDPrivate != "" {
		sender, err = webpush.NewSender(cfg.VAPIDPublic, cfg.VAPIDPrivate, cfg.VAPIDSubject, nil)
		if err != nil {
			return fmt.Errorf("web push keys: %w", err)
		}
	} else {
		slog.Warn("no VAPID keys configured: push notifications are disabled, email only (run `almanack gen-vapid`)")
	}

	eventSvc := events.New(st, cfg.FamilyTZ, clk)

	notifier, err := notify.New(notify.Options{
		Store: st, Events: eventSvc, Push: sender, PushHosts: cfg.PushHosts,
		Mailer: mail, Catalog: catalog,
		Clock: clk, Location: cfg.FamilyTZ, BaseURL: cfg.BaseURL,
		Horizon: cfg.PlanHorizon, Tick: cfg.SchedulerTick,
		OwnerEmail: cfg.OwnerEmail, HeartbeatTime: cfg.HeartbeatTime,
	})
	if err != nil {
		return fmt.Errorf("notification scheduler: %w", err)
	}

	assetFS, err := assets()
	if err != nil {
		return err
	}
	srv, err := httpapi.New(httpapi.Deps{
		Store: st, Events: eventSvc, Notifier: notifier, Mailer: mail,
		Catalog: catalog, Clock: clk, Config: cfg, Web: assetFS,
	})
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Bind before announcing readiness. systemd marks the unit active on READY=1 and
	// `systemctl restart` returns there, so a bind failure discovered afterwards — a
	// second copy of the unit, a proxy holding the port — reported success on a service
	// that was already dying, which is the one distinction Type=notify exists to make.
	// Connections arriving between here and Serve wait in the accept backlog.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}

	// Migrations are done, the router is built and the port is bound: the service is
	// genuinely ready.
	sdReady()
	sdStatus("serving on " + ln.Addr().String())
	slog.Info("ready", "listen", ln.Addr().String(), "base_url", cfg.BaseURL,
		"timezone", cfg.TZName, "app_version", srv.AppVersion(), "dev_mode", cfg.Dev)
	if cfg.Dev {
		slog.Info("development mode", "url", cfg.BaseURL, "dev_tools", cfg.BaseURL+"/dev/", "mail", cfg.MailDir)
	}

	errs := make(chan error, 2)

	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server: %w", err)
		}
	}()

	go func() {
		// Run performs the boot catch-up before its first tick.
		if err := notifier.Run(ctx, watchdog(cfg.SchedulerTick)); err != nil {
			errs <- fmt.Errorf("scheduler: %w", err)
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	sdStopping()
	sdStatus("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	slog.Info("stopped")
	return nil
}

// newClock returns the real clock, or in development a fake one starting at the
// current instant. Dev mode can then travel forward through POST /dev/clock, which is
// how a digest scheduled for 07:30 tomorrow gets tested without waiting for tomorrow.
func newClock(cfg config.Config) clock.Clock {
	if !cfg.Dev {
		return clock.Real{}
	}
	slog.Info("development clock is controllable via POST /dev/clock")
	return clock.NewFake(time.Now())
}

// buildMailer chooses the channel. Development writes .eml files to a directory
// instead of sending anything, so the whole notification pipeline is testable on a
// laptop with no mail server.
func buildMailer(cfg config.Config) (mailer.Mailer, error) {
	if cfg.Dev {
		m, err := mailer.NewFile(cfg.MailDir)
		if err != nil {
			return nil, err
		}
		slog.Info("development mail sink", "dir", cfg.MailDir)
		return m, nil
	}
	return mailer.NewSMTP(cfg.SMTPAddr, cfg.MailFrom), nil
}

// watchdog returns the callback the scheduler invokes each completed tick, or nil
// when the process is not running under a systemd unit with WatchdogSec.
//
// It pings every tick, with no throttle. There used to be one — suppress the ping until
// half of WatchdogSec had passed, which is what systemd's own convention suggests — but
// the callback is only ever reached when a tick completes, so the real spacing was that
// half plus a whole tick, against a deadline of the whole WatchdogSec. The shipped
// defaults left 30 seconds of margin, and an operator who raised ALMANACK_TICK to reduce
// load on a small box got a healthy server restarted every two minutes. A datagram costs
// nothing, so spacing the pings a tick apart is both simpler and four times safer.
func watchdog(tick time.Duration) func() {
	interval := sdWatchdogInterval()
	if interval <= 0 {
		return nil
	}
	// sdWatchdogInterval halves WATCHDOG_USEC by convention; the deadline systemd
	// actually enforces is the whole of it.
	deadline := 2 * interval
	// The warning is at half the deadline, not at the whole of it. Pinging once per
	// tick makes the spacing one tick only if ticks are instant; a tick that takes a
	// while pushes the next ping out by however long it took, and ticks are not
	// instant — holding an exclusive lock on the database stretched a one-second tick
	// to five. So the margin worth having is the one systemd itself assumes, which is
	// why sdWatchdogInterval halves WATCHDOG_USEC in the first place. Warning only at
	// the full deadline let ALMANACK_TICK=119s against the shipped WatchdogSec=120s
	// pass in silence and then restart-loop on the first slow tick.
	if tick > interval {
		slog.Warn("ALMANACK_TICK leaves too little room under WatchdogSec: a tick that runs long will miss the deadline and systemd will restart this service",
			"tick", tick, "watchdog_sec", deadline, "keep_tick_under", interval)
	}
	slog.Info("systemd watchdog active", "ping_every", tick, "watchdog_sec", deadline)
	return sdWatchdogPing
}
