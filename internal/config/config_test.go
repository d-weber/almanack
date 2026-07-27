package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Configuration is the one part of this application that is edited by hand, at 3am,
// over SSH, by someone who did not write it. So these tests care as much about what
// the errors say as about whether an error happens: "configuration problems" that
// does not name the offending setting costs an hour, and a setting that silently
// falls back to its default costs a wrong-timezone calendar nobody thinks to blame
// on the config file.
//
// The tests are in-package deliberately. `known` is the parser's own list of settings
// and is unexported; reading it directly is what makes the cross-check against
// almanack.conf.example possible without a second, hand-copied list to drift.

var examplePath = filepath.Join("..", "..", "almanack.conf.example")

// isolateEnv removes every ALMANACK_* variable for the duration of the test.
// Load reads the ambient environment, so without this a maintainer who happens to
// export ALMANACK_TZ gets failures in tests that never mention timezones.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "ALMANACK_") {
			continue
		}
		os.Unsetenv(key)
		t.Cleanup(func() { os.Setenv(key, value) })
	}
}

// requireNoSystemConfig skips tests that rely on there being no configuration file
// at all: on a machine that really is running Almanack, Load would read DefaultPath
// and the test would be measuring that machine rather than the code.
func requireNoSystemConfig(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(DefaultPath); err == nil {
		t.Skipf("%s exists on this machine, so Load would read it", DefaultPath)
	}
}

func writeConf(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "almanack.conf")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// minimalProd is the smallest configuration a non-dev server is allowed to start
// with, so that a test about one setting is not drowned in complaints about seven
// others. Every line here is a setting validate() insists on outside dev mode.
func minimalProd(extra ...string) []string {
	return append([]string{
		"ALMANACK_BASE_URL=https://almanack.example.org",
		"ALMANACK_DATA=/var/lib/almanack/almanack.db",
		"ALMANACK_MAIL_FROM=almanack@example.org",
		"ALMANACK_OWNER_EMAIL=you@example.org",
		"ALMANACK_VAPID_PUBLIC=BJxBoRCV9nqPublicKeyMaterial",
		"ALMANACK_VAPID_PRIVATE=q1dXpw3UpT5PrivateKeyMaterial",
		"ALMANACK_VAPID_SUBJECT=mailto:you@example.org",
	}, extra...)
}

// overriding is minimalProd with one setting given a different value, replaced in
// place rather than repeated, so a test about one value does not quietly depend on
// which of two duplicate lines wins.
func overriding(key, value string) []string {
	lines := minimalProd()
	for i, line := range lines {
		if strings.HasPrefix(line, key+"=") {
			lines[i] = key + "=" + value
			return lines
		}
	}
	return append(lines, key+"="+value)
}

func loadConf(t *testing.T, lines ...string) (Config, error) {
	t.Helper()
	return Load(writeConf(t, lines...))
}

// wantErrMentioning fails unless the error exists and names each of the given
// strings. The key name is the whole value of a configuration error: an operator
// who is told "not a duration" without being told which setting has to bisect the
// file by hand.
func wantErrMentioning(t *testing.T, err error, mentions ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("Load succeeded; want a startup error")
	}
	for _, m := range mentions {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error does not mention %q, so it does not tell the operator what to fix:\n%v", m, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

// almanack.conf.example presents these values as "the default", which makes them a
// promise: omit the line and you get exactly this. A default that drifts is a
// configuration change nobody deployed, nobody reviewed, and nobody can see in a diff.
func TestLoadDefaults(t *testing.T) {
	isolateEnv(t)

	cfg, err := loadConf(t, minimalProd()...)
	if err != nil {
		t.Fatalf("Load of a minimal production config: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"ALMANACK_DEV", cfg.Dev, false},
		{"ALMANACK_LISTEN", cfg.ListenAddr, "127.0.0.1:8080"},
		{"ALMANACK_TZ", cfg.TZName, "Europe/Paris"},
		{"ALMANACK_TRUSTED_PROXIES", strings.Join(cfg.TrustedProxies, ","), "127.0.0.1,::1"},
		{"ALMANACK_SMTP", cfg.SMTPAddr, "127.0.0.1:25"},
		{"ALMANACK_ALSACE_MOSELLE", cfg.AlsaceMoselle, false},
		{"ALMANACK_HOLIDAY_COLOR", cfg.HolidayColor, "#d32f2f"},
		{"ALMANACK_SOURCE_URL", cfg.SourceURL, ""},
		{"ALMANACK_PLAN_HORIZON", cfg.PlanHorizon, 48 * time.Hour},
		{"ALMANACK_TICK", cfg.SchedulerTick, 30 * time.Second},
		{"ALMANACK_HEARTBEAT_TIME", cfg.HeartbeatTime, "08:00"},
		{"ALMANACK_BACKUP_KEEP_HOURLY", cfg.KeepHourly, 48},
		{"ALMANACK_BACKUP_KEEP_DAILY", cfg.KeepDaily, 14},
		{"ALMANACK_BACKUP_KEEP_WEEKLY", cfg.KeepWeekly, 8},
		{"ALMANACK_BACKUP_KEEP_MONTHLY", cfg.KeepMonthly, 24},
		{"ALMANACK_LOG_LEVEL", cfg.LogLevel, "info"},
		{"ALMANACK_LOG_FORMAT", cfg.LogFormat, "text"},
		// Not a constant but a derivation: snapshots land beside the database, so a
		// deployment that never sets it still has somewhere to put them.
		{"ALMANACK_BACKUP_DIR", cfg.BackupDir, filepath.Join("/var/lib/almanack", "backups")},
		// Dev-only, and it must stay empty in production: a non-empty MailDir is what
		// switches the mailer to the file sink instead of the MTA.
		{"ALMANACK_MAIL_DIR", cfg.MailDir, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s defaulted to %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}

	// FamilyTZ is what every date bucket in the application is computed in, so a
	// Config that validated without one would fail much later and much less clearly.
	if cfg.FamilyTZ == nil {
		t.Fatal("FamilyTZ is nil after a successful Load")
	}
	if cfg.FamilyTZ.String() != "Europe/Paris" {
		t.Errorf("FamilyTZ = %q, want Europe/Paris", cfg.FamilyTZ)
	}
}

// A value set in the file must beat the built-in default, and a value in the
// environment must beat the file. That order is the documented contract (package
// doc, and the header of almanack.conf.example); an EnvironmentFile deployment and
// a --config deployment both depend on it.
func TestPrecedence(t *testing.T) {
	isolateEnv(t)

	path := writeConf(t, minimalProd("ALMANACK_TZ=Europe/Berlin", "ALMANACK_LOG_LEVEL=warn")...)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TZName != "Europe/Berlin" {
		t.Errorf("file value ignored: TZName = %q, want Europe/Berlin", cfg.TZName)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("file value ignored: LogLevel = %q, want warn", cfg.LogLevel)
	}

	t.Setenv("ALMANACK_TZ", "Europe/Lisbon")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TZName != "Europe/Lisbon" {
		t.Errorf("environment did not override the file: TZName = %q, want Europe/Lisbon", cfg.TZName)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("overriding one setting disturbed another: LogLevel = %q, want warn", cfg.LogLevel)
	}
}

// An explicit --config path must win over a stale exported ALMANACK_CONFIG. The
// same class of bug as TestTakeConfigFlag in cmd/almanack: the server comes up on a
// configuration other than the one the operator named, and nothing says so.
func TestExplicitPathBeatsEnvironment(t *testing.T) {
	isolateEnv(t)

	wanted := writeConf(t, minimalProd("ALMANACK_TZ=Europe/Berlin")...)
	t.Setenv("ALMANACK_CONFIG", writeConf(t, minimalProd("ALMANACK_TZ=Europe/Lisbon")...))

	cfg, err := Load(wanted)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConfigPath != wanted {
		t.Errorf("ConfigPath = %q, want the explicitly named %q", cfg.ConfigPath, wanted)
	}
	if cfg.TZName != "Europe/Berlin" {
		t.Errorf("read the wrong file: TZName = %q, want Europe/Berlin", cfg.TZName)
	}
}

// With no path given, ALMANACK_CONFIG selects the file. This is the systemd path:
// the unit sets the variable in an EnvironmentFile and never passes a flag.
func TestEmptyPathFallsBackToEnvironment(t *testing.T) {
	isolateEnv(t)

	path := writeConf(t, minimalProd("ALMANACK_TZ=Europe/Berlin")...)
	t.Setenv("ALMANACK_CONFIG", path)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ConfigPath != path {
		t.Errorf("ConfigPath = %q, want %q", cfg.ConfigPath, path)
	}
	if cfg.TZName != "Europe/Berlin" {
		t.Errorf("TZName = %q, want Europe/Berlin", cfg.TZName)
	}
}

// ---------------------------------------------------------------------------
// Strictness — 0.2.0 made a misspelling an error instead of a silent default
// ---------------------------------------------------------------------------

// ALMANACK_TZZ=Europe/Paris used to be ignored, which books every event in the
// wrong timezone and looks like an application bug for as long as it takes someone
// to reread the file character by character.
func TestUnknownKeyIsRejectedByName(t *testing.T) {
	isolateEnv(t)

	_, err := loadConf(t, minimalProd("ALMANACK_TZZ=Europe/Paris")...)
	wantErrMentioning(t, err, "ALMANACK_TZZ", "almanack.conf.example")
}

// A file with two typos must report both. An operator fixing configuration over SSH
// restarts the service once per problem, and each restart is a minute of downtime.
func TestAllProblemsAreReportedTogether(t *testing.T) {
	isolateEnv(t)

	_, err := loadConf(t, minimalProd(
		"ALMANACK_TZZ=Europe/Paris",
		"ALMANACK_TICK=soon",
		"ALMANACK_LOG_LEVEL=chatty",
	)...)
	wantErrMentioning(t, err, "ALMANACK_TZZ", "ALMANACK_TICK", "ALMANACK_LOG_LEVEL")
}

// The file is also usable as a systemd EnvironmentFile, which may legitimately carry
// unrelated variables. Only the ALMANACK_ namespace is a closed set; rejecting the
// rest would make one file impossible to use for both purposes.
func TestForeignKeysAreLeftAlone(t *testing.T) {
	isolateEnv(t)

	if _, err := loadConf(t, minimalProd("PATH=/usr/local/bin:/usr/bin", "TZ=UTC")...); err != nil {
		t.Fatalf("a non-ALMANACK key was treated as a problem: %v", err)
	}
}

// A value that does not parse must name the setting and show what was rejected.
// ALMANACK_ALSACE_MOSELLE=yes is the case that motivated this: the natural spelling
// of "true" quietly switched the two extra public holidays back off.
func TestUnparseableValuesNameTheSetting(t *testing.T) {
	isolateEnv(t)

	cases := []struct {
		name     string
		line     string
		mentions []string
	}{
		{"bool", "ALMANACK_ALSACE_MOSELLE=yes", []string{"ALMANACK_ALSACE_MOSELLE", `"yes"`, "true"}},
		{"bool/dev", "ALMANACK_DEV=on", []string{"ALMANACK_DEV", `"on"`}},
		{"duration", "ALMANACK_TICK=30", []string{"ALMANACK_TICK", `"30"`, "duration"}},
		{"duration/horizon", "ALMANACK_PLAN_HORIZON=2 days", []string{"ALMANACK_PLAN_HORIZON", "duration"}},
		{"int", "ALMANACK_BACKUP_KEEP_HOURLY=lots", []string{"ALMANACK_BACKUP_KEEP_HOURLY", `"lots"`, "whole number"}},
		{"int/daily", "ALMANACK_BACKUP_KEEP_DAILY=14.5", []string{"ALMANACK_BACKUP_KEEP_DAILY", "whole number"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConf(t, minimalProd(tc.line)...)
			wantErrMentioning(t, err, tc.mentions...)
		})
	}
}

// The spellings of true that operators and the Makefile actually use. `make dev`
// ships ALMANACK_DEV=1, so if strconv.ParseBool were ever swapped for a comparison
// against "true" the whole development environment would silently become production.
func TestBoolSpellings(t *testing.T) {
	isolateEnv(t)

	for _, spelling := range []string{"true", "TRUE", "True", "1", "t"} {
		t.Run(spelling, func(t *testing.T) {
			cfg, err := loadConf(t, minimalProd("ALMANACK_ALSACE_MOSELLE="+spelling)...)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !cfg.AlsaceMoselle {
				t.Errorf("ALMANACK_ALSACE_MOSELLE=%s did not enable the setting", spelling)
			}
		})
	}
	for _, spelling := range []string{"false", "FALSE", "0", "f"} {
		t.Run(spelling, func(t *testing.T) {
			cfg, err := loadConf(t, minimalProd("ALMANACK_ALSACE_MOSELLE="+spelling)...)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AlsaceMoselle {
				t.Errorf("ALMANACK_ALSACE_MOSELLE=%s did not disable the setting", spelling)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Required settings and validation
// ---------------------------------------------------------------------------

// Each of these is required outside dev mode, and each has to fail by name. A
// server that starts without ALMANACK_OWNER_EMAIL is a server where nothing fails
// loudly ever again, which is worse than one that refuses to start.
func TestRequiredSettings(t *testing.T) {
	isolateEnv(t)

	cases := []struct {
		name     string
		omit     string
		mentions []string
	}{
		{"data", "ALMANACK_DATA", []string{"ALMANACK_DATA", "required"}},
		{"base url", "ALMANACK_BASE_URL", []string{"ALMANACK_BASE_URL", "required"}},
		{"mail from", "ALMANACK_MAIL_FROM", []string{"ALMANACK_MAIL_FROM", "required"}},
		{"owner email", "ALMANACK_OWNER_EMAIL", []string{"ALMANACK_OWNER_EMAIL", "required"}},
		{"vapid public", "ALMANACK_VAPID_PUBLIC", []string{"ALMANACK_VAPID_PUBLIC", "gen-vapid"}},
		{"vapid private", "ALMANACK_VAPID_PRIVATE", []string{"ALMANACK_VAPID_PRIVATE", "gen-vapid"}},
		{"vapid subject", "ALMANACK_VAPID_SUBJECT", []string{"ALMANACK_VAPID_SUBJECT", "required"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			for _, line := range minimalProd() {
				if key, _, _ := strings.Cut(line, "="); key != tc.omit {
					lines = append(lines, line)
				}
			}
			// Guards the test, not the code: a renamed setting in minimalProd would
			// otherwise turn every case here into a tautology that omits nothing.
			if len(lines) != len(minimalProd())-1 {
				t.Fatalf("%s is not in minimalProd, so this case omits nothing", tc.omit)
			}
			_, err := loadConf(t, lines...)
			wantErrMentioning(t, err, tc.mentions...)
		})
	}
}

// docs/deployment.md promises this in as many words: PWA installation and Web Push
// both refuse an insecure origin, so an http:// base URL produces an application
// that installs nowhere and notifies nobody. Catching it at startup is the whole
// point — the symptom otherwise appears days later on someone's phone.
func TestBaseURLMustBeHTTPSOutsideDev(t *testing.T) {
	isolateEnv(t)

	cases := []struct {
		name string
		url  string
	}{
		{"plain http", "http://almanack.example.org"},
		{"http on localhost", "http://localhost:8080"},
		{"scheme omitted", "almanack.example.org"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConf(t, overriding("ALMANACK_BASE_URL", tc.url)...)
			wantErrMentioning(t, err, "ALMANACK_BASE_URL", "https")
		})
	}
}

// The base URL is concatenated with paths to build invite and password-reset links,
// so a trailing slash left in by a copy-paste would produce https://host//invite/…
// — which some proxies and some mail clients treat as a different URL.
func TestBaseURLTrailingSlashIsTrimmed(t *testing.T) {
	isolateEnv(t)

	cfg, err := loadConf(t, overriding("ALMANACK_BASE_URL", "https://almanack.example.org/")...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseURL != "https://almanack.example.org" {
		t.Errorf("BaseURL = %q, want the trailing slash removed", cfg.BaseURL)
	}
}

// A timezone that does not exist has to be fatal. Every date bucket in the
// application — "today", digest membership, occurrence identity — is computed in
// this zone, so falling back to UTC would put the whole family's calendar an hour
// out for half the year without a single error in the log.
func TestUnknownTimezoneIsFatal(t *testing.T) {
	isolateEnv(t)

	_, err := loadConf(t, minimalProd("ALMANACK_TZ=Europe/Atlantis")...)
	wantErrMentioning(t, err, "ALMANACK_TZ", "Europe/Atlantis")
}

// The remaining validations, each of which protects something that would otherwise
// fail far from its cause: a colour the browser cannot parse, a log level slog does
// not know, a scheduler that never ticks, a heartbeat that never fires.
func TestValueValidation(t *testing.T) {
	isolateEnv(t)

	cases := []struct {
		name     string
		line     string
		mentions []string
	}{
		{"holiday colour without a hash", "ALMANACK_HOLIDAY_COLOR=d32f2f", []string{"ALMANACK_HOLIDAY_COLOR", "hex"}},
		{"holiday colour by name", "ALMANACK_HOLIDAY_COLOR=red", []string{"ALMANACK_HOLIDAY_COLOR", "hex"}},
		{"holiday colour shorthand", "ALMANACK_HOLIDAY_COLOR=#d32", []string{"ALMANACK_HOLIDAY_COLOR", "hex"}},
		{"holiday colour not hex", "ALMANACK_HOLIDAY_COLOR=#gggggg", []string{"ALMANACK_HOLIDAY_COLOR", "hex"}},
		{"log level", "ALMANACK_LOG_LEVEL=verbose", []string{"ALMANACK_LOG_LEVEL", "debug, info, warn or error"}},
		{"log format", "ALMANACK_LOG_FORMAT=logfmt", []string{"ALMANACK_LOG_FORMAT", "text or json"}},
		{"zero horizon", "ALMANACK_PLAN_HORIZON=0s", []string{"ALMANACK_PLAN_HORIZON", "positive"}},
		{"negative horizon", "ALMANACK_PLAN_HORIZON=-1h", []string{"ALMANACK_PLAN_HORIZON", "positive"}},
		{"zero tick", "ALMANACK_TICK=0s", []string{"ALMANACK_TICK", "positive"}},
		{"negative tick", "ALMANACK_TICK=-30s", []string{"ALMANACK_TICK", "positive"}},
		{"heartbeat without a colon", "ALMANACK_HEARTBEAT_TIME=0800", []string{"ALMANACK_HEARTBEAT_TIME", "HH:MM"}},
		{"heartbeat single digit", "ALMANACK_HEARTBEAT_TIME=8:00", []string{"ALMANACK_HEARTBEAT_TIME", "HH:MM"}},
		{"heartbeat hour out of range", "ALMANACK_HEARTBEAT_TIME=25:00", []string{"ALMANACK_HEARTBEAT_TIME", "HH:MM"}},
		{"heartbeat minute out of range", "ALMANACK_HEARTBEAT_TIME=08:75", []string{"ALMANACK_HEARTBEAT_TIME", "HH:MM"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConf(t, minimalProd(tc.line)...)
			wantErrMentioning(t, err, tc.mentions...)
		})
	}
}

// The values on the other side of those boundaries have to be accepted, or the
// validation is just a different way of refusing to start.
func TestValidValuesAreAccepted(t *testing.T) {
	isolateEnv(t)

	cases := []struct{ name, line string }{
		{"uppercase log level", "ALMANACK_LOG_LEVEL=WARN"},
		{"json logs", "ALMANACK_LOG_FORMAT=json"},
		{"uppercase hex colour", "ALMANACK_HOLIDAY_COLOR=#D32F2F"},
		{"midnight heartbeat", "ALMANACK_HEARTBEAT_TIME=00:00"},
		{"last minute heartbeat", "ALMANACK_HEARTBEAT_TIME=23:59"},
		{"zero retention", "ALMANACK_BACKUP_KEEP_HOURLY=0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadConf(t, minimalProd(tc.line)...); err != nil {
				t.Errorf("%s was rejected: %v", tc.line, err)
			}
		})
	}
}

// Which file the problems came from, or that there was no file at all. Two config
// files on one host is a normal accident — a package default and an Ansible-managed
// one — and an error that does not say which was read cannot be acted on.
func TestErrorSaysWhereItRead(t *testing.T) {
	isolateEnv(t)

	path := writeConf(t, "ALMANACK_LOG_LEVEL=verbose")
	_, err := Load(path)
	wantErrMentioning(t, err, path)

	requireNoSystemConfig(t)
	_, err = Load("")
	wantErrMentioning(t, err, "No configuration file was read", "--config", DefaultPath, "almanack.conf.example")
}

// ---------------------------------------------------------------------------
// Dev mode
// ---------------------------------------------------------------------------

// Dev mode fills in everything a developer would otherwise have to write out, which
// is what lets `make dev` start with four environment variables and no file at all.
// Losing that turns a clone-and-run into a scavenger hunt through the source.
func TestDevModeFillsInTheRest(t *testing.T) {
	isolateEnv(t)
	requireNoSystemConfig(t)

	t.Setenv("ALMANACK_DEV", "1")
	t.Setenv("ALMANACK_LISTEN", "127.0.0.1:8080")
	t.Setenv("ALMANACK_TZ", "Europe/Paris")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("dev mode refused to start without a configuration file: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"dev", cfg.Dev, true},
		{"data", cfg.DataPath, filepath.Join("devdata", "almanack.db")},
		{"backup dir", cfg.BackupDir, filepath.Join("devdata", "backups")},
		{"mail dir", cfg.MailDir, filepath.Join("devdata", "mail")},
		{"mail from", cfg.MailFrom, "almanack@localhost"},
		{"owner email", cfg.OwnerEmail, "owner@localhost"},
		// RFC 8292 wants a contact on every push; dev derives one rather than
		// making the developer invent an address.
		{"vapid subject", cfg.VAPIDSubject, "mailto:owner@localhost"},
		// 127.0.0.1 becomes localhost because a secure-context browser treats the
		// name, not the literal, as trustworthy for service workers.
		{"base url", cfg.BaseURL, "http://localhost:8080"},
		// No file was read, and the config has to say so rather than imply one.
		{"config path", cfg.ConfigPath, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("dev %s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

// Dev must not inherit production's requirements, or the local http://localhost
// origin and the absent VAPID keypair would both be startup failures.
func TestDevModeRelaxesProductionRequirements(t *testing.T) {
	isolateEnv(t)

	if _, err := loadConf(t,
		"ALMANACK_DEV=true",
		"ALMANACK_BASE_URL=http://localhost:8080",
		"ALMANACK_DATA=/tmp/dev.db",
	); err != nil {
		t.Fatalf("dev mode applied a production requirement: %v", err)
	}
}

// Explicit settings must survive dev mode's filling-in, or a developer pointing at
// a scratch database quietly writes to the shared one instead.
func TestDevModeDoesNotOverrideExplicitValues(t *testing.T) {
	isolateEnv(t)

	cfg, err := loadConf(t,
		"ALMANACK_DEV=true",
		"ALMANACK_DATA=/tmp/scratch/other.db",
		"ALMANACK_BASE_URL=http://127.0.0.1:9999",
		"ALMANACK_MAIL_FROM=me@example.org",
		"ALMANACK_MAIL_DIR=/tmp/scratch/outbox",
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"data", cfg.DataPath, "/tmp/scratch/other.db"},
		{"base url", cfg.BaseURL, "http://127.0.0.1:9999"},
		{"mail from", cfg.MailFrom, "me@example.org"},
		{"mail dir", cfg.MailDir, "/tmp/scratch/outbox"},
		{"backup dir follows the data path", cfg.BackupDir, filepath.Join("/tmp/scratch", "backups")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The shipped example file
// ---------------------------------------------------------------------------

// almanack.conf.example is what every new install starts from, and docs/install.md
// tells operators to copy it. If it stops parsing, or names a setting this version
// no longer accepts, the first thing a new deployment does is fail to start.
func TestShippedExampleLoads(t *testing.T) {
	isolateEnv(t)

	// The two secrets the example deliberately ships empty; `almanack gen-vapid`
	// produces them, so supply them here rather than pretending the file is complete.
	t.Setenv("ALMANACK_VAPID_PUBLIC", "BJxBoRCV9nqPublicKeyMaterial")
	t.Setenv("ALMANACK_VAPID_PRIVATE", "q1dXpw3UpT5PrivateKeyMaterial")

	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("almanack.conf.example does not load; every new install starts here:\n%v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"base url", cfg.BaseURL, "https://almanack.example.org"},
		{"data", cfg.DataPath, "/var/lib/almanack/almanack.db"},
		{"backup dir", cfg.BackupDir, "/var/lib/almanack/backups"},
		{"tz", cfg.TZName, "Europe/Paris"},
		// Written as #d32f2f, on a line that a comment-stripping parser would eat.
		{"holiday colour", cfg.HolidayColor, "#d32f2f"},
		// The commented-out development section must stay commented out: an example
		// that ships ALMANACK_DEV=true would drop Secure from the session cookie.
		{"dev", cfg.Dev, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("example %s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

// exemptFromExample records the keys that legitimately do not appear in
// almanack.conf.example. Anything else missing is drift.
var exemptFromExample = map[string]string{
	// Chicken and egg: this one names the file, so it cannot be set inside it.
	// `almanack --help` and cmd/almanack's usage text document it instead.
	"ALMANACK_CONFIG": "selects the configuration file, so it cannot live in it",
}

// The cross-check. `known` is the set of settings the parser accepts; the example
// file is the set an operator can discover. When those two drift apart the failure
// is silent in both directions: a new setting nobody knows to set, or a setting the
// documentation still advertises that nothing reads. Version 0.2.0 had to fix two
// of the latter (ALMANACK_OWNER_EMAIL and ALMANACK_HEARTBEAT_TIME were required and
// read by nothing), which is why this is a test and not a review habit.
func TestExampleAndParserAgreeOnEveryKey(t *testing.T) {
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read almanack.conf.example: %v", err)
	}

	// Comments count as documentation: ALMANACK_DEV and ALMANACK_MAIL_DIR are
	// deliberately shown commented out, and prose in the file names settings too.
	documented := map[string]bool{}
	for _, key := range regexp.MustCompile(`ALMANACK_[A-Z0-9_]+`).FindAllString(string(data), -1) {
		documented[key] = true
	}
	if len(documented) == 0 {
		t.Fatal("found no ALMANACK_* keys in almanack.conf.example; the scan is broken, not the file")
	}

	for key := range known {
		if documented[key] || exemptFromExample[key] != "" {
			continue
		}
		t.Errorf("%s is accepted by the parser but appears nowhere in almanack.conf.example.\n"+
			"The example claims to list every setting the application has, so an operator has\n"+
			"no way to find this one. Document it there, or add it to exemptFromExample with\n"+
			"the reason, in the same commit.", key)
	}

	for key := range documented {
		if known[key] {
			continue
		}
		t.Errorf("almanack.conf.example names %s, which this version does not accept.\n"+
			"Setting it in a real configuration file is now a startup error, so the example\n"+
			"is telling operators to break their install. Remove it from the example, or add\n"+
			"it back to `known` if it was dropped by accident.", key)
	}
}

// The example's header promises "Values shown are the defaults unless marked
// REQUIRED", which is the line operators read to decide whether they can leave a
// setting alone. Loading the example must therefore produce exactly the same values
// as loading a file that sets none of them.
func TestExampleShowsTheRealDefaults(t *testing.T) {
	isolateEnv(t)

	t.Setenv("ALMANACK_VAPID_PUBLIC", "BJxBoRCV9nqPublicKeyMaterial")
	t.Setenv("ALMANACK_VAPID_PRIVATE", "q1dXpw3UpT5PrivateKeyMaterial")

	fromExample, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load(almanack.conf.example): %v", err)
	}
	fromDefaults, err := loadConf(t, minimalProd()...)
	if err != nil {
		t.Fatalf("Load of a minimal config: %v", err)
	}

	// Only the settings the promise covers. The rest — paths, hostnames, addresses,
	// the VAPID pair — are site-specific or REQUIRED and have no default to agree with.
	cases := []struct {
		key string
		of  func(Config) any
	}{
		{"ALMANACK_LISTEN", func(c Config) any { return c.ListenAddr }},
		{"ALMANACK_TZ", func(c Config) any { return c.TZName }},
		{"ALMANACK_ALSACE_MOSELLE", func(c Config) any { return c.AlsaceMoselle }},
		{"ALMANACK_HOLIDAY_COLOR", func(c Config) any { return c.HolidayColor }},
		{"ALMANACK_SOURCE_URL", func(c Config) any { return c.SourceURL }},
		{"ALMANACK_TRUSTED_PROXIES", func(c Config) any { return strings.Join(c.TrustedProxies, ",") }},
		{"ALMANACK_SMTP", func(c Config) any { return c.SMTPAddr }},
		{"ALMANACK_HEARTBEAT_TIME", func(c Config) any { return c.HeartbeatTime }},
		{"ALMANACK_PLAN_HORIZON", func(c Config) any { return c.PlanHorizon }},
		{"ALMANACK_TICK", func(c Config) any { return c.SchedulerTick }},
		{"ALMANACK_BACKUP_KEEP_HOURLY", func(c Config) any { return c.KeepHourly }},
		{"ALMANACK_BACKUP_KEEP_DAILY", func(c Config) any { return c.KeepDaily }},
		{"ALMANACK_BACKUP_KEEP_WEEKLY", func(c Config) any { return c.KeepWeekly }},
		{"ALMANACK_BACKUP_KEEP_MONTHLY", func(c Config) any { return c.KeepMonthly }},
		{"ALMANACK_LOG_LEVEL", func(c Config) any { return c.LogLevel }},
		{"ALMANACK_LOG_FORMAT", func(c Config) any { return c.LogFormat }},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got, want := tc.of(fromExample), tc.of(fromDefaults); got != want {
				t.Errorf("almanack.conf.example shows %s as %v, but the built-in default is %v.\n"+
					"One of the two moved without the other; operators who left the line alone\n"+
					"and operators who deleted it now get different servers.", tc.key, got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ParseFile — the systemd EnvironmentFile subset
// ---------------------------------------------------------------------------

// The format is systemd's precisely so that one templated file can serve as an
// EnvironmentFile and as --config. Every case here is something systemd tolerates
// and an Ansible template plausibly emits.
func TestParseFile(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  map[string]string
	}{
		{"plain", []string{"ALMANACK_TZ=Europe/Paris"}, map[string]string{"ALMANACK_TZ": "Europe/Paris"}},
		{"blank lines and comments", []string{"# a comment", "", "   ", "ALMANACK_TZ=UTC"},
			map[string]string{"ALMANACK_TZ": "UTC"}},
		{"indented comment", []string{"   # indented", "ALMANACK_TZ=UTC"}, map[string]string{"ALMANACK_TZ": "UTC"}},
		{"double quotes", []string{`ALMANACK_MAIL_FROM="a@example.org"`},
			map[string]string{"ALMANACK_MAIL_FROM": "a@example.org"}},
		{"single quotes", []string{`ALMANACK_MAIL_FROM='a@example.org'`},
			map[string]string{"ALMANACK_MAIL_FROM": "a@example.org"}},
		// So that the same file can also be sourced by a shell script.
		{"export prefix", []string{"export ALMANACK_TZ=UTC"}, map[string]string{"ALMANACK_TZ": "UTC"}},
		{"surrounding whitespace", []string{"  ALMANACK_TZ = UTC  "}, map[string]string{"ALMANACK_TZ": "UTC"}},
		// The hash is not a comment introducer mid-line: ALMANACK_HOLIDAY_COLOR is
		// written #d32f2f, and a parser that stripped from the first # would leave it empty.
		{"hash inside a value", []string{"ALMANACK_HOLIDAY_COLOR=#d32f2f"},
			map[string]string{"ALMANACK_HOLIDAY_COLOR": "#d32f2f"}},
		// Passwords and base64 keys contain =; only the first one separates.
		{"equals inside a value", []string{"ALMANACK_VAPID_PUBLIC=BJxBoR=="},
			map[string]string{"ALMANACK_VAPID_PUBLIC": "BJxBoR=="}},
		{"empty value", []string{"ALMANACK_SOURCE_URL="}, map[string]string{"ALMANACK_SOURCE_URL": ""}},
		{"later line wins", []string{"ALMANACK_TZ=UTC", "ALMANACK_TZ=Europe/Paris"},
			map[string]string{"ALMANACK_TZ": "Europe/Paris"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFile(writeConf(t, tc.lines...))
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parsed %v, want %v", got, tc.want)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("%s = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

// A malformed file must name the line. Configuration files are templated, and a
// template that emits a stray line produces a file nobody has read with their own
// eyes; "expected KEY=VALUE" without a line number is a search.
func TestParseFileRejectsMalformedLines(t *testing.T) {
	cases := []struct {
		name     string
		lines    []string
		mentions []string
	}{
		{"no equals sign", []string{"ALMANACK_TZ=UTC", "ALMANACK_LISTEN"}, []string{":2", "ALMANACK_LISTEN"}},
		{"empty key", []string{"=orphaned"}, []string{":1", "empty key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConf(t, tc.lines...)
			_, err := ParseFile(path)
			wantErrMentioning(t, err, append(tc.mentions, path)...)
		})
	}
}

// A line too long for the scanner has to be an error rather than a truncated value.
// The plausible version of this is a templating accident that concatenates a whole
// inventory onto one line; a silently shortened VAPID key would then be accepted and
// every push would fail its signature check instead.
func TestParseFileRejectsOversizedLines(t *testing.T) {
	path := writeConf(t, "ALMANACK_VAPID_PRIVATE="+strings.Repeat("k", 128*1024))
	_, err := ParseFile(path)
	wantErrMentioning(t, err, path)
}

// A named file that is not there is an error, not an empty configuration. Silently
// continuing would start the server on defaults after a typo in the unit file, and
// the first sign would be an unreachable port or a database in the wrong place.
func TestMissingConfigFileIsAnError(t *testing.T) {
	isolateEnv(t)

	missing := filepath.Join(t.TempDir(), "nowhere.conf")
	_, err := Load(missing)
	wantErrMentioning(t, err, missing)
}

// ---------------------------------------------------------------------------
// Redacted
// ---------------------------------------------------------------------------

// Redacted() is printed at startup and served from /healthz, so the VAPID private
// key must not survive the trip. That key can never be rotated without breaking
// every push subscription in the family, which makes a leak into journald or a
// browser tab genuinely expensive.
func TestRedactedWithholdsSecrets(t *testing.T) {
	isolateEnv(t)

	const private = "q1dXpw3UpT5PrivateKeyMaterialThatMustNotAppear"
	cfg, err := loadConf(t, minimalProd(
		"ALMANACK_VAPID_PRIVATE="+private,
		"ALMANACK_VAPID_PUBLIC=BJxBoRCV9nqPublicKeyMaterialAlsoLong",
	)...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	out := strings.Join(cfg.Redacted(), "\n")
	if strings.Contains(out, private) {
		t.Errorf("the VAPID private key appears in full in Redacted():\n%s", out)
	}
	if !strings.Contains(out, "vapid_private=") {
		t.Error("Redacted() omits vapid_private entirely; an operator cannot tell whether one is configured")
	}

	// A short secret must not be summarised into something guessable either.
	short := cfg
	short.VAPIDPrivate = "abc123"
	if strings.Contains(strings.Join(short.Redacted(), "\n"), "abc123") {
		t.Error("a short VAPID private key survives redaction")
	}
}

// The distinction between "unset" and "empty string" is the one an operator needs
// when a heartbeat is not arriving, so unset settings say so rather than rendering
// as a blank after the equals sign.
func TestRedactedMarksUnsetSettings(t *testing.T) {
	isolateEnv(t)

	path := writeConf(t,
		"ALMANACK_DEV=true",
		"ALMANACK_DATA=/tmp/dev.db",
		"ALMANACK_BASE_URL=http://localhost:8080",
	)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	joined := strings.Join(cfg.Redacted(), "\n")
	for _, want := range []string{"vapid_public=(unset)", "vapid_private=(unset)", "source_url=(unset)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Redacted() does not contain %q:\n%s", want, joined)
		}
	}
	// The values an operator diagnoses with have to be there in full, the file that
	// was read most of all: it is the answer to "which configuration is this?".
	for _, want := range []string{"config_path=" + path, "data=/tmp/dev.db", "base_url=http://localhost:8080", "tz=Europe/Paris"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Redacted() does not contain %q:\n%s", want, joined)
		}
	}
}
