// Package config turns a configuration file and the environment into a validated Config.
//
// Every setting the binary has is exposed here, because the machine that deploys this
// (Ansible, in this deployment) must be able to configure the whole application by
// templating one file. Nothing is hidden in a code constant that would require a
// rebuild to change.
//
// The file format is deliberately systemd's EnvironmentFile format — KEY=VALUE, #
// comments, optional quotes — so a single templated file can be used either as
// `EnvironmentFile=` in a unit or passed as `almanack --config <path>`, with no
// translation step and no second format to maintain.
//
// Precedence: environment variable > config file > built-in default.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultPath is consulted when no config path is given and the file exists.
const DefaultPath = "/etc/almanack/almanack.conf"

// known is every setting this binary reads, so a misspelling can be reported rather
// than ignored. Keep it in step with almanack.conf.example.
var known = map[string]bool{
	"ALMANACK_CONFIG": true, "ALMANACK_DEV": true, "ALMANACK_LISTEN": true,
	"ALMANACK_BASE_URL": true, "ALMANACK_DATA": true, "ALMANACK_BACKUP_DIR": true,
	"ALMANACK_TZ": true, "ALMANACK_ALSACE_MOSELLE": true, "ALMANACK_SOURCE_URL": true,
	"ALMANACK_TRUSTED_PROXIES": true, "ALMANACK_SMTP": true, "ALMANACK_MAIL_FROM": true,
	"ALMANACK_OWNER_EMAIL": true, "ALMANACK_MAIL_DIR": true, "ALMANACK_HEARTBEAT_TIME": true,
	"ALMANACK_VAPID_PUBLIC": true, "ALMANACK_VAPID_PRIVATE": true, "ALMANACK_VAPID_SUBJECT": true,
	"ALMANACK_PUSH_HOSTS":   true,
	"ALMANACK_PLAN_HORIZON": true, "ALMANACK_TICK": true,
	"ALMANACK_BACKUP_KEEP_HOURLY": true, "ALMANACK_BACKUP_KEEP_DAILY": true,
	"ALMANACK_BACKUP_KEEP_WEEKLY": true, "ALMANACK_BACKUP_KEEP_MONTHLY": true,
	"ALMANACK_LOG_LEVEL": true, "ALMANACK_LOG_FORMAT": true, "ALMANACK_HOLIDAY_COLOR": true,
}

// DefaultHolidayColor is the red public holidays are drawn in unless configured.
const DefaultHolidayColor = "#d32f2f"

// DefaultSourceURL is deliberately empty: only whoever publishes a build knows where
// its source lives. Set ALMANACK_SOURCE_URL and the About screen links to it — which is
// how an AGPL-3.0 network service offers its source to the people using it (section
// 13). If you deploy a modified version, point it at *your* source, not upstream's.
const DefaultSourceURL = ""

// Config is the whole runtime configuration.
type Config struct {
	// Dev turns on the local development affordances: the /dev endpoints, a file
	// mail sink instead of SMTP, a controllable clock, and cookies without Secure so
	// http://localhost works. It must never be set on the family server.
	Dev bool

	ListenAddr string // ALMANACK_LISTEN
	BaseURL    string // ALMANACK_BASE_URL — public origin, used in invite links and emails
	DataPath   string // ALMANACK_DATA — the SQLite file
	BackupDir  string // ALMANACK_BACKUP_DIR

	TZName   string // ALMANACK_TZ
	FamilyTZ *time.Location

	// TrustedProxies are the peers whose X-Forwarded-For header may be believed.
	// Behind a reverse proxy every request appears to come from the proxy, so
	// without this the login rate limiter would share one bucket for the whole
	// family — lock one person out and you lock out everyone.
	TrustedProxies []string // ALMANACK_TRUSTED_PROXIES, CSV

	// Mail. The binary only ever talks to a local MTA: when the family's provider
	// next changes its authentication rules, that is an OS config edit, not an
	// application rebuild.
	SMTPAddr   string // ALMANACK_SMTP
	MailFrom   string // ALMANACK_MAIL_FROM
	OwnerEmail string // ALMANACK_OWNER_EMAIL — receives failure alerts and the ops heartbeat
	MailDir    string // ALMANACK_MAIL_DIR — dev only: where the file sink writes .eml files

	VAPIDPublic  string // ALMANACK_VAPID_PUBLIC
	VAPIDPrivate string // ALMANACK_VAPID_PRIVATE
	VAPIDSubject string // ALMANACK_VAPID_SUBJECT — mailto: contact, required by RFC 8292

	// PushHosts are the hostnames a push subscription endpoint may point at. A
	// subscription endpoint is a URL a member supplies and this server then posts
	// to, so it is the one place a request can be aimed at the machine's own
	// network; the allowlist is what keeps it pointed at a push service. Empty
	// means domain.DefaultPushHosts, which covers every browser. A single "*"
	// turns the check off, for a self-hosted push service.
	PushHosts []string // ALMANACK_PUSH_HOSTS, CSV

	AlsaceMoselle bool // ALMANACK_ALSACE_MOSELLE — the two extra public holidays

	// HolidayColor is the colour public holidays are drawn in. They are not events
	// and belong to no calendar, so they have no label to take a colour from.
	HolidayColor string // ALMANACK_HOLIDAY_COLOR

	// SourceURL is where this build's source can be obtained. Almanack is AGPL-3.0:
	// if you modify it and let other people use it over a network, section 13
	// obliges you to offer them the source of *your* version. The app shows this
	// link in its About screen, which is the simplest way to comply.
	SourceURL string // ALMANACK_SOURCE_URL

	PlanHorizon   time.Duration // ALMANACK_PLAN_HORIZON — how far ahead notifications are materialized
	SchedulerTick time.Duration // ALMANACK_TICK — how often the outbox is drained

	// HeartbeatTime is when the daily ops summary is mailed to OwnerEmail (HH:MM,
	// family time). A family server has no pager, so this mail — and its absence —
	// is the monitoring. Empty disables it.
	HeartbeatTime string // ALMANACK_HEARTBEAT_TIME

	// Backup retention, applied by `almanack backup --prune`. Generational rather than
	// "last N days", so that corruption discovered late is still recoverable.
	KeepHourly  int // ALMANACK_BACKUP_KEEP_HOURLY
	KeepDaily   int // ALMANACK_BACKUP_KEEP_DAILY
	KeepWeekly  int // ALMANACK_BACKUP_KEEP_WEEKLY
	KeepMonthly int // ALMANACK_BACKUP_KEEP_MONTHLY

	LogLevel  string // ALMANACK_LOG_LEVEL — debug|info|warn|error
	LogFormat string // ALMANACK_LOG_FORMAT — text|json

	// ConfigPath records where settings were read from, for logging and /healthz.
	ConfigPath string
}

// Load reads the config file (if any) and the environment, then validates. Pass an
// empty path to use ALMANACK_CONFIG, or DefaultPath when that exists.
func Load(path string) (Config, error) {
	if path == "" {
		path = os.Getenv("ALMANACK_CONFIG")
	}
	if path == "" {
		if _, err := os.Stat(DefaultPath); err == nil {
			path = DefaultPath
		}
	}

	file := map[string]string{}
	if path != "" {
		var err error
		file, err = ParseFile(path)
		if err != nil {
			return Config{}, err
		}
	}

	get := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		if v, ok := file[key]; ok && v != "" {
			return v
		}
		return def
	}
	// getAllowingEmpty is get() for a setting whose documented contract includes an
	// empty value, which today is ALMANACK_HEARTBEAT_TIME and nothing else: setting
	// it to nothing is how the daily mail is turned off, and get() read that as
	// "absent" and handed back 08:00.
	//
	// It is deliberately not the rule for every setting. Everywhere else an emptied
	// line is a templating accident rather than an instruction, and taking it
	// literally would read ALMANACK_TZ= as UTC — time.LoadLocation("") is UTC, and
	// returns no error — which puts the whole family's calendar an hour out for half
	// the year on an upgrade nobody would connect it to.
	getAllowingEmpty := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if v, ok := file[key]; ok {
			return v
		}
		return def
	}
	// A misspelt value used to fall back to the default in silence, so
	// ALMANACK_ALSACE_MOSELLE=yes — the natural spelling — quietly switched the two
	// extra public holidays back off. Collect the complaint instead; the reporting
	// at the end of Load already knows how to present it.
	var bad []string
	getBool := func(key string, def bool) bool {
		v := get(key, "")
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s=%q is not a true/false value (use true or false)", key, v))
			return def
		}
		return b
	}
	getInt := func(key string, def int) int {
		v := get(key, "")
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s=%q is not a whole number", key, v))
			return def
		}
		return n
	}
	getDur := func(key string, def time.Duration) time.Duration {
		v := get(key, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s=%q is not a duration (try 30s, 10m, 48h)", key, v))
			return def
		}
		return d
	}

	c := Config{
		ConfigPath:     path,
		Dev:            getBool("ALMANACK_DEV", false),
		ListenAddr:     get("ALMANACK_LISTEN", "127.0.0.1:8080"),
		BaseURL:        strings.TrimRight(get("ALMANACK_BASE_URL", ""), "/"),
		DataPath:       get("ALMANACK_DATA", ""),
		BackupDir:      get("ALMANACK_BACKUP_DIR", ""),
		TZName:         get("ALMANACK_TZ", "Europe/Paris"),
		TrustedProxies: splitCSV(get("ALMANACK_TRUSTED_PROXIES", "127.0.0.1,::1")),
		SMTPAddr:       get("ALMANACK_SMTP", "127.0.0.1:25"),
		MailFrom:       get("ALMANACK_MAIL_FROM", ""),
		OwnerEmail:     get("ALMANACK_OWNER_EMAIL", ""),
		MailDir:        get("ALMANACK_MAIL_DIR", ""),
		VAPIDPublic:    get("ALMANACK_VAPID_PUBLIC", ""),
		VAPIDPrivate:   get("ALMANACK_VAPID_PRIVATE", ""),
		VAPIDSubject:   get("ALMANACK_VAPID_SUBJECT", ""),
		PushHosts:      splitCSV(get("ALMANACK_PUSH_HOSTS", "")),
		AlsaceMoselle:  getBool("ALMANACK_ALSACE_MOSELLE", false),
		HolidayColor:   get("ALMANACK_HOLIDAY_COLOR", DefaultHolidayColor),
		SourceURL:      get("ALMANACK_SOURCE_URL", DefaultSourceURL),
		PlanHorizon:    getDur("ALMANACK_PLAN_HORIZON", 48*time.Hour),
		SchedulerTick:  getDur("ALMANACK_TICK", 30*time.Second),
		HeartbeatTime:  getAllowingEmpty("ALMANACK_HEARTBEAT_TIME", "08:00"),
		KeepHourly:     getInt("ALMANACK_BACKUP_KEEP_HOURLY", 48),
		KeepDaily:      getInt("ALMANACK_BACKUP_KEEP_DAILY", 14),
		KeepWeekly:     getInt("ALMANACK_BACKUP_KEEP_WEEKLY", 8),
		KeepMonthly:    getInt("ALMANACK_BACKUP_KEEP_MONTHLY", 24),
		LogLevel:       get("ALMANACK_LOG_LEVEL", "info"),
		LogFormat:      get("ALMANACK_LOG_FORMAT", "text"),
	}

	loc, err := time.LoadLocation(c.TZName)
	if err != nil {
		return Config{}, fmt.Errorf("ALMANACK_TZ %q: %w", c.TZName, err)
	}
	c.FamilyTZ = loc

	if c.Dev {
		if c.DataPath == "" {
			c.DataPath = filepath.Join("devdata", "almanack.db")
		}
		if c.BaseURL == "" {
			c.BaseURL = "http://" + strings.Replace(c.ListenAddr, "127.0.0.1", "localhost", 1)
		}
		if c.MailDir == "" {
			c.MailDir = filepath.Join(filepath.Dir(c.DataPath), "mail")
		}
		if c.MailFrom == "" {
			c.MailFrom = "almanack@localhost"
		}
		if c.OwnerEmail == "" {
			c.OwnerEmail = "owner@localhost"
		}
		if c.VAPIDSubject == "" {
			c.VAPIDSubject = "mailto:" + c.OwnerEmail
		}
	}
	if c.BackupDir == "" && c.DataPath != "" {
		c.BackupDir = filepath.Join(filepath.Dir(c.DataPath), "backups")
	}

	// An unrecognised key is almost always a typo, and the settings are a closed set
	// — silently ignoring ALMANACK_TZZ books every event in the wrong timezone.
	for key := range file {
		if !strings.HasPrefix(key, "ALMANACK_") {
			continue
		}
		if !known[key] {
			bad = append(bad, fmt.Sprintf("%s is not a setting this version understands (check the spelling against almanack.conf.example)", key))
		}
	}

	if err := c.validate(bad); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate(problems []string) error {
	if c.DataPath == "" {
		problems = append(problems, "ALMANACK_DATA is required (path to the SQLite file)")
	}
	if c.BaseURL == "" {
		problems = append(problems, "ALMANACK_BASE_URL is required (used in invite links and emails)")
	}
	if !isHexColor(c.HolidayColor) {
		problems = append(problems, fmt.Sprintf("ALMANACK_HOLIDAY_COLOR=%q must be a hex colour such as #d32f2f", c.HolidayColor))
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("ALMANACK_LOG_LEVEL=%q must be debug, info, warn or error", c.LogLevel))
	}
	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
	default:
		problems = append(problems, fmt.Sprintf("ALMANACK_LOG_FORMAT=%q must be text or json", c.LogFormat))
	}
	if c.PlanHorizon <= 0 {
		problems = append(problems, "ALMANACK_PLAN_HORIZON must be positive")
	}
	if c.SchedulerTick <= 0 {
		problems = append(problems, "ALMANACK_TICK must be positive")
	}
	if c.HeartbeatTime != "" && !validHHMM(c.HeartbeatTime) {
		problems = append(problems, "ALMANACK_HEARTBEAT_TIME must be HH:MM (or empty to disable)")
	}
	if !c.Dev {
		if !strings.HasPrefix(c.BaseURL, "https://") {
			problems = append(problems, "ALMANACK_BASE_URL must be https outside dev mode: PWA installation and Web Push both require a secure origin")
		}
		if c.MailFrom == "" {
			problems = append(problems, "ALMANACK_MAIL_FROM is required")
		}
		if c.OwnerEmail == "" {
			problems = append(problems, "ALMANACK_OWNER_EMAIL is required: it receives failure alerts, and without it nothing on this server fails loudly")
		}
		if c.VAPIDPublic == "" || c.VAPIDPrivate == "" {
			problems = append(problems, "ALMANACK_VAPID_PUBLIC and ALMANACK_VAPID_PRIVATE are required (generate once with `almanack gen-vapid`; never rotate them, as that invalidates every push subscription)")
		}
		if c.VAPIDSubject == "" {
			problems = append(problems, "ALMANACK_VAPID_SUBJECT is required, e.g. mailto:you@example.org")
		}
	}
	if len(problems) > 0 {
		hint := ""
		if c.ConfigPath == "" {
			hint = "\n\nNo configuration file was read. Pass --config <path>, set ALMANACK_CONFIG, or place one at " + DefaultPath + ".\nSee almanack.conf.example for a complete annotated file."
		} else {
			hint = "\n\nConfiguration file: " + c.ConfigPath
		}
		return errors.New("configuration problems:\n  - " + strings.Join(problems, "\n  - ") + hint)
	}
	return nil
}

// ParseFile reads a systemd EnvironmentFile-style KEY=VALUE file.
func ParseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		// Tolerate a leading "export " so the same file can also be sourced by a shell.
		text = strings.TrimPrefix(text, "export ")

		key, value, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE, got %q", path, line, text)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, line)
		}
		out[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return out, nil
}

// Redacted returns the configuration as ordered key/value lines with secrets masked,
// for logging at startup and for the /healthz detail view.
func (c Config) Redacted() []string {
	mask := func(s string) string {
		if s == "" {
			return "(unset)"
		}
		if len(s) <= 8 {
			return "********"
		}
		return s[:4] + "…" + s[len(s)-2:]
	}
	return []string{
		"config_path=" + orNone(c.ConfigPath),
		"dev=" + strconv.FormatBool(c.Dev),
		"listen=" + c.ListenAddr,
		"base_url=" + c.BaseURL,
		"data=" + c.DataPath,
		"backup_dir=" + c.BackupDir,
		"tz=" + c.TZName,
		"trusted_proxies=" + strings.Join(c.TrustedProxies, ","),
		"smtp=" + c.SMTPAddr,
		"mail_from=" + orNone(c.MailFrom),
		"owner_email=" + orNone(c.OwnerEmail),
		"vapid_public=" + mask(c.VAPIDPublic),
		"vapid_private=" + mask(c.VAPIDPrivate),
		"vapid_subject=" + orNone(c.VAPIDSubject),
		"push_hosts=" + orDefault(strings.Join(c.PushHosts, ",")),
		"alsace_moselle=" + strconv.FormatBool(c.AlsaceMoselle),
		"source_url=" + orNone(c.SourceURL),
		"plan_horizon=" + c.PlanHorizon.String(),
		"tick=" + c.SchedulerTick.String(),
		"heartbeat_time=" + orOff(c.HeartbeatTime),
		"log_level=" + c.LogLevel,
	}
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

// orOff is orNone for a setting whose empty value is a decision rather than an
// omission: an empty ALMANACK_HEARTBEAT_TIME is how the daily mail is turned off,
// and "(unset)" would read as an oversight to whoever is asking why it stopped.
func orOff(s string) string {
	if s == "" {
		return "(disabled)"
	}
	return s
}

// orDefault is orNone for a setting whose empty value means a built-in list
// rather than nothing at all: "(unset)" would read as "push is not restricted".
func orDefault(s string) string {
	if s == "" {
		return "(built-in list)"
	}
	return s
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validHHMM(s string) bool {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	return err1 == nil && err2 == nil && hh >= 0 && hh < 24 && mm >= 0 && mm < 60
}

// isHexColor accepts the #rrggbb form the browser expects.
func isHexColor(v string) bool {
	if len(v) != 7 || v[0] != '#' {
		return false
	}
	for _, r := range v[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
