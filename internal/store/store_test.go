package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"
)

// baseTime is the instant every test starts from: a Wednesday morning, safely inside
// French summer time so that the family-tz conversions in EventsInRange are exercised
// at UTC+2 rather than at a lucky UTC+0.
var baseTime = time.Date(2026, 7, 1, 7, 0, 0, 0, time.UTC)

func testLocation(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("load Europe/Paris: %v", err)
	}
	return loc
}

// newStore opens a fresh database in a temp directory. It returns the path too, so a
// test can close the store and reopen the same file.
func newStore(t *testing.T) (*Store, *clock.Fake, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "almanack.db")
	clk := clock.NewFake(baseTime)
	s, err := Open(path, testLocation(t), clk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clk, path
}

func ctx() context.Context { return context.Background() }

func mustUser(t *testing.T, s *Store, email, name string) domain.User {
	t.Helper()
	u, err := s.CreateUser(ctx(), domain.User{
		Email: email, DisplayName: name, Color: "#336699", Lang: domain.LangFR,
		WeekStart: time.Monday, TimeFormat: "24h",
	}, "argon2id$fake$"+email)
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

func mustCalendar(t *testing.T, s *Store, creator int64, name string) domain.Calendar {
	t.Helper()
	c, err := s.CreateCalendar(ctx(), domain.Calendar{Name: name, Color: "#123456", CreatorID: creator})
	if err != nil {
		t.Fatalf("create calendar %s: %v", name, err)
	}
	return c
}

// firstLabel returns a calendar's first seeded label, which every test event is filed
// under.
func firstLabel(t *testing.T, s *Store, calID int64) domain.Label {
	t.Helper()
	labels, err := s.ListLabels(ctx(), calID)
	if err != nil {
		t.Fatalf("list labels: %v", err)
	}
	if len(labels) == 0 {
		t.Fatal("calendar has no labels")
	}
	return labels[0]
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

func TestMigrationsAreIdempotent(t *testing.T) {
	s, _, path := newStore(t)

	embedded, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}

	countApplied := func(s *Store) int {
		t.Helper()
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatalf("count schema_migrations: %v", err)
		}
		return n
	}
	if got := countApplied(s); got != len(embedded) {
		t.Fatalf("applied %d migrations, want %d", got, len(embedded))
	}

	// A row written before the restart must survive it, and reopening must not try to
	// replay anything.
	u := mustUser(t, s, "reopen@example.test", "Reopen")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path, testLocation(t), clock.NewFake(baseTime))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if got := countApplied(s2); got != len(embedded) {
		t.Fatalf("after reopen applied %d migrations, want %d", got, len(embedded))
	}
	if _, err := s2.UserByID(ctx(), u.ID); err != nil {
		t.Fatalf("user lost across reopen: %v", err)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	s, _, path := newStore(t)

	// Pretend a future release has been here.
	if _, err := s.DB().Exec(
		`INSERT INTO schema_migrations (version, applied_at) VALUES (9999, ?)`,
		mustInstant(baseTime)); err != nil {
		t.Fatalf("fake future migration: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := Open(path, testLocation(t), clock.NewFake(baseTime)); err == nil {
		t.Fatal("Open accepted a database migrated by a newer binary; it must refuse")
	}
}

func TestOpenRejectsBadArguments(t *testing.T) {
	if _, err := Open("", testLocation(t), clock.NewFake(baseTime)); err == nil {
		t.Error("Open accepted an empty path")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "x.db"), nil, clock.NewFake(baseTime)); err == nil {
		t.Error("Open accepted a nil location")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "y.db"), testLocation(t), nil); err == nil {
		t.Error("Open accepted a nil clock")
	}
}

// ---------------------------------------------------------------------------
// Users, sessions, resets
// ---------------------------------------------------------------------------

func TestUserLifecycle(t *testing.T) {
	s, _, _ := newStore(t)

	if n, err := s.CountUsers(ctx()); err != nil || n != 0 {
		t.Fatalf("CountUsers on a fresh database = %d, %v; want 0, nil", n, err)
	}

	u := mustUser(t, s, "Claire@Example.test", "Claire")
	if u.ID == 0 || u.CreatedAt.IsZero() {
		t.Fatalf("CreateUser returned %+v; want an id and a created_at", u)
	}
	if u.HasAvatar {
		t.Error("a new user should not have an avatar")
	}

	// users.email is COLLATE NOCASE, so both the duplicate check and the lookup are
	// case-insensitive.
	if _, err := s.CreateUser(ctx(), domain.User{Email: "claire@example.test", DisplayName: "Impostor",
		Color: "#000000", Lang: domain.LangFR, TimeFormat: "24h"}, "hash"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate email error = %v; want domain.ErrConflict", err)
	}
	byEmail, err := s.UserByEmail(ctx(), "CLAIRE@EXAMPLE.TEST")
	if err != nil || byEmail.ID != u.ID {
		t.Fatalf("UserByEmail (different case) = %+v, %v", byEmail, err)
	}

	if _, err := s.UserByID(ctx(), 4242); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UserByID of a missing user = %v; want domain.ErrNotFound", err)
	}

	hash, err := s.UserPasswordHash(ctx(), u.ID)
	if err != nil || hash == "" {
		t.Fatalf("UserPasswordHash = %q, %v", hash, err)
	}
	if err := s.SetPassword(ctx(), u.ID, "argon2id$new"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if hash, _ = s.UserPasswordHash(ctx(), u.ID); hash != "argon2id$new" {
		t.Fatalf("password hash after SetPassword = %q", hash)
	}

	u.DisplayName = "Claire M."
	u.Lang = domain.LangEN
	u.WeekStart = time.Sunday
	if err := s.UpdateUser(ctx(), u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	got, err := s.UserByID(ctx(), u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.DisplayName != "Claire M." || got.Lang != domain.LangEN || got.WeekStart != time.Sunday {
		t.Fatalf("UpdateUser did not stick: %+v", got)
	}

	// Avatars.
	if _, err := s.Avatar(ctx(), u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Avatar with none set = %v; want domain.ErrNotFound", err)
	}
	if err := s.SetAvatar(ctx(), u.ID, []byte{0x89, 'P', 'N', 'G'}); err != nil {
		t.Fatalf("SetAvatar: %v", err)
	}
	if got, _ := s.UserByID(ctx(), u.ID); !got.HasAvatar {
		t.Error("HasAvatar is false after SetAvatar")
	}
	img, err := s.Avatar(ctx(), u.ID)
	if err != nil || string(img) != "\x89PNG" {
		t.Fatalf("Avatar = %q, %v", img, err)
	}
	if err := s.DeleteAvatar(ctx(), u.ID); err != nil {
		t.Fatalf("DeleteAvatar: %v", err)
	}
	if _, err := s.Avatar(ctx(), u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Avatar after delete = %v; want domain.ErrNotFound", err)
	}

	mustUser(t, s, "marc@example.test", "Aaron")
	users, err := s.ListUsers(ctx())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 || users[0].DisplayName != "Aaron" {
		t.Fatalf("ListUsers = %+v; want two users ordered by display name", users)
	}
	if n, _ := s.CountUsers(ctx()); n != 2 {
		t.Fatalf("CountUsers = %d; want 2", n)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s, clk, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")

	expires := baseTime.Add(domain.SessionTTL)
	sess, err := s.CreateSession(ctx(), u.ID, "hash-a", expires)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID == 0 || !sess.ExpiresAt.Equal(expires) {
		t.Fatalf("CreateSession = %+v", sess)
	}

	gotSess, gotUser, err := s.SessionByToken(ctx(), "hash-a")
	if err != nil {
		t.Fatalf("SessionByToken: %v", err)
	}
	if gotSess.ID != sess.ID || gotUser.ID != u.ID || gotUser.Email != u.Email {
		t.Fatalf("SessionByToken = %+v / %+v", gotSess, gotUser)
	}

	if _, _, err := s.SessionByToken(ctx(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SessionByToken with an unknown token = %v; want domain.ErrNotFound", err)
	}

	// Sliding renewal.
	later := baseTime.Add(48 * time.Hour)
	clk.Set(later)
	if err := s.TouchSession(ctx(), sess.ID, later, later.Add(domain.SessionTTL)); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	gotSess, _, _ = s.SessionByToken(ctx(), "hash-a")
	if !gotSess.LastSeenAt.Equal(later) {
		t.Fatalf("last_seen_at = %v; want %v", gotSess.LastSeenAt, later)
	}

	// An expired session is invisible, whatever the pruner has or has not done.
	clk.Set(later.Add(domain.SessionTTL + time.Hour))
	if _, _, err := s.SessionByToken(ctx(), "hash-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SessionByToken on an expired session = %v; want domain.ErrNotFound", err)
	}
	n, err := s.DeleteExpiredSessions(ctx(), clk.Now())
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredSessions = %d, %v; want 1, nil", n, err)
	}

	// Logout is idempotent; a password change logs every device out.
	clk.Set(baseTime)
	if _, err := s.CreateSession(ctx(), u.ID, "hash-b", baseTime.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.CreateSession(ctx(), u.ID, "hash-c", baseTime.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.DeleteSession(ctx(), "hash-b"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := s.DeleteSession(ctx(), "hash-b"); err != nil {
		t.Fatalf("DeleteSession must be idempotent, got %v", err)
	}
	if err := s.DeleteUserSessions(ctx(), u.ID); err != nil {
		t.Fatalf("DeleteUserSessions: %v", err)
	}
	if _, _, err := s.SessionByToken(ctx(), "hash-c"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("session survived DeleteUserSessions: %v", err)
	}
}

func TestPasswordResetIsSingleUse(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")

	expires := baseTime.Add(domain.PasswordResetTTL)
	if err := s.CreatePasswordReset(ctx(), u.ID, "reset-hash", expires); err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}

	got, err := s.ConsumePasswordReset(ctx(), "reset-hash", baseTime.Add(time.Minute))
	if err != nil || got != u.ID {
		t.Fatalf("ConsumePasswordReset = %d, %v; want %d, nil", got, err, u.ID)
	}
	if _, err := s.ConsumePasswordReset(ctx(), "reset-hash", baseTime.Add(2*time.Minute)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second ConsumePasswordReset = %v; want domain.ErrNotFound", err)
	}

	// Expiry.
	if err := s.CreatePasswordReset(ctx(), u.ID, "stale-hash", expires); err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}
	if _, err := s.ConsumePasswordReset(ctx(), "stale-hash", expires.Add(time.Second)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired ConsumePasswordReset = %v; want domain.ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Calendars, membership, labels
// ---------------------------------------------------------------------------

func TestCreateCalendarSeedsMembershipAndLabels(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")

	cal := mustCalendar(t, s, u.ID, "Maison")
	if cal.ID == 0 || cal.CreatedAt.IsZero() {
		t.Fatalf("CreateCalendar = %+v", cal)
	}

	member, err := s.IsMember(ctx(), cal.ID, u.ID)
	if err != nil || !member {
		t.Fatalf("the creator is not a member: %v, %v", member, err)
	}

	labels, err := s.ListLabels(ctx(), cal.ID)
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	if len(labels) != domain.LabelsPerCalendar {
		t.Fatalf("seeded %d labels, want %d", len(labels), domain.LabelsPerCalendar)
	}
	colors := map[string]bool{}
	for i, l := range labels {
		if l.Position != i {
			t.Errorf("label %d has position %d", i, l.Position)
		}
		if l.Name == "" || l.Color == "" {
			t.Errorf("label %d is unnamed or uncoloured: %+v", i, l)
		}
		if colors[l.Color] {
			t.Errorf("label colour %s is used twice", l.Color)
		}
		colors[l.Color] = true
	}

	// Labels are renamed, never created or deleted.
	l := labels[3]
	l.Name = "Médecin"
	l.Color = "#ff00aa"
	l.Position = 9
	if err := s.UpdateLabel(ctx(), l); err != nil {
		t.Fatalf("UpdateLabel: %v", err)
	}
	got, err := s.LabelByID(ctx(), l.ID)
	if err != nil || got.Name != "Médecin" || got.Color != "#ff00aa" || got.Position != 9 {
		t.Fatalf("LabelByID after update = %+v, %v", got, err)
	}

	cal.Name = "Maison Dupont"
	if err := s.UpdateCalendar(ctx(), cal); err != nil {
		t.Fatalf("UpdateCalendar: %v", err)
	}
	if got, _ := s.CalendarByID(ctx(), cal.ID); got.Name != "Maison Dupont" {
		t.Fatalf("UpdateCalendar did not stick: %+v", got)
	}

	cals, err := s.ListCalendarsForUser(ctx(), u.ID)
	if err != nil || len(cals) != 1 || cals[0].ID != cal.ID {
		t.Fatalf("ListCalendarsForUser = %+v, %v", cals, err)
	}

	if err := s.DeleteCalendar(ctx(), cal.ID); err != nil {
		t.Fatalf("DeleteCalendar: %v", err)
	}
	if _, err := s.CalendarByID(ctx(), cal.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("calendar survived deletion: %v", err)
	}
	// Labels cascade away with it.
	if labels, _ := s.ListLabels(ctx(), cal.ID); len(labels) != 0 {
		t.Fatalf("%d labels outlived their calendar", len(labels))
	}
}

func TestMembershipAndCreatorTransfer(t *testing.T) {
	s, clk, _ := newStore(t)
	creator := mustUser(t, s, "creator@example.test", "Creator")
	early := mustUser(t, s, "early@example.test", "Early")
	late := mustUser(t, s, "late@example.test", "Late")
	cal := mustCalendar(t, s, creator.ID, "Maison")

	clk.Set(baseTime.Add(time.Hour))
	if err := s.AddMember(ctx(), cal.ID, early.ID); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	// A second click on the same invite link must not be an error.
	if err := s.AddMember(ctx(), cal.ID, early.ID); err != nil {
		t.Fatalf("AddMember must be idempotent, got %v", err)
	}
	clk.Set(baseTime.Add(2 * time.Hour))
	if err := s.AddMember(ctx(), cal.ID, late.ID); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if n, _ := s.CountMembers(ctx(), cal.ID); n != 3 {
		t.Fatalf("CountMembers = %d; want 3", n)
	}
	members, err := s.ListMembers(ctx(), cal.ID)
	if err != nil || len(members) != 3 {
		t.Fatalf("ListMembers = %+v, %v", members, err)
	}
	if members[0].UserID != creator.ID || members[1].UserID != early.ID || members[2].UserID != late.ID {
		t.Fatalf("ListMembers is not oldest-first: %+v", members)
	}

	m, err := s.Membership(ctx(), cal.ID, early.ID)
	if err != nil || m.Muted || m.ParticipatingOnly {
		t.Fatalf("Membership = %+v, %v; want the defaults", m, err)
	}
	m.Muted = true
	m.ParticipatingOnly = true
	if err := s.UpdateMembership(ctx(), m); err != nil {
		t.Fatalf("UpdateMembership: %v", err)
	}
	if got, _ := s.Membership(ctx(), cal.ID, early.ID); !got.Muted || !got.ParticipatingOnly {
		t.Fatalf("UpdateMembership did not stick: %+v", got)
	}

	// The creator leaves: the longest-standing remaining member takes over.
	if err := s.TransferCreator(ctx(), cal.ID); err != nil {
		t.Fatalf("TransferCreator: %v", err)
	}
	got, _ := s.CalendarByID(ctx(), cal.ID)
	if got.CreatorID != early.ID {
		t.Fatalf("creator is now %d; want the longest-standing member %d", got.CreatorID, early.ID)
	}

	if err := s.RemoveMember(ctx(), cal.ID, creator.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if err := s.RemoveMember(ctx(), cal.ID, creator.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removing a non-member = %v; want domain.ErrNotFound", err)
	}
	if member, _ := s.IsMember(ctx(), cal.ID, creator.ID); member {
		t.Error("IsMember still true after RemoveMember")
	}

	// With nobody left to promote, the caller has to delete the calendar instead.
	if err := s.RemoveMember(ctx(), cal.ID, late.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if err := s.TransferCreator(ctx(), cal.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("TransferCreator with no candidate = %v; want domain.ErrNotFound", err)
	}
}

func TestInviteExpiryAndRevocation(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")

	inv, err := s.CreateInvite(ctx(), domain.Invite{CalendarID: cal.ID, CreatedBy: u.ID}, "invite-hash")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if !inv.ExpiresAt.Equal(baseTime.Add(domain.InviteTTL)) {
		t.Fatalf("default expiry = %v; want now + InviteTTL", inv.ExpiresAt)
	}

	gotInv, gotCal, err := s.InviteByToken(ctx(), "invite-hash", baseTime.Add(time.Hour))
	if err != nil || gotInv.ID != inv.ID || gotCal.ID != cal.ID {
		t.Fatalf("InviteByToken = %+v / %+v, %v", gotInv, gotCal, err)
	}

	// Multi-use inside the window.
	if err := s.IncrementInviteUse(ctx(), inv.ID); err != nil {
		t.Fatalf("IncrementInviteUse: %v", err)
	}
	if err := s.IncrementInviteUse(ctx(), inv.ID); err != nil {
		t.Fatalf("IncrementInviteUse: %v", err)
	}
	gotInv, _, _ = s.InviteByToken(ctx(), "invite-hash", baseTime.Add(time.Hour))
	if gotInv.UsedCount != 2 {
		t.Fatalf("used_count = %d; want 2", gotInv.UsedCount)
	}

	// Expired.
	if _, _, err := s.InviteByToken(ctx(), "invite-hash", inv.ExpiresAt.Add(time.Second)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired InviteByToken = %v; want domain.ErrNotFound", err)
	}

	// Revoked.
	revokedAt := baseTime.Add(2 * time.Hour)
	if err := s.RevokeInvite(ctx(), inv.ID, revokedAt); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}
	if _, _, err := s.InviteByToken(ctx(), "invite-hash", baseTime.Add(3*time.Hour)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoked InviteByToken = %v; want domain.ErrNotFound", err)
	}
	if err := s.RevokeInvite(ctx(), inv.ID, revokedAt); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("re-revoking = %v; want domain.ErrNotFound", err)
	}

	// Revoked invites stay listed so they can be recognised in settings.
	invites, err := s.ListInvites(ctx(), cal.ID)
	if err != nil || len(invites) != 1 {
		t.Fatalf("ListInvites = %+v, %v", invites, err)
	}
	if invites[0].RevokedAt == nil || !invites[0].RevokedAt.Equal(revokedAt) {
		t.Fatalf("revoked_at = %v; want %v", invites[0].RevokedAt, revokedAt)
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func TestEventCRUDTimed(t *testing.T) {
	s, clk, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	other := mustUser(t, s, "marc@example.test", "Marc")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)

	// 16:30 Paris on 4 August 2026 is 14:30 UTC.
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)
	e, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Dentiste", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		Location: "Cabinet", URL: "https://example.test/rdv", Notes: "Apporter la carte vitale",
		LabelID: label.ID, CreatedBy: u.ID, Participants: []int64{other.ID, u.ID, u.ID},
	}, nil)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if e.ID == 0 || e.AllDay || !e.StartsAt.Equal(starts) || !e.StartDate.IsZero() {
		t.Fatalf("CreateEvent = %+v", e)
	}
	if e.UpdatedBy != u.ID || e.CreatedAt.IsZero() || !e.CreatedAt.Equal(e.UpdatedAt) {
		t.Fatalf("audit columns = %+v", e)
	}
	if len(e.Participants) != 2 {
		t.Fatalf("participants = %v; want the duplicate collapsed", e.Participants)
	}

	got, err := s.EventByID(ctx(), e.ID)
	if err != nil {
		t.Fatalf("EventByID: %v", err)
	}
	if got.Title != "Dentiste" || got.Location != "Cabinet" || got.Notes == "" || got.RecurrenceID != nil {
		t.Fatalf("EventByID = %+v", got)
	}
	if len(got.Participants) != 2 {
		t.Fatalf("EventByID participants = %v", got.Participants)
	}

	clk.Advance(time.Hour)
	got.Title = "Dentiste (reporté)"
	got.UpdatedBy = other.ID
	if err := s.UpdateEvent(ctx(), got); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	after, _ := s.EventByID(ctx(), e.ID)
	if after.Title != "Dentiste (reporté)" || after.UpdatedBy != other.ID {
		t.Fatalf("UpdateEvent did not stick: %+v", after)
	}
	if !after.UpdatedAt.After(after.CreatedAt) {
		t.Fatalf("updated_at %v did not move past created_at %v", after.UpdatedAt, after.CreatedAt)
	}

	if err := s.SetParticipants(ctx(), e.ID, nil); err != nil {
		t.Fatalf("SetParticipants: %v", err)
	}
	if ids, _ := s.ListParticipants(ctx(), e.ID); len(ids) != 0 {
		t.Fatalf("participants after clearing = %v", ids)
	}
	if err := s.SetParticipants(ctx(), e.ID, []int64{other.ID}); err != nil {
		t.Fatalf("SetParticipants: %v", err)
	}
	if ids, _ := s.ListParticipants(ctx(), e.ID); len(ids) != 1 || ids[0] != other.ID {
		t.Fatalf("participants = %v", ids)
	}

	if err := s.DeleteEvent(ctx(), e.ID); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	if _, err := s.EventByID(ctx(), e.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("event survived deletion: %v", err)
	}
	if err := s.DeleteEvent(ctx(), e.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleting twice = %v; want domain.ErrNotFound", err)
	}
	if ids, _ := s.ListParticipants(ctx(), e.ID); len(ids) != 0 {
		t.Fatalf("participants outlived their event: %v", ids)
	}
}

func TestEventCRUDAllDay(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)

	start := domain.MustParseDate("2026-08-10")
	end := domain.MustParseDate("2026-08-12") // inclusive
	e, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Vacances", AllDay: true, StartDate: start, EndDate: end,
		LabelID: label.ID, CreatedBy: u.ID,
	}, nil)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if !e.AllDay || !e.StartDate.Equal(start) || !e.EndDate.Equal(end) {
		t.Fatalf("CreateEvent = %+v", e)
	}
	// An all-day event is dates only: never a midnight instant, which is the
	// off-by-one-day bug the schema exists to prevent.
	if !e.StartsAt.IsZero() || !e.EndsAt.IsZero() {
		t.Fatalf("all-day event carries instants: %+v", e)
	}

	got, err := s.EventByID(ctx(), e.ID)
	if err != nil || !got.AllDay || !got.StartDate.Equal(start) || !got.EndDate.Equal(end) {
		t.Fatalf("EventByID = %+v, %v", got, err)
	}
	if !got.StartsAt.IsZero() {
		t.Fatalf("all-day event read back with an instant: %+v", got)
	}
}

func TestEventCheckConstraintRejectsMalformed(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	instant := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	cases := []struct {
		name  string
		event domain.Event
	}{
		{"all-day carrying instants", domain.Event{
			CalendarID: cal.ID, Title: "Faux", AllDay: true,
			StartDate: domain.MustParseDate("2026-08-10"), EndDate: domain.MustParseDate("2026-08-10"),
			StartsAt: instant, EndsAt: instant.Add(time.Hour),
			LabelID: label.ID, CreatedBy: u.ID,
		}},
		{"all-day with no dates", domain.Event{
			CalendarID: cal.ID, Title: "Faux", AllDay: true,
			LabelID: label.ID, CreatedBy: u.ID,
		}},
		{"timed carrying dates", domain.Event{
			CalendarID: cal.ID, Title: "Faux",
			StartsAt: instant, EndsAt: instant.Add(time.Hour),
			StartDate: domain.MustParseDate("2026-08-10"), EndDate: domain.MustParseDate("2026-08-10"),
			LabelID: label.ID, CreatedBy: u.ID,
		}},
		{"timed with no instants", domain.Event{
			CalendarID: cal.ID, Title: "Faux",
			LabelID: label.ID, CreatedBy: u.ID,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreateEvent(ctx(), tc.event, nil); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("CreateEvent = %v; want domain.ErrInvalid from the CHECK constraint", err)
			}
		})
	}

	// A label from another calendar is a foreign key the schema can enforce, so it too
	// comes back as ErrInvalid rather than as a raw driver error.
	_, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Sans étiquette", StartsAt: instant, EndsAt: instant.Add(time.Hour),
		LabelID: 999999, CreatedBy: u.ID,
	}, nil)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CreateEvent with a bogus label = %v; want domain.ErrInvalid", err)
	}
}

func TestRecurrenceRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)

	until := domain.MustParseDate("2026-12-31")
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)
	e, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 2,
		ByWeekday: []time.Weekday{time.Friday, time.Tuesday, time.Tuesday},
		Until:     &until, DTStart: domain.MustParseDate("2026-08-04"),
	})
	if err != nil {
		t.Fatalf("CreateEvent with a recurrence: %v", err)
	}
	if e.RecurrenceID == nil {
		t.Fatal("CreateEvent did not link the recurrence")
	}

	r, err := s.RecurrenceByID(ctx(), *e.RecurrenceID)
	if err != nil {
		t.Fatalf("RecurrenceByID: %v", err)
	}
	if r.Freq != domain.FreqWeekly || r.Interval != 2 {
		t.Fatalf("recurrence = %+v", r)
	}
	// The weekday set is canonicalised: sorted, deduplicated.
	if len(r.ByWeekday) != 2 || r.ByWeekday[0] != time.Tuesday || r.ByWeekday[1] != time.Friday {
		t.Fatalf("by_weekday = %v; want [Tuesday Friday]", r.ByWeekday)
	}
	if r.Until == nil || !r.Until.Equal(until) {
		t.Fatalf("until = %v", r.Until)
	}

	// A monthly rule must pick exactly one mode; two is rejected by the schema.
	day := 15
	ord := 2
	_, err = s.CreateRecurrence(ctx(), domain.Recurrence{
		Freq: domain.FreqMonthly, Interval: 1, ByMonthday: &day, WeekOrdinal: &ord,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-08-04"),
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("ambiguous monthly recurrence = %v; want domain.ErrInvalid", err)
	}

	r.Interval = 3
	r.Until = nil
	if err := s.UpdateRecurrence(ctx(), r); err != nil {
		t.Fatalf("UpdateRecurrence: %v", err)
	}
	got, _ := s.RecurrenceByID(ctx(), r.ID)
	if got.Interval != 3 || got.Until != nil {
		t.Fatalf("UpdateRecurrence did not stick: %+v", got)
	}

	// Dropping the pattern leaves the template behind as a one-off event.
	if err := s.DeleteRecurrence(ctx(), r.ID); err != nil {
		t.Fatalf("DeleteRecurrence: %v", err)
	}
	orphan, err := s.EventByID(ctx(), e.ID)
	if err != nil {
		t.Fatalf("EventByID after DeleteRecurrence: %v", err)
	}
	if orphan.RecurrenceID != nil {
		t.Fatalf("recurrence_id = %v; ON DELETE SET NULL should have cleared it", orphan.RecurrenceID)
	}
}

func TestOverridesIncludingCancellation(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	series, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-08-04")})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	recID := *series.RecurrenceID

	if m, err := s.Overrides(ctx(), recID); err != nil || len(m) != 0 {
		t.Fatalf("Overrides on a fresh series = %v, %v; want an empty map", m, err)
	}

	// One occurrence moved: a standalone copy plus an override pointing at it.
	moved := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	copyEv, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine (plus tard)", StartsAt: moved, EndsAt: moved.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, nil)
	if err != nil {
		t.Fatalf("CreateEvent for the override: %v", err)
	}
	movedDate := domain.MustParseDate("2026-08-11")
	if err := s.SetOverride(ctx(), recID, movedDate, &copyEv.ID); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	// Another cancelled outright.
	cancelledDate := domain.MustParseDate("2026-08-18")
	if err := s.SetOverride(ctx(), recID, cancelledDate, nil); err != nil {
		t.Fatalf("SetOverride (cancel): %v", err)
	}

	overrides, err := s.Overrides(ctx(), recID)
	if err != nil {
		t.Fatalf("Overrides: %v", err)
	}
	if len(overrides) != 2 {
		t.Fatalf("Overrides = %v; want two entries", overrides)
	}
	if got, ok := overrides[movedDate]; !ok || got == nil || *got != copyEv.ID {
		t.Fatalf("override on %s = %v; want event %d", movedDate, got, copyEv.ID)
	}
	if got, ok := overrides[cancelledDate]; !ok || got != nil {
		t.Fatalf("override on %s = %v; want a nil value meaning cancelled", cancelledDate, got)
	}

	// Overwriting an override in place.
	if err := s.SetOverride(ctx(), recID, movedDate, nil); err != nil {
		t.Fatalf("SetOverride (replace): %v", err)
	}
	overrides, _ = s.Overrides(ctx(), recID)
	if got := overrides[movedDate]; got != nil {
		t.Fatalf("override on %s = %v after being replaced with a cancellation", movedDate, got)
	}

	// Restoring an occurrence, twice: the second is a no-op, not an error.
	if err := s.DeleteOverride(ctx(), recID, movedDate); err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	if err := s.DeleteOverride(ctx(), recID, movedDate); err != nil {
		t.Fatalf("DeleteOverride must be idempotent, got %v", err)
	}
	overrides, _ = s.Overrides(ctx(), recID)
	if _, ok := overrides[movedDate]; ok {
		t.Fatalf("override on %s survived deletion", movedDate)
	}

	// Overrides cascade with their series.
	if err := s.DeleteRecurrence(ctx(), recID); err != nil {
		t.Fatalf("DeleteRecurrence: %v", err)
	}
	if m, _ := s.Overrides(ctx(), recID); len(m) != 0 {
		t.Fatalf("%d overrides outlived their recurrence", len(m))
	}
}

// TestOverrideRefByEventID: the copy an occurrence edit leaves behind is what the API
// hands out and the client sends back, so the store has to be able to say which
// occurrence of which series that copy stands for.
func TestOverrideRefByEventID(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	series, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-08-04")})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	recID := *series.RecurrenceID

	// The copy is moved to the Thursday, so its own start date is not the date the
	// override is filed under. Only the override row knows that one.
	moved := time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC)
	copyEv, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine (déplacée)", StartsAt: moved, EndsAt: moved.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, nil)
	if err != nil {
		t.Fatalf("CreateEvent for the override: %v", err)
	}
	occDate := domain.MustParseDate("2026-08-11")
	if err := s.SetOverride(ctx(), recID, occDate, &copyEv.ID); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	ref, err := s.OverrideRefByEventID(ctx(), copyEv.ID)
	if err != nil {
		t.Fatalf("OverrideRefByEventID: %v", err)
	}
	if ref.RecurrenceID != recID || ref.SeriesEventID != series.ID || !ref.OccurrenceDate.Equal(occDate) {
		t.Errorf("ref = %+v; want recurrence %d, series event %d, %s", ref, recID, series.ID, occDate)
	}

	// The series template itself is not an override of anything, and neither is an
	// ordinary event.
	for _, id := range []int64{series.ID, series.ID + 1000} {
		if _, err := s.OverrideRefByEventID(ctx(), id); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("OverrideRefByEventID(%d) = %v; want ErrNotFound", id, err)
		}
	}

	// Detaching the override — what a series split does to a copy whose date the new
	// pattern no longer produces — makes it an ordinary event again.
	if err := s.DeleteOverride(ctx(), recID, occDate); err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	if _, err := s.OverrideRefByEventID(ctx(), copyEv.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a detached copy still resolves to a series: %v", err)
	}
}

func TestRepointOverrides(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	series, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-08-04")})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	oldRec := *series.RecurrenceID

	past := domain.MustParseDate("2026-08-04")
	splitDay := domain.MustParseDate("2026-08-18")
	future := domain.MustParseDate("2026-08-25")
	for _, d := range []domain.Date{past, splitDay, future} {
		if err := s.SetOverride(ctx(), oldRec, d, nil); err != nil {
			t.Fatalf("SetOverride %s: %v", d, err)
		}
	}

	// "This and following": a new series takes over from splitDay.
	newRec, err := s.CreateRecurrence(ctx(), domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: splitDay,
	})
	if err != nil {
		t.Fatalf("CreateRecurrence: %v", err)
	}
	if err := s.RepointOverrides(ctx(), oldRec, newRec.ID, splitDay); err != nil {
		t.Fatalf("RepointOverrides: %v", err)
	}

	oldOverrides, _ := s.Overrides(ctx(), oldRec)
	if len(oldOverrides) != 1 {
		t.Fatalf("old series kept %d overrides; want only the one before the split", len(oldOverrides))
	}
	if _, ok := oldOverrides[past]; !ok {
		t.Fatalf("the pre-split override moved: %v", oldOverrides)
	}

	newOverrides, _ := s.Overrides(ctx(), newRec.ID)
	if len(newOverrides) != 2 {
		t.Fatalf("new series has %d overrides; want the two on or after the split", len(newOverrides))
	}
	for _, d := range []domain.Date{splitDay, future} {
		if _, ok := newOverrides[d]; !ok {
			t.Fatalf("override on %s did not travel to the new series", d)
		}
	}

	// Running it again moves nothing and is not an error.
	if err := s.RepointOverrides(ctx(), oldRec, newRec.ID, splitDay); err != nil {
		t.Fatalf("RepointOverrides must be idempotent, got %v", err)
	}
	if err := s.RepointOverrides(ctx(), oldRec, newRec.ID, domain.Date{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("RepointOverrides with a zero date = %v; want domain.ErrInvalid", err)
	}
}

func TestEventsInRange(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	marc := mustUser(t, s, "marc@example.test", "Marc")
	cal := mustCalendar(t, s, u.ID, "Maison")
	other := mustCalendar(t, s, u.ID, "Autre")
	label := firstLabel(t, s, cal.ID)
	otherLabel := firstLabel(t, s, other.ID)

	// 16:30 Paris on 4 August = 14:30 UTC.
	timedStart := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)
	timed, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Dentiste", StartsAt: timedStart, EndsAt: timedStart.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID, Participants: []int64{marc.ID},
	}, nil)
	if err != nil {
		t.Fatalf("create timed event: %v", err)
	}

	allDay, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Vacances", AllDay: true,
		StartDate: domain.MustParseDate("2026-08-10"), EndDate: domain.MustParseDate("2026-08-12"),
		LabelID: label.ID, CreatedBy: u.ID,
	}, nil)
	if err != nil {
		t.Fatalf("create all-day event: %v", err)
	}

	// Outside the window.
	outsideStart := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	if _, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Rentrée", StartsAt: outsideStart, EndsAt: outsideStart.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, nil); err != nil {
		t.Fatalf("create out-of-window event: %v", err)
	}
	// In another calendar the query is not asking about.
	if _, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: other.ID, Title: "Ailleurs", StartsAt: timedStart, EndsAt: timedStart.Add(time.Hour),
		LabelID: otherLabel.ID, CreatedBy: u.ID,
	}, nil); err != nil {
		t.Fatalf("create event in the other calendar: %v", err)
	}

	// A weekly series with one moved occurrence and one cancelled one.
	seriesStart := time.Date(2026, 8, 4, 16, 0, 0, 0, time.UTC)
	series, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine", StartsAt: seriesStart, EndsAt: seriesStart.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID, Participants: []int64{u.ID, marc.ID},
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-08-04")})
	if err != nil {
		t.Fatalf("create series: %v", err)
	}
	recID := *series.RecurrenceID

	movedStart := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)
	movedEv, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine (20h)", StartsAt: movedStart, EndsAt: movedStart.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID, Participants: []int64{marc.ID},
	}, nil)
	if err != nil {
		t.Fatalf("create override event: %v", err)
	}
	if err := s.SetOverride(ctx(), recID, domain.MustParseDate("2026-08-11"), &movedEv.ID); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if err := s.SetOverride(ctx(), recID, domain.MustParseDate("2026-08-18"), nil); err != nil {
		t.Fatalf("SetOverride (cancel): %v", err)
	}

	// A series that ended before the window, and one that starts after it.
	endedUntil := domain.MustParseDate("2026-07-15")
	if _, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Fini", StartsAt: timedStart, EndsAt: timedStart.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Monday}, Until: &endedUntil,
		DTStart: domain.MustParseDate("2026-06-01")}); err != nil {
		t.Fatalf("create ended series: %v", err)
	}
	if _, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Plus tard", StartsAt: outsideStart, EndsAt: outsideStart.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Monday}, DTStart: domain.MustParseDate("2026-10-01")}); err != nil {
		t.Fatalf("create future series: %v", err)
	}

	from := domain.MustParseDate("2026-08-01")
	to := domain.MustParseDate("2026-08-31")
	res, err := s.EventsInRange(ctx(), []int64{cal.ID}, from, to)
	if err != nil {
		t.Fatalf("EventsInRange: %v", err)
	}

	// The override copy must not appear as a single: it belongs to its series.
	if len(res.Singles) != 2 {
		var titles []string
		for _, e := range res.Singles {
			titles = append(titles, e.Title)
		}
		t.Fatalf("Singles = %v; want the timed and all-day events only", titles)
	}
	singles := map[int64]domain.Event{}
	for _, e := range res.Singles {
		singles[e.ID] = e
	}
	if _, ok := singles[timed.ID]; !ok {
		t.Error("the timed event is missing from Singles")
	}
	if _, ok := singles[allDay.ID]; !ok {
		t.Error("the all-day event is missing from Singles")
	}
	if _, ok := singles[movedEv.ID]; ok {
		t.Error("the override copy leaked into Singles")
	}
	if got := singles[timed.ID].Participants; len(got) != 1 || got[0] != marc.ID {
		t.Errorf("participants of the timed single = %v", got)
	}

	if len(res.Series) != 1 {
		t.Fatalf("Series = %d rows; want only the one overlapping the window", len(res.Series))
	}
	row := res.Series[0]
	if row.Event.ID != series.ID || row.Recurrence.ID != recID {
		t.Fatalf("SeriesRow = %+v", row)
	}
	if len(row.Event.Participants) != 2 {
		t.Errorf("series template participants = %v", row.Event.Participants)
	}
	if len(row.Overrides) != 2 {
		t.Fatalf("Overrides = %v; want two", row.Overrides)
	}
	if id := row.Overrides[domain.MustParseDate("2026-08-11")]; id == nil || *id != movedEv.ID {
		t.Fatalf("moved override = %v", id)
	}
	if id, ok := row.Overrides[domain.MustParseDate("2026-08-18")]; !ok || id != nil {
		t.Fatalf("cancelled override = %v, present=%v", id, ok)
	}
	if len(row.OverrideEvents) != 1 {
		t.Fatalf("OverrideEvents = %v; want the one moved copy", row.OverrideEvents)
	}
	ov := row.OverrideEvents[movedEv.ID]
	if ov.Title != "Piscine (20h)" {
		t.Fatalf("override event = %+v", ov)
	}
	if len(ov.Participants) != 1 || ov.Participants[0] != marc.ID {
		t.Fatalf("override event participants = %v", ov.Participants)
	}

	// Window boundaries, in family-tz terms. 4 August 16:30 Paris is inside a window
	// that ends on the 4th, and outside one that ends on the 3rd — even though its
	// stored instant is 14:30 UTC.
	res, err = s.EventsInRange(ctx(), []int64{cal.ID}, domain.MustParseDate("2026-08-04"), domain.MustParseDate("2026-08-04"))
	if err != nil {
		t.Fatalf("EventsInRange: %v", err)
	}
	if len(res.Singles) != 1 || res.Singles[0].ID != timed.ID {
		t.Fatalf("single-day window = %+v; want just the dentist", res.Singles)
	}
	res, _ = s.EventsInRange(ctx(), []int64{cal.ID}, domain.MustParseDate("2026-08-05"), domain.MustParseDate("2026-08-09"))
	if len(res.Singles) != 0 {
		t.Fatalf("window after the dentist and before the holiday = %+v; want nothing", res.Singles)
	}
	// The all-day holiday spans 10-12 inclusive: a window covering only the 12th finds it.
	res, _ = s.EventsInRange(ctx(), []int64{cal.ID}, domain.MustParseDate("2026-08-12"), domain.MustParseDate("2026-08-12"))
	if len(res.Singles) != 1 || res.Singles[0].ID != allDay.ID {
		t.Fatalf("last day of the holiday = %+v; end_date must be inclusive", res.Singles)
	}

	// Degenerate inputs.
	if res, err := s.EventsInRange(ctx(), nil, from, to); err != nil || len(res.Singles) != 0 || len(res.Series) != 0 {
		t.Fatalf("EventsInRange with no calendars = %+v, %v", res, err)
	}
	if _, err := s.EventsInRange(ctx(), []int64{cal.ID}, to, from); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("inverted window = %v; want domain.ErrInvalid", err)
	}
}

func TestSearchIsAccentInsensitive(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	marc := mustUser(t, s, "marc@example.test", "Marc")
	cal := mustCalendar(t, s, u.ID, "Maison")
	labels, _ := s.ListLabels(ctx(), cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	mk := func(title, location, notes string, label domain.Label, participants ...int64) domain.Event {
		t.Helper()
		e, err := s.CreateEvent(ctx(), domain.Event{
			CalendarID: cal.ID, Title: title, Location: location, Notes: notes,
			StartsAt: starts, EndsAt: starts.Add(time.Hour),
			LabelID: label.ID, CreatedBy: u.ID, Participants: participants,
		}, nil)
		if err != nil {
			t.Fatalf("create event %q: %v", title, err)
		}
		return e
	}

	ecole := mk("Réunion parents", "École Jean Moulin", "", labels[1], marc.ID)
	noel := mk("Marché de Noël", "", "penser aux gâteaux", labels[2])
	mk("Football", "Stade", "", labels[3])

	find := func(q string, participant, label *int64) []int64 {
		t.Helper()
		events, err := s.SearchEvents(ctx(), []int64{cal.ID}, q, participant, label)
		if err != nil {
			t.Fatalf("SearchEvents(%q): %v", q, err)
		}
		ids := make([]int64, 0, len(events))
		for _, e := range events {
			ids = append(ids, e.ID)
		}
		return ids
	}

	cases := []struct {
		query string
		want  int64
	}{
		{"ecole", ecole.ID},   // unaccented query, accented data
		{"École", ecole.ID},   // accented query, accented data
		{"ECOLE", ecole.ID},   // and case-insensitively
		{"reunion", ecole.ID}, // title
		{"noel", noel.ID},     // title
		{"gateaux", noel.ID},  // notes are searched too
	}
	for _, tc := range cases {
		got := find(tc.query, nil, nil)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("SearchEvents(%q) = %v; want [%d]", tc.query, got, tc.want)
		}
	}

	// Filters.
	if got := find("", &marc.ID, nil); len(got) != 1 || got[0] != ecole.ID {
		t.Errorf("participant filter = %v; want [%d]", got, ecole.ID)
	}
	if got := find("", nil, &labels[2].ID); len(got) != 1 || got[0] != noel.ID {
		t.Errorf("label filter = %v; want [%d]", got, noel.ID)
	}
	if got := find("noel", &marc.ID, nil); len(got) != 0 {
		t.Errorf("mismatched filters = %v; want nothing", got)
	}
	if got := find("introuvable", nil, nil); len(got) != 0 {
		t.Errorf("no match = %v; want nothing", got)
	}

	// LIKE wildcards in the query are literal text, not a pattern.
	if got := find("%", nil, nil); len(got) != 0 {
		t.Errorf("SearchEvents(%q) = %v; the wildcard must be escaped", "%", got)
	}

	// A recurring series is its template, once — and its overrides do not show up as
	// separate hits.
	series, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: labels[4].ID, CreatedBy: u.ID,
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-08-04")})
	if err != nil {
		t.Fatalf("create series: %v", err)
	}
	copyEv := mk("Piscine reportée", "", "", labels[4])
	if err := s.SetOverride(ctx(), *series.RecurrenceID, domain.MustParseDate("2026-08-11"), &copyEv.ID); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if got := find("piscine", nil, nil); len(got) != 1 || got[0] != series.ID {
		t.Errorf("SearchEvents(\"piscine\") = %v; want just the template %d", got, series.ID)
	}
}

func TestFoldAccents(t *testing.T) {
	cases := []struct{ in, want string }{
		{"École", "ecole"},
		{"NOËL", "noel"},
		{"Cœur de Bœuf", "coeur de boeuf"},
		{"Ça où ?", "ca ou ?"},
		{"plain ascii", "plain ascii"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := foldSearch(tc.in); got != tc.want {
			t.Errorf("foldSearch(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	if got := searchNorm("Réunion", "École", "Gâteau"); got != "reunion ecole gateau" {
		t.Errorf("searchNorm = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Reminders, prefs, queue
// ---------------------------------------------------------------------------

func TestReplaceRemindersIsScopedToOneUser(t *testing.T) {
	s, _, _ := newStore(t)
	claire := mustUser(t, s, "claire@example.test", "Claire")
	marc := mustUser(t, s, "marc@example.test", "Marc")
	cal := mustCalendar(t, s, claire.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	e, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Dentiste", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: label.ID, CreatedBy: claire.ID,
	}, nil)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ten, sixty := 10, 60
	if err := s.ReplaceReminders(ctx(), &e.ID, nil, claire.ID, []domain.Reminder{
		{OffsetMinutes: &ten}, {OffsetMinutes: &sixty},
	}); err != nil {
		t.Fatalf("ReplaceReminders for Claire: %v", err)
	}
	if err := s.ReplaceReminders(ctx(), &e.ID, nil, marc.ID, []domain.Reminder{
		{OffsetMinutes: &sixty},
	}); err != nil {
		t.Fatalf("ReplaceReminders for Marc: %v", err)
	}

	claireRs, err := s.ListReminders(ctx(), &e.ID, nil, claire.ID)
	if err != nil || len(claireRs) != 2 {
		t.Fatalf("Claire's reminders = %+v, %v; want 2", claireRs, err)
	}
	for _, r := range claireRs {
		if r.UserID != claire.ID || r.EventID == nil || *r.EventID != e.ID {
			t.Fatalf("reminder scoped wrongly: %+v", r)
		}
	}

	// Replacing Claire's must leave Marc's alone.
	if err := s.ReplaceReminders(ctx(), &e.ID, nil, claire.ID, nil); err != nil {
		t.Fatalf("ReplaceReminders (clear): %v", err)
	}
	if rs, _ := s.ListReminders(ctx(), &e.ID, nil, claire.ID); len(rs) != 0 {
		t.Fatalf("Claire's reminders after clearing = %+v", rs)
	}
	marcRs, _ := s.ListReminders(ctx(), &e.ID, nil, marc.ID)
	if len(marcRs) != 1 {
		t.Fatalf("Marc's reminders = %+v; replacing Claire's must not touch them", marcRs)
	}

	// All-day reminders take the other shape.
	one := 1
	if err := s.ReplaceReminders(ctx(), &e.ID, nil, claire.ID, []domain.Reminder{
		{DaysBefore: &one, AtTimeLocal: "09:00"},
	}); err != nil {
		t.Fatalf("ReplaceReminders (all-day shape): %v", err)
	}
	rs, _ := s.ListReminders(ctx(), &e.ID, nil, claire.ID)
	if len(rs) != 1 || rs[0].DaysBefore == nil || *rs[0].DaysBefore != 1 || rs[0].AtTimeLocal != "09:00" {
		t.Fatalf("all-day reminder round trip = %+v", rs)
	}

	// Rejected shapes.
	if err := s.ReplaceReminders(ctx(), &e.ID, nil, claire.ID, []domain.Reminder{
		{OffsetMinutes: &ten, DaysBefore: &one, AtTimeLocal: "09:00"},
	}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("reminder with both shapes = %v; want domain.ErrInvalid", err)
	}
	if err := s.ReplaceReminders(ctx(), &e.ID, nil, claire.ID, []domain.Reminder{{}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("reminder with neither shape = %v; want domain.ErrInvalid", err)
	}
	if _, err := s.ListReminders(ctx(), &e.ID, &e.ID, claire.ID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("both scopes = %v; want domain.ErrInvalid", err)
	}
	if _, err := s.ListReminders(ctx(), nil, nil, claire.ID); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("no scope = %v; want domain.ErrInvalid", err)
	}

	// The planner sees everyone's.
	all, err := s.ListAllReminders(ctx())
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAllReminders = %+v, %v; want 2", all, err)
	}

	// Reminders cascade with their event.
	if err := s.DeleteEvent(ctx(), e.ID); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	if all, _ := s.ListAllReminders(ctx()); len(all) != 0 {
		t.Fatalf("%d reminders outlived their event", len(all))
	}
}

func TestPrefsDefaultsAndUpsert(t *testing.T) {
	s, _, _ := newStore(t)
	claire := mustUser(t, s, "claire@example.test", "Claire")
	marc := mustUser(t, s, "marc@example.test", "Marc")

	// Never saved: the schema defaults, including email reminders ON, because iOS push
	// dies silently and email is the parallel channel rather than a fallback.
	p, err := s.Prefs(ctx(), claire.ID)
	if err != nil {
		t.Fatalf("Prefs: %v", err)
	}
	if p.UserID != claire.ID || !p.DigestEnabled || p.DigestTime != "07:30" || !p.EmailReminders || !p.ActivityPush {
		t.Fatalf("default prefs = %+v", p)
	}
	if p.DigestOnEmpty || p.DailySummaryMode || p.EmailDigest || p.SummaryTime != "20:00" {
		t.Fatalf("default prefs = %+v", p)
	}
	if _, err := s.Prefs(ctx(), 999999); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Prefs for a missing user = %v; want domain.ErrNotFound", err)
	}

	p.DigestEnabled = false
	p.DigestTime = "06:45"
	p.EmailDigest = true
	if err := s.UpdatePrefs(ctx(), p); err != nil {
		t.Fatalf("UpdatePrefs (insert): %v", err)
	}
	p.SummaryTime = "21:15"
	if err := s.UpdatePrefs(ctx(), p); err != nil {
		t.Fatalf("UpdatePrefs (update): %v", err)
	}
	got, _ := s.Prefs(ctx(), claire.ID)
	if got.DigestEnabled || got.DigestTime != "06:45" || !got.EmailDigest || got.SummaryTime != "21:15" {
		t.Fatalf("prefs after upsert = %+v", got)
	}

	// The planner must see the user who never opened settings, with defaults.
	all, err := s.ListAllPrefs(ctx())
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAllPrefs = %+v, %v; want one row per user", all, err)
	}
	byUser := map[int64]domain.NotificationPrefs{}
	for _, p := range all {
		byUser[p.UserID] = p
	}
	if !byUser[marc.ID].DigestEnabled || byUser[marc.ID].DigestTime != "07:30" {
		t.Fatalf("Marc never saved prefs and must appear with defaults: %+v", byUser[marc.ID])
	}
	if byUser[claire.ID].DigestTime != "06:45" {
		t.Fatalf("Claire's saved prefs = %+v", byUser[claire.ID])
	}
}

func TestPushSubscriptions(t *testing.T) {
	s, _, _ := newStore(t)
	claire := mustUser(t, s, "claire@example.test", "Claire")
	marc := mustUser(t, s, "marc@example.test", "Marc")

	sub := domain.PushSubscription{
		UserID: claire.ID, Endpoint: "https://push.example.test/abc",
		P256DH: "key-1", Auth: "auth-1", UALabel: "iPhone",
	}
	if err := s.UpsertPushSubscription(ctx(), sub); err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}
	// The client re-registers on every app open; the endpoint is the identity.
	sub.P256DH = "key-2"
	sub.UALabel = "iPhone 16"
	if err := s.UpsertPushSubscription(ctx(), sub); err != nil {
		t.Fatalf("UpsertPushSubscription (repeat): %v", err)
	}

	subs, err := s.ListPushSubscriptions(ctx(), claire.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("ListPushSubscriptions = %+v, %v; want one row", subs, err)
	}
	if subs[0].P256DH != "key-2" || subs[0].UALabel != "iPhone 16" {
		t.Fatalf("upsert did not refresh the keys: %+v", subs[0])
	}
	id := subs[0].ID

	if err := s.MarkPushFailure(ctx(), id); err != nil {
		t.Fatalf("MarkPushFailure: %v", err)
	}
	if subs, _ := s.ListPushSubscriptions(ctx(), claire.ID); subs[0].Failures != 1 {
		t.Fatalf("failures = %d; want 1", subs[0].Failures)
	}
	okAt := baseTime.Add(time.Hour)
	if err := s.MarkPushOK(ctx(), id, okAt); err != nil {
		t.Fatalf("MarkPushOK: %v", err)
	}
	subs, _ = s.ListPushSubscriptions(ctx(), claire.ID)
	if subs[0].Failures != 0 || !subs[0].LastOKAt.Equal(okAt) {
		t.Fatalf("after MarkPushOK: %+v", subs[0])
	}

	confirmedAt := baseTime.Add(2 * time.Hour)
	if err := s.ConfirmPushSubscription(ctx(), sub.Endpoint, confirmedAt); err != nil {
		t.Fatalf("ConfirmPushSubscription: %v", err)
	}
	subs, _ = s.ListPushSubscriptions(ctx(), claire.ID)
	if !subs[0].LastConfirmedAt.Equal(confirmedAt) {
		t.Fatalf("last_confirmed_at = %v; want %v", subs[0].LastConfirmedAt, confirmedAt)
	}
	if err := s.ConfirmPushSubscription(ctx(), "https://push.example.test/unknown", confirmedAt); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("confirming an unknown endpoint = %v; want domain.ErrNotFound", err)
	}

	if err := s.UpsertPushSubscription(ctx(), domain.PushSubscription{
		UserID: marc.ID, Endpoint: "https://push.example.test/xyz", P256DH: "k", Auth: "a",
	}); err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}
	all, err := s.ListAllPushSubscriptions(ctx())
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAllPushSubscriptions = %+v, %v; want 2", all, err)
	}

	// A 410 from the push service can race with another delivery attempt, so pruning
	// twice must be fine.
	if err := s.DeletePushSubscription(ctx(), sub.Endpoint); err != nil {
		t.Fatalf("DeletePushSubscription: %v", err)
	}
	if err := s.DeletePushSubscription(ctx(), sub.Endpoint); err != nil {
		t.Fatalf("DeletePushSubscription must be idempotent, got %v", err)
	}
	if subs, _ := s.ListPushSubscriptions(ctx(), claire.ID); len(subs) != 0 {
		t.Fatalf("subscription survived deletion: %+v", subs)
	}
}

func TestNotificationQueue(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")

	due := baseTime.Add(time.Hour)
	q := domain.QueuedNotification{
		UserID: u.ID, Kind: domain.KindReminder,
		SourceRef: "reminder:12:2026-08-04", Payload: `{"title":"Dentiste"}`, DueAt: due,
	}

	// Idempotency is structural: the planner recomputes the same window every tick.
	for range 3 {
		if err := s.EnqueueNotification(ctx(), q); err != nil {
			t.Fatalf("EnqueueNotification: %v", err)
		}
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM notification_queue`).Scan(&n); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	if n != 1 {
		t.Fatalf("enqueueing the same source_ref three times inserted %d rows; want 1", n)
	}

	// A different due_at is a different notification (the occurrence moved).
	moved := q
	moved.DueAt = due.Add(30 * time.Minute)
	if err := s.EnqueueNotification(ctx(), moved); err != nil {
		t.Fatalf("EnqueueNotification: %v", err)
	}

	// Nothing is due yet.
	if got, err := s.DueNotifications(ctx(), baseTime, 10); err != nil || len(got) != 0 {
		t.Fatalf("DueNotifications before the slot = %+v, %v", got, err)
	}
	got, err := s.DueNotifications(ctx(), due.Add(time.Hour), 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("DueNotifications = %+v, %v; want 2", got, err)
	}
	if !got[0].DueAt.Before(got[1].DueAt) {
		t.Fatalf("DueNotifications is not oldest-first: %v then %v", got[0].DueAt, got[1].DueAt)
	}
	if got[0].Kind != domain.KindReminder || got[0].Payload != `{"title":"Dentiste"}` {
		t.Fatalf("queued row = %+v", got[0])
	}
	if limited, _ := s.DueNotifications(ctx(), due.Add(time.Hour), 1); len(limited) != 1 {
		t.Fatalf("limit is not honoured: %d rows", len(limited))
	}

	first, second := got[0].ID, got[1].ID
	sendAt := due.Add(time.Hour)
	if err := s.MarkSending(ctx(), first, sendAt); err != nil {
		t.Fatalf("MarkSending: %v", err)
	}
	if err := s.MarkSent(ctx(), first, sendAt); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if err := s.MarkSkipped(ctx(), second, "event already passed", sendAt); err != nil {
		t.Fatalf("MarkSkipped: %v", err)
	}
	if got, _ := s.DueNotifications(ctx(), sendAt, 10); len(got) != 0 {
		t.Fatalf("sent and skipped rows came back as due: %+v", got)
	}

	var sentAt, skipped, sending any
	if err := s.DB().QueryRow(
		`SELECT sent_at, skipped, sending_started_at FROM notification_queue WHERE id = ?`, second,
	).Scan(&sentAt, &skipped, &sending); err != nil {
		t.Fatalf("read skipped row: %v", err)
	}
	if sentAt != nil {
		// sent_at means a provider accepted the message. A skip is not a send.
		t.Fatalf("MarkSkipped set sent_at to %v", sentAt)
	}
	if skipped == nil || sending == nil {
		t.Fatalf("skipped row = reason %v, sending_started_at %v", skipped, sending)
	}

	// Boot catch-up.
	unsent, err := s.ListUnsentBefore(ctx(), sendAt)
	if err != nil {
		t.Fatalf("ListUnsentBefore: %v", err)
	}
	if len(unsent) != 0 {
		t.Fatalf("ListUnsentBefore = %+v; the only two rows are sent and skipped", unsent)
	}
	q.SourceRef = "reminder:12:2026-08-05"
	q.DueAt = due.Add(2 * time.Hour)
	if err := s.EnqueueNotification(ctx(), q); err != nil {
		t.Fatalf("EnqueueNotification: %v", err)
	}
	if unsent, _ = s.ListUnsentBefore(ctx(), q.DueAt.Add(time.Minute)); len(unsent) != 1 {
		t.Fatalf("ListUnsentBefore = %+v; want the one fresh row", unsent)
	}
}

func TestDeleteUnsentBySourcePrefix(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")

	enqueue := func(ref string, offset time.Duration) {
		t.Helper()
		if err := s.EnqueueNotification(ctx(), domain.QueuedNotification{
			UserID: u.ID, Kind: domain.KindReminder, SourceRef: ref, Payload: "{}",
			DueAt: baseTime.Add(offset),
		}); err != nil {
			t.Fatalf("EnqueueNotification %s: %v", ref, err)
		}
	}
	enqueue("reminder:12:2026-08-04", time.Hour)
	enqueue("reminder:12:2026-08-11", 2*time.Hour)
	enqueue("reminder:99:2026-08-04", 3*time.Hour)
	enqueue("digest:2026-08-04", 4*time.Hour)

	// A sent row is history and must survive the prune.
	sent, err := s.DueNotifications(ctx(), baseTime.Add(90*time.Minute), 10)
	if err != nil || len(sent) != 1 {
		t.Fatalf("DueNotifications = %+v, %v", sent, err)
	}
	if err := s.MarkSent(ctx(), sent[0].ID, baseTime.Add(90*time.Minute)); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	n, err := s.DeleteUnsentBySourcePrefix(ctx(), "reminder:12:")
	if err != nil {
		t.Fatalf("DeleteUnsentBySourcePrefix: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows; want only the unsent reminder for event 12", n)
	}

	var refs []string
	rows, err := s.DB().Query(`SELECT source_ref FROM notification_queue ORDER BY source_ref`)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("scan: %v", err)
		}
		refs = append(refs, ref)
	}
	want := []string{"digest:2026-08-04", "reminder:12:2026-08-04", "reminder:99:2026-08-04"}
	if len(refs) != len(want) {
		t.Fatalf("queue = %v; want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("queue = %v; want %v", refs, want)
		}
	}

	// The prefix is literal: an underscore is not a single-character wildcard.
	if n, err := s.DeleteUnsentBySourcePrefix(ctx(), "reminder_"); err != nil || n != 0 {
		t.Fatalf("DeleteUnsentBySourcePrefix(%q) = %d, %v; wildcards must be escaped", "reminder_", n, err)
	}
}

// ---------------------------------------------------------------------------
// Activity, holidays, meta
// ---------------------------------------------------------------------------

func TestActivityLog(t *testing.T) {
	s, clk, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	other := mustCalendar(t, s, u.ID, "Autre")

	eventID := int64(42)
	for i, title := range []string{"Dentiste", "Piscine", "Vacances"} {
		clk.Set(baseTime.Add(time.Duration(i) * time.Hour))
		if err := s.LogActivity(ctx(), domain.Activity{
			CalendarID: cal.ID, UserID: u.ID, Action: domain.ActionEventCreated,
			EventID: &eventID, Title: title,
		}); err != nil {
			t.Fatalf("LogActivity: %v", err)
		}
	}
	clk.Set(baseTime.Add(4 * time.Hour))
	if err := s.LogActivity(ctx(), domain.Activity{
		CalendarID: other.ID, UserID: u.ID, Action: domain.ActionMemberJoined, Title: "Marc",
	}); err != nil {
		t.Fatalf("LogActivity: %v", err)
	}

	entries, err := s.ListActivity(ctx(), []int64{cal.ID}, 10, time.Time{})
	if err != nil || len(entries) != 3 {
		t.Fatalf("ListActivity = %+v, %v; want 3", entries, err)
	}
	if entries[0].Title != "Vacances" {
		t.Fatalf("ListActivity is not newest-first: %+v", entries)
	}
	if entries[0].EventID == nil || *entries[0].EventID != eventID {
		t.Fatalf("event_id = %v", entries[0].EventID)
	}

	// Pagination cursor.
	older, err := s.ListActivity(ctx(), []int64{cal.ID}, 10, entries[0].At)
	if err != nil || len(older) != 2 {
		t.Fatalf("ListActivity before the newest = %+v, %v; want 2", older, err)
	}
	if limited, _ := s.ListActivity(ctx(), []int64{cal.ID}, 1, time.Time{}); len(limited) != 1 {
		t.Fatal("limit is not honoured")
	}

	// Several calendars at once, which is how the feed is actually read.
	both, err := s.ListActivity(ctx(), []int64{cal.ID, other.ID}, 10, time.Time{})
	if err != nil || len(both) != 4 {
		t.Fatalf("ListActivity across calendars = %+v, %v; want 4", both, err)
	}
	if entries, _ := s.ListActivity(ctx(), nil, 10, time.Time{}); len(entries) != 0 {
		t.Fatalf("ListActivity with no calendars = %+v", entries)
	}
}

func TestHolidayOverridesAndMeta(t *testing.T) {
	s, _, _ := newStore(t)

	if m, err := s.HolidayOverrides(ctx()); err != nil || len(m) != 0 {
		t.Fatalf("HolidayOverrides on a fresh database = %v, %v", m, err)
	}

	added := domain.MustParseDate("2027-05-08")
	suppressed := domain.MustParseDate("2027-07-14")
	name := "Victoire 1945"
	if err := s.SetHolidayOverride(ctx(), added, &name); err != nil {
		t.Fatalf("SetHolidayOverride: %v", err)
	}
	if err := s.SetHolidayOverride(ctx(), suppressed, nil); err != nil {
		t.Fatalf("SetHolidayOverride (suppress): %v", err)
	}

	m, err := s.HolidayOverrides(ctx())
	if err != nil || len(m) != 2 {
		t.Fatalf("HolidayOverrides = %v, %v", m, err)
	}
	if got := m[added]; got == nil || *got != name {
		t.Fatalf("override for %s = %v; want %q", added, got, name)
	}
	if got, ok := m[suppressed]; !ok || got != nil {
		t.Fatalf("override for %s = %v, present=%v; a nil name suppresses", suppressed, got, ok)
	}

	// Upsert.
	renamed := "Victoire"
	if err := s.SetHolidayOverride(ctx(), added, &renamed); err != nil {
		t.Fatalf("SetHolidayOverride (rename): %v", err)
	}
	m, _ = s.HolidayOverrides(ctx())
	if got := m[added]; got == nil || *got != renamed {
		t.Fatalf("override after rename = %v", got)
	}
	if err := s.SetHolidayOverride(ctx(), domain.Date{}, &name); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("SetHolidayOverride with a zero date = %v; want domain.ErrInvalid", err)
	}

	// Meta: a missing key is "" and no error.
	v, err := s.GetMeta(ctx(), "planner_horizon")
	if err != nil || v != "" {
		t.Fatalf("GetMeta of a missing key = %q, %v; want \"\", nil", v, err)
	}
	if err := s.SetMeta(ctx(), "planner_horizon", "2026-08-06T00:00:00Z"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := s.SetMeta(ctx(), "planner_horizon", "2026-08-07T00:00:00Z"); err != nil {
		t.Fatalf("SetMeta (upsert): %v", err)
	}
	if v, _ = s.GetMeta(ctx(), "planner_horizon"); v != "2026-08-07T00:00:00Z" {
		t.Fatalf("GetMeta = %q", v)
	}
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// TestOpenWithAwkwardPath keeps the DSN builder honest: a data directory containing a
// '?' or a '#' would otherwise be cut in half by SQLite's URI parser, and the database
// would silently be created somewhere else.
func TestOpenWithAwkwardPath(t *testing.T) {
	if got, want := dsn("/data/a?b#c%d.db"), "file:/data/a%3fb%23c%25d.db?"; !strings.HasPrefix(got, want) {
		t.Errorf("dsn = %q; want it to start %q", got, want)
	}

	dir := filepath.Join(t.TempDir(), "agen#da ?v2 100%")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "almanack.db")

	s, err := Open(path, testLocation(t), clock.NewFake(baseTime))
	if err != nil {
		t.Fatalf("Open with an awkward path: %v", err)
	}
	defer s.Close()

	if _, err := s.CreateUser(ctx(), domain.User{
		Email: "claire@example.test", DisplayName: "Claire", Color: "#336699",
		Lang: domain.LangFR, TimeFormat: "24h",
	}, "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the database was not created at %s: %v", path, err)
	}
}

func TestPingAndDB(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.Ping(ctx()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if s.DB() == nil {
		t.Fatal("DB() returned nil; the backup subcommand needs it for VACUUM INTO")
	}
	if s.Location().String() != "Europe/Paris" {
		t.Fatalf("Location = %s", s.Location())
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s, _, _ := newStore(t)
	// The pragma verified at Open is load-bearing: every ON DELETE CASCADE in the
	// schema depends on it, and SQLite leaves it off by default.
	if _, err := s.DB().Exec(
		`INSERT INTO calendar_members (calendar_id, user_id, joined_at) VALUES (9999, 8888, ?)`,
		mustInstant(baseTime)); err == nil {
		t.Fatal("a membership referencing no calendar and no user was accepted")
	}
}

// TestConcurrentWritesAndReads guards the pool decision documented on maxOpenConns:
// several connections, WAL, BEGIN IMMEDIATE and a busy timeout. Each iteration is a
// multi-statement write transaction interleaved with the main range read, which is the
// shape that would surface a lock upgrade failure or a pool deadlock.
func TestConcurrentWritesAndReads(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	const writers, each = 8, 5
	errs := make(chan error, writers*each)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				_, err := s.CreateEvent(ctx(), domain.Event{
					CalendarID: cal.ID, Title: "Piscine", StartsAt: starts, EndsAt: starts.Add(time.Hour),
					LabelID: label.ID, CreatedBy: u.ID, Participants: []int64{u.ID},
				}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
					ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-08-04")})
				if err != nil {
					errs <- err
					return
				}
				if _, err := s.EventsInRange(ctx(), []int64{cal.ID},
					domain.MustParseDate("2026-08-01"), domain.MustParseDate("2026-08-31")); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent access: %v", err)
	}

	res, err := s.EventsInRange(ctx(), []int64{cal.ID},
		domain.MustParseDate("2026-08-01"), domain.MustParseDate("2026-08-31"))
	if err != nil {
		t.Fatalf("EventsInRange: %v", err)
	}
	if len(res.Series) != writers*each {
		t.Fatalf("wrote %d series, read back %d", writers*each, len(res.Series))
	}
}

func TestInstantsRoundTripAtSecondPrecision(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")

	// Instants are stored as RFC 3339 with no fractional part, so sub-second detail is
	// dropped rather than silently mis-sorted.
	expires := time.Date(2026, 8, 4, 14, 30, 45, 123456789, time.UTC)
	sess, err := s.CreateSession(ctx(), u.ID, "hash", expires)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if want := expires.Truncate(time.Second); !sess.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v; want %v", sess.ExpiresAt, want)
	}
	if sess.ExpiresAt.Location() != time.UTC {
		t.Fatalf("instants must come back in UTC, got %v", sess.ExpiresAt.Location())
	}
}
