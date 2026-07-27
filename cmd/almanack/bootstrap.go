package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"strings"
	"time"

	"almanack/internal/auth"
	"almanack/internal/clock"
	"almanack/internal/config"
	"almanack/internal/domain"
	"almanack/internal/store"
)

// Signing up requires a valid invite — there is no open registration, which is what
// keeps a WAN-facing family calendar from collecting strangers. That leaves a
// chicken-and-egg problem on a fresh database: no account exists to issue the first
// invite. This command is the way in, and it is deliberately a local CLI operation
// rather than an HTTP endpoint, so the bootstrap window is never reachable from the
// internet at all.
func runBootstrap(ctx context.Context, cfg config.Config, args []string) error {
	var email, name, calendarName, color, lang string
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.StringVar(&email, "email", "", "email address of the first account (required)")
	fs.StringVar(&name, "name", "", "display name, e.g. \"Alex\" (required)")
	fs.StringVar(&calendarName, "calendar", "Family", "name of the first shared calendar")
	fs.StringVar(&color, "color", "#c0392b", "this person's colour, #rrggbb")
	fs.StringVar(&lang, "lang", "fr", "interface language: fr or en")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if email == "" || name == "" {
		fs.Usage()
		return fmt.Errorf("--email and --name are required")
	}
	if !domain.Language(lang).Valid() {
		return fmt.Errorf("--lang must be fr or en")
	}

	st, err := store.Open(cfg.DataPath, cfg.FamilyTZ, clock.Real{})
	if err != nil {
		return err
	}
	defer st.Close()

	// Refusing when accounts exist keeps this from becoming a backdoor that quietly
	// mints admins on a live system.
	existing, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	if existing > 0 {
		return fmt.Errorf("this database already has %d account(s); invite the rest of the family from inside the app instead", existing)
	}

	password, err := generatePassword()
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	user, err := st.CreateUser(ctx, domain.User{
		Email:       strings.TrimSpace(email),
		DisplayName: strings.TrimSpace(name),
		Color:       color,
		Lang:        domain.Language(lang),
		WeekStart:   time.Monday,
		TimeFormat:  "24h",
		IsAdmin:     true,
	}, hash)
	if err != nil {
		return fmt.Errorf("create the first account: %w", err)
	}
	if err := st.UpdatePrefs(ctx, domain.NotificationPrefs{
		UserID: user.ID, DigestEnabled: true, DigestTime: "07:30",
		SummaryTime: "20:00", EmailReminders: true, ActivityPush: true,
	}); err != nil {
		return err
	}

	cal, err := st.CreateCalendar(ctx, domain.Calendar{
		Name: strings.TrimSpace(calendarName), Color: "#3b7ddd", CreatorID: user.ID,
	})
	if err != nil {
		return fmt.Errorf("create the first calendar: %w", err)
	}

	token, tokenHash, err := auth.NewToken()
	if err != nil {
		return err
	}
	if _, err := st.CreateInvite(ctx, domain.Invite{
		CalendarID: cal.ID,
		CreatedBy:  user.ID,
		ExpiresAt:  time.Now().Add(domain.InviteTTL).UTC(),
	}, tokenHash); err != nil {
		return fmt.Errorf("create the first invite: %w", err)
	}

	base := strings.TrimRight(cfg.BaseURL, "/")
	fmt.Printf(`Created the first account and the "%s" calendar.

  Sign in at   %s
  Email        %s
  Password     %s

Change that password after signing in; it was generated here and has been printed
to this terminal, so treat it as compromised the moment anyone else sees the screen.

Invite the rest of the family with this link (valid 7 days, reusable):

  %s/join/%s

You can issue more invites from inside the app.
`, cal.Name, base, user.Email, password, base, token)
	return nil
}

// generatePassword returns a random, readable initial password. It exists to be
// changed: it is printed to a terminal and quite possibly to a deployment log.
func generatePassword() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
