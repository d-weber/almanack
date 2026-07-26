package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"agenda/internal/clock"
	"agenda/internal/config"
	"agenda/internal/events"
	"agenda/internal/httpapi"
	"agenda/internal/i18n"
	"agenda/internal/mailer"
	"agenda/internal/notify"
	"agenda/internal/store"
	"agenda/internal/webpush"
)

// runServe wires everything together and runs until the context is cancelled.
//
// The ordering here is deliberate. Migrations finish before READY is signalled, so a
// deployment health check can tell "still migrating" from "dead". The scheduler's
// catch-up runs before the first tick, so an outage cannot leave a hole in the
// reminders. And the watchdog is fed by the scheduler rather than by a timer of its
// own, so a wedged scheduler restarts the process instead of leaving a server that
// answers HTTP cheerfully while sending nothing.
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
		slog.Warn("no VAPID keys configured: push notifications are disabled, email only (run `agenda gen-vapid`)")
	}

	eventSvc := events.New(st, cfg.FamilyTZ, clk)

	notifier, err := notify.New(notify.Options{
		Store: st, Events: eventSvc, Push: sender, Mailer: mail, Catalog: catalog,
		Clock: clk, Location: cfg.FamilyTZ, BaseURL: cfg.BaseURL,
		Horizon: cfg.PlanHorizon, Tick: cfg.SchedulerTick,
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
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Migrations are done and the router is built: the service is genuinely ready.
	sdReady()
	sdStatus("serving on " + cfg.ListenAddr)
	slog.Info("ready", "listen", cfg.ListenAddr, "base_url", cfg.BaseURL,
		"timezone", cfg.TZName, "app_version", srv.AppVersion(), "dev_mode", cfg.Dev)
	if cfg.Dev {
		slog.Info("development mode", "url", cfg.BaseURL, "dev_tools", cfg.BaseURL+"/dev/", "mail", cfg.MailDir)
	}

	errs := make(chan error, 2)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server: %w", err)
		}
	}()

	go func() {
		// Run performs the boot catch-up before its first tick.
		if err := notifier.Run(ctx, watchdog()); err != nil {
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
func watchdog() func() {
	interval := sdWatchdogInterval()
	if interval <= 0 {
		return nil
	}
	slog.Info("systemd watchdog active", "ping_interval", interval)

	var last time.Time
	return func() {
		// The scheduler ticks more often than the watchdog needs feeding.
		if time.Since(last) < interval {
			return
		}
		last = time.Now()
		sdWatchdogPing()
	}
}
