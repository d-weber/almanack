package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"almanack/internal/auth"
	"almanack/internal/clock"
	"almanack/internal/config"
	"almanack/internal/domain"
	"almanack/internal/store"
)

func bootstrapConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		DataPath: filepath.Join(t.TempDir(), "almanack.db"),
		BaseURL:  "https://calendrier.example.org",
		FamilyTZ: testTZ(t),
	}
}

// capture swaps the process's standard streams for a pipe. The bootstrap command's only
// channel to the operator is what it prints — the initial password and the invite link
// exist nowhere else, the token is stored only as a hash — so a test that cannot read the
// output cannot check the one thing the command is for.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	realOut, realErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w

	read := make(chan string, 1)
	go func() {
		var buf strings.Builder
		_, _ = io.Copy(&buf, r)
		read <- buf.String()
	}()

	func() {
		defer func() {
			os.Stdout, os.Stderr = realOut, realErr
			w.Close()
		}()
		fn()
	}()

	out := <-read
	r.Close()
	return out
}

// labelled returns the value printed after a label, reading the output the way the person
// at the terminal does.
func labelled(t *testing.T, out, label string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == label {
			return strings.Join(fields[1:], " ")
		}
	}
	t.Fatalf("no %s line in the bootstrap output:\n%s", label, out)
	return ""
}

func inviteLink(t *testing.T, out string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "/join/") {
			return line
		}
	}
	t.Fatalf("no invite link in the bootstrap output:\n%s", out)
	return ""
}

func reopen(t *testing.T, cfg config.Config) *store.Store {
	t.Helper()
	st, err := store.Open(cfg.DataPath, cfg.FamilyTZ, clock.NewFake(backupBase))
	if err != nil {
		t.Fatalf("reopen the database: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Signup is invite-only and there is no HTTP route to the first account, so this command
// is the only way into a fresh deployment. Everything it prints has to work first time,
// on a host the operator is standing at with nothing else to fall back on.
func TestBootstrapCreatesTheFirstAccount(t *testing.T) {
	ctx := context.Background()
	cfg := bootstrapConfig(t)

	var runErr error
	out := capture(t, func() {
		runErr = runBootstrap(ctx, cfg, []string{
			"--email", " alex@example.org ", "--name", " Alex ", "--calendar", " Maison ", "--lang", "en",
		})
	})
	if runErr != nil {
		t.Fatalf("runBootstrap: %v\n%s", runErr, out)
	}

	st := reopen(t, cfg)
	if n, err := st.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("CountUsers = %d, %v, want 1", n, err)
	}
	// Surrounding whitespace is trimmed from all three: a value pasted into a terminal
	// carries a stray space often enough, and " alex@example.org " is an address no
	// password reset would ever reach — on the one account that cannot be recreated.
	user, err := st.UserByEmail(ctx, "alex@example.org")
	if err != nil {
		t.Fatalf("the account was not created under the address given: %v", err)
	}
	if user.DisplayName != "Alex" {
		t.Errorf("display name = %q, want %q", user.DisplayName, "Alex")
	}
	// Without this the family has an account and nobody who can administer it, and there
	// is no second run of bootstrap to fix it.
	if !user.IsAdmin {
		t.Error("the first account is not an admin")
	}
	if user.Lang != domain.LangEN {
		t.Errorf("lang = %q, want %q", user.Lang, domain.LangEN)
	}
	if user.WeekStart != time.Monday {
		t.Errorf("week start = %v, want Monday", user.WeekStart)
	}

	// The scheduler reads these; a first account with no row would silently receive no
	// digest and no reminders until somebody opened the settings screen.
	prefs, err := st.Prefs(ctx, user.ID)
	if err != nil {
		t.Fatalf("prefs: %v", err)
	}
	if !prefs.DigestEnabled || prefs.DigestTime != "07:30" || prefs.SummaryTime != "20:00" {
		t.Errorf("notification prefs = %+v, want the digest enabled at 07:30 and a summary at 20:00", prefs)
	}
	if !prefs.EmailReminders || !prefs.ActivityPush {
		t.Errorf("notification prefs = %+v, want email reminders and activity push on", prefs)
	}

	// ListCalendarsForUser is membership-based and is the authorisation source of truth,
	// so this also settles that the creator was made a member of their own calendar.
	cals, err := st.ListCalendarsForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list calendars: %v", err)
	}
	if len(cals) != 1 || cals[0].Name != "Maison" {
		t.Fatalf("calendars = %+v, want one named %q", cals, "Maison")
	}

	// The printed password is the operator's only way in, and it is never stored in
	// plaintext, so the only proof it is the right one is that it verifies.
	password := labelled(t, out, "Password")
	hash, err := st.UserPasswordHash(ctx, user.ID)
	if err != nil {
		t.Fatalf("password hash: %v", err)
	}
	ok, err := auth.VerifyPassword(hash, password)
	if err != nil || !ok {
		t.Errorf("the printed password does not open the account it was printed for (%v)", err)
	}

	// Likewise the link: the token is stored only as a SHA-256, so a link that does not
	// resolve would be discovered by the family, days later, in a browser.
	link := inviteLink(t, out)
	prefix := cfg.BaseURL + "/join/"
	if !strings.HasPrefix(link, prefix) {
		t.Fatalf("invite link %q does not start with %q", link, prefix)
	}
	token := strings.TrimPrefix(link, prefix)
	inv, cal, err := st.InviteByToken(ctx, auth.HashToken(token), time.Now().UTC())
	if err != nil {
		t.Fatalf("the printed invite link does not resolve: %v", err)
	}
	if cal.ID != cals[0].ID || inv.CreatedBy != user.ID {
		t.Errorf("the invite opens calendar %d created by %d, want %d and %d", cal.ID, inv.CreatedBy, cals[0].ID, user.ID)
	}
	if inv.ExpiresAt.Before(time.Now().UTC()) {
		t.Errorf("the invite expired at %s, before it was printed", inv.ExpiresAt)
	}
}

// A base URL is copied out of a config file by hand, so it arrives with a trailing slash
// as often as not. A link with a doubled slash is one an operator has to repair before
// sending it to the family.
func TestBootstrapPrintsALinkWithoutADoubledSlash(t *testing.T) {
	cfg := bootstrapConfig(t)
	cfg.BaseURL = "https://calendrier.example.org/"

	var runErr error
	out := capture(t, func() {
		runErr = runBootstrap(context.Background(), cfg, []string{"--email", "alex@example.org", "--name", "Alex"})
	})
	if runErr != nil {
		t.Fatalf("runBootstrap: %v\n%s", runErr, out)
	}
	if link := inviteLink(t, out); !strings.HasPrefix(link, "https://calendrier.example.org/join/") {
		t.Errorf("invite link = %q", link)
	}
}

// docs/deployment.md states it as a hard contract: "Refuses to run once any account
// exists." Without that, a command whose whole purpose is to mint an admin account
// without a password is a backdoor on a live system — and it is reachable by anyone who
// can run a binary as the service user.
func TestBootstrapRefusesOnceAnAccountExists(t *testing.T) {
	ctx := context.Background()
	cfg := bootstrapConfig(t)

	st := reopen(t, cfg)
	existing, err := st.CreateUser(ctx, domain.User{
		Email: "camille@example.org", DisplayName: "Camille", Color: "#336699",
		Lang: domain.LangFR, WeekStart: time.Monday, TimeFormat: "24h",
	}, "argon2id$fake$camille")
	if err != nil {
		t.Fatalf("create the existing account: %v", err)
	}
	cal, err := st.CreateCalendar(ctx, domain.Calendar{Name: "Maison", Color: "#3b7ddd", CreatorID: existing.ID})
	if err != nil {
		t.Fatalf("create the existing calendar: %v", err)
	}
	hashBefore, err := st.UserPasswordHash(ctx, existing.ID)
	if err != nil {
		t.Fatalf("password hash: %v", err)
	}

	var runErr error
	out := capture(t, func() {
		runErr = runBootstrap(ctx, cfg, []string{"--email", "stranger@example.org", "--name", "Stranger"})
	})
	if runErr == nil {
		t.Fatalf("bootstrap created a second first account on a live database:\n%s", out)
	}

	// The refusal must be total. A half-run that created the account and stopped short of
	// the calendar would leave the family with an intruder they cannot see.
	if n, err := st.CountUsers(ctx); err != nil || n != 1 {
		t.Errorf("CountUsers = %d, %v, want 1", n, err)
	}
	after, err := st.UserByID(ctx, existing.ID)
	if err != nil {
		t.Fatalf("the existing account: %v", err)
	}
	if after.Email != existing.Email || after.DisplayName != existing.DisplayName {
		t.Errorf("the existing account changed: %+v, want %+v", after, existing)
	}
	hashAfter, err := st.UserPasswordHash(ctx, existing.ID)
	if err != nil {
		t.Fatalf("password hash: %v", err)
	}
	if hashAfter != hashBefore {
		t.Error("the existing account's password was replaced")
	}
	cals, err := st.ListCalendarsForUser(ctx, existing.ID)
	if err != nil {
		t.Fatalf("list calendars: %v", err)
	}
	if len(cals) != 1 || cals[0].ID != cal.ID {
		t.Errorf("calendars = %+v, want only the one that was already there", cals)
	}
	// An invite minted by a refused run would outlive it by seven days and let whoever
	// holds the link into the family's calendar.
	invites, err := st.ListInvites(ctx, cal.ID)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(invites) != 0 {
		t.Errorf("the refused run left %d invite(s) behind", len(invites))
	}
}

// Bad arguments must be caught before the database is opened. Creating the file as a side
// effect of a typo leaves an empty database where the operator's next command — or the
// server itself — will find one and assume it is the real thing.
func TestBootstrapValidatesItsArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no address", []string{"--name", "Alex"}, "--email"},
		{"no name", []string{"--email", "alex@example.org"}, "--name"},
		{"neither", nil, "--email"},
		{
			// There are only two message catalogues, and a user whose language matches
			// neither would get a blank interface rather than an error.
			name: "a language nothing is translated into",
			args: []string{"--email", "alex@example.org", "--name", "Alex", "--lang", "de"},
			want: "--lang",
		},
		{
			name: "a flag this command does not have",
			args: []string{"--email", "alex@example.org", "--name", "Alex", "--admin"},
			want: "admin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bootstrapConfig(t)
			var err error
			out := capture(t, func() { err = runBootstrap(context.Background(), cfg, tc.args) })
			if err == nil {
				t.Fatalf("runBootstrap(%q) succeeded:\n%s", tc.args, out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if _, statErr := os.Stat(cfg.DataPath); !os.IsNotExist(statErr) {
				t.Errorf("a rejected invocation created %s anyway (stat error %v)", cfg.DataPath, statErr)
			}
		})
	}
}

// The password is printed to a terminal and quite possibly to a deployment log, so it
// exists to be changed — but it still has to be a password nobody can guess, and it has
// to be a different one on every host it is run on.
func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for range 32 {
		pw, err := generatePassword()
		if err != nil {
			t.Fatalf("generatePassword: %v", err)
		}
		if len(pw) < 16 {
			t.Fatalf("generated password %q is %d characters", pw, len(pw))
		}
		if seen[pw] {
			t.Fatalf("generatePassword returned %q twice", pw)
		}
		seen[pw] = true
	}
}
