// Command almanack is the whole application: an HTTP server, its scheduler, and the
// handful of operational subcommands a deployment needs. One binary, one database
// file, no runtime to install.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"almanack/internal/config"
	"almanack/internal/webpush"
	"almanack/web"

	_ "modernc.org/sqlite" // the pure-Go driver: no CGO, so the binary stays static

	// Embedded timezone data, used only when the operating system has none. The
	// system's tzdata still wins when present, which is what keeps DST rules current
	// without rebuilding the binary — but a container or a stripped host would
	// otherwise fail to load Europe/Paris at all, and every date in the application
	// is interpreted in the family's zone.
	_ "time/tzdata"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "almanack: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath string
		showHelp   bool
	)
	fs := flag.NewFlagSet("almanack", flag.ContinueOnError)
	fs.StringVar(&configPath, "config", "", "path to the configuration file (default: $ALMANACK_CONFIG, then "+config.DefaultPath+")")
	fs.BoolVar(&showHelp, "help", false, "show usage")
	fs.Usage = usage
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if showHelp {
		usage()
		return nil
	}

	args := fs.Args()
	command := "serve"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	// flag stops parsing at the first non-flag argument, so `almanack serve --config
	// /etc/almanack/almanack.conf` — the order an operator naturally writes, and the
	// one the "pass --config <path>" error message invites — left the flag sitting in
	// args to be silently ignored. The process then started on whatever configuration
	// it found elsewhere, or on none. Accept the flag in either position.
	path, args, err := takeConfigFlag(args)
	if err != nil {
		return err
	}
	if path != "" {
		configPath = path
	}

	if command == "version" {
		fmt.Println(versionString())
		return nil
	}
	// Generating keys must work before there is any configuration to load, since the
	// keys are one of the things the configuration will contain.
	if command == "gen-vapid" {
		return runGenVAPID()
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	setupLogging(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch command {
	case "serve":
		return runServe(ctx, cfg)
	case "bootstrap":
		return runBootstrap(ctx, cfg, args)
	case "seed":
		force := hasFlag(args, "--force", "-force")
		return runSeed(ctx, cfg, force)
	case "backup":
		dir := ""
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				dir = a
			}
		}
		prune := hasFlag(args, "--prune", "-prune")
		res, err := runBackup(ctx, cfg, dir, prune)
		if err != nil {
			return err
		}
		fmt.Printf("snapshot %s (%s) verified in %s", res.Path, humanBytes(res.Bytes), res.Elapsed.Round(1e6))
		if prune {
			fmt.Printf(", %d old snapshot(s) removed", res.Pruned)
		}
		fmt.Println()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func runGenVAPID() error {
	pub, priv, err := webpush.GenerateKeys()
	if err != nil {
		return fmt.Errorf("generate VAPID keys: %w", err)
	}
	fmt.Printf(`A new VAPID keypair. Put both values in your configuration file and store
them in your secret manager.

ALMANACK_VAPID_PUBLIC=%s
ALMANACK_VAPID_PRIVATE=%s

Generate this ONCE for a deployment and never rotate it. Every push subscription
in every family member's browser is bound to this public key: changing it silently
stops notifications on every device until each person reinstalls the app.
`, pub, priv)
	return nil
}

func setupLogging(cfg config.Config) {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if strings.EqualFold(cfg.LogFormat, "json") {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))
}

// assets returns the embedded browser application.
func assets() (fs.FS, error) { return web.FS(), nil }

func versionString() string {
	v := version
	if v == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					v = s.Value[:7]
				}
			}
		}
	}
	return "almanack " + v
}

// takeConfigFlag pulls a --config out of a command's arguments, in either the
// `--config path` or the `--config=path` spelling, and returns what is left for the
// command's own parsing.
func takeConfigFlag(args []string) (string, []string, error) {
	rest := make([]string, 0, len(args))
	path := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(a, "=")
		if name != "--config" && name != "-config" {
			rest = append(rest, a)
			continue
		}
		if hasValue {
			path = value
			continue
		}
		if i+1 >= len(args) {
			return "", nil, fmt.Errorf("%s needs a path", a)
		}
		path = args[i+1]
		i++
	}
	return path, rest, nil
}

func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func usage() {
	fmt.Fprint(os.Stderr, `almanack — a shared calendar with a ten-year shelf life

Usage:
  almanack [--config <path>] <command>

Commands:
  serve                 Run the server (the default)
  bootstrap --email <address> --name <name> [--calendar <name>]
                        Create the first account and calendar on an empty database,
                        and print an invite link for the rest of the family. Signup
                        is invite-only, so this is the way in on a fresh install.
  seed [--force]        Fill the database with a demo family, for development
  backup <dir> [--prune]
                        Take a verified snapshot; --prune applies the retention
                        policy from the configuration. Exits non-zero if the
                        snapshot fails its integrity check.
  gen-vapid             Generate a VAPID keypair. Run once per deployment.
  version               Print the version

Configuration is read from --config, then $ALMANACK_CONFIG, then `+config.DefaultPath+`,
with environment variables overriding the file. See almanack.conf.example for every
setting.
`)
}
