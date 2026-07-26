package main

import (
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Minimal sd_notify support, written out rather than imported.
//
// The protocol is a datagram of "KEY=value" to the socket named in NOTIFY_SOCKET —
// about twenty lines of standard library. Taking a dependency for that would be a
// poor trade in a project whose whole argument is that every dependency is a
// maintenance obligation for the next twenty years.
//
// Two things depend on this working. READY=1 is sent only after migrations finish,
// so a deployment health check can distinguish "still migrating" from "dead". And the
// watchdog ping is driven by the scheduler loop rather than by a timer of its own, so
// a scheduler that wedges stops the pings and gets the process restarted — the
// alternative being a process that answers HTTP cheerfully for weeks while silently
// sending no reminders at all.

func sdNotify(state string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return // not running under systemd; nothing to tell
	}
	// A leading '@' denotes an abstract socket, whose name starts with a NUL byte.
	name := socket
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		slog.Warn("sd_notify: cannot reach the notify socket", "error", err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		slog.Warn("sd_notify: write failed", "state", state, "error", err)
	}
}

// sdReady announces that startup is complete, including migrations.
func sdReady() { sdNotify("READY=1") }

// sdStopping announces a clean shutdown so systemd does not treat it as a failure.
func sdStopping() { sdNotify("STOPPING=1") }

// sdStatus sets the one-line status shown by `systemctl status`.
func sdStatus(text string) { sdNotify("STATUS=" + text) }

// sdWatchdogInterval returns how often the watchdog must be fed, or zero when the
// unit has no WatchdogSec. systemd's convention is to ping at half the interval.
func sdWatchdogInterval() time.Duration {
	usec := os.Getenv("WATCHDOG_USEC")
	if usec == "" {
		return 0
	}
	// Only the process systemd started should feed the watchdog.
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" {
		if p, err := strconv.Atoi(pid); err == nil && p != os.Getpid() {
			return 0
		}
	}
	n, err := strconv.ParseInt(usec, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Microsecond / 2
}

// sdWatchdogPing is handed to the scheduler, which calls it once per completed tick.
func sdWatchdogPing() { sdNotify("WATCHDOG=1") }
