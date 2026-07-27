package store

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"

	// The upgrade test reads the fixture's data back the way the application does:
	// auth to prove a password hash still verifies, recur to prove a series still
	// expands. Both are pure and neither imports this package.
	"almanack/internal/auth"
	"almanack/internal/recur"
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
// Transactions
// ---------------------------------------------------------------------------

// errInterrupted stands in for whatever ends a sequence of writes half-way through: a
// phone that lost its signal, a process that was killed, a statement that failed.
var errInterrupted = errors.New("interrupted")

// TestInTxRollsBackEveryWrite is the property internal/events relies on for scoped
// edits: several writes through ordinary Store methods, and either all of them or none.
func TestInTxRollsBackEveryWrite(t *testing.T) {
	s, _, _ := newStore(t)
	claire := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, claire.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	err := s.InTx(ctx(), func(tx *Store) error {
		e, err := tx.CreateEvent(ctx(), domain.Event{
			CalendarID: cal.ID, Title: "Dentiste", StartsAt: starts, EndsAt: starts.Add(time.Hour),
			LabelID: label.ID, CreatedBy: claire.ID, Participants: []int64{claire.ID},
		}, nil)
		if err != nil {
			return err
		}
		ten := 10
		if err := tx.ReplaceReminders(ctx(), &e.ID, nil, claire.ID, []domain.Reminder{{OffsetMinutes: &ten}}); err != nil {
			return err
		}
		if err := tx.LogActivity(ctx(), domain.Activity{
			CalendarID: cal.ID, UserID: claire.ID, Action: domain.ActionEventCreated,
			EventID: &e.ID, Title: e.Title,
		}); err != nil {
			return err
		}
		// Everything above is visible to this transaction and to nothing else.
		if got, err := tx.EventByID(ctx(), e.ID); err != nil || got.Title != "Dentiste" {
			t.Errorf("inside the transaction, EventByID = %+v, %v", got, err)
		}
		return errInterrupted
	})
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("InTx = %v; want the error fn returned", err)
	}

	res, err := s.EventsInRange(ctx(), []int64{cal.ID},
		domain.MustParseDate("2026-08-01"), domain.MustParseDate("2026-08-31"))
	if err != nil {
		t.Fatalf("EventsInRange: %v", err)
	}
	if len(res.Singles) != 0 {
		t.Errorf("%d events survived a rolled-back transaction", len(res.Singles))
	}
	if all, err := s.ListAllReminders(ctx()); err != nil || len(all) != 0 {
		t.Errorf("%d reminders survived a rolled-back transaction (%v)", len(all), err)
	}
	// The activity log is where change notifications are planned from, so a row left
	// behind here would announce an edit that never happened.
	if rows, err := s.ListActivity(ctx(), []int64{cal.ID}, 10, 0); err != nil || len(rows) != 0 {
		t.Errorf("%d activity rows survived a rolled-back transaction (%v)", len(rows), err)
	}

	// And the Store is still usable afterwards: a rollback releases the connection.
	if _, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Dentiste", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: label.ID, CreatedBy: claire.ID,
	}, nil); err != nil {
		t.Fatalf("CreateEvent after a rollback: %v", err)
	}
}

// TestInTxCommitsEveryWrite is the other half: nothing is left in the transaction.
func TestInTxCommitsEveryWrite(t *testing.T) {
	s, _, _ := newStore(t)
	claire := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, claire.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	var id int64
	if err := s.InTx(ctx(), func(tx *Store) error {
		e, err := tx.CreateEvent(ctx(), domain.Event{
			CalendarID: cal.ID, Title: "Dentiste", StartsAt: starts, EndsAt: starts.Add(time.Hour),
			LabelID: label.ID, CreatedBy: claire.ID, Participants: []int64{claire.ID},
		}, nil)
		id = e.ID
		return err
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}
	got, err := s.EventByID(ctx(), id)
	if err != nil || got.Title != "Dentiste" || len(got.Participants) != 1 {
		t.Fatalf("after commit, EventByID = %+v, %v", got, err)
	}
}

// TestInTxNestsWithoutDeadlocking is the hazard a transaction-scoped Store is built to
// remove. The pool holds four connections and every write transaction takes SQLite's
// write lock at BEGIN, so a Store method that began a *second* transaction while inside
// one would sit waiting for a lock its own caller is holding — five seconds of
// busy_timeout and then a failure, or worse under load.
//
// So the methods that use a transaction internally join the one already open, and InTx
// inside InTx is the same transaction rather than a new one. The deadline is what makes
// a regression here fail in seconds instead of hanging until the test binary is killed.
func TestInTxNestsWithoutDeadlocking(t *testing.T) {
	s, _, _ := newStore(t)
	claire := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, claire.ID, "Maison")
	label := firstLabel(t, s, cal.ID)
	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)

	deadline, cancel := context.WithTimeout(ctx(), 20*time.Second)
	defer cancel()

	var id int64
	err := s.InTx(deadline, func(tx *Store) error {
		// CreateEvent, SetParticipants and ReplaceReminders each run a transaction of
		// their own when called on the pool.
		e, err := tx.CreateEvent(deadline, domain.Event{
			CalendarID: cal.ID, Title: "Dentiste", StartsAt: starts, EndsAt: starts.Add(time.Hour),
			LabelID: label.ID, CreatedBy: claire.ID,
		}, nil)
		if err != nil {
			return err
		}
		id = e.ID
		if err := tx.SetParticipants(deadline, e.ID, []int64{claire.ID}); err != nil {
			return err
		}
		ten := 10
		if err := tx.ReplaceReminders(deadline, &e.ID, nil, claire.ID, []domain.Reminder{{OffsetMinutes: &ten}}); err != nil {
			return err
		}
		// And a nested InTx, which is what a service calling one of its own flows from
		// inside another would produce.
		return tx.InTx(deadline, func(inner *Store) error {
			return inner.UpdateEvent(deadline, e)
		})
	})
	if err != nil {
		t.Fatalf("nested writes inside InTx: %v", err)
	}
	got, err := s.EventByID(ctx(), id)
	if err != nil || len(got.Participants) != 1 {
		t.Fatalf("after nested writes, EventByID = %+v, %v", got, err)
	}
	if all, err := s.ListAllReminders(ctx()); err != nil || len(all) != 1 {
		t.Fatalf("reminders after nested writes = %+v, %v", all, err)
	}
}

// TestInTxRollsBackWhatANestedCallWrote pins the consequence of joining rather than
// nesting: an inner InTx that succeeds is not committed on its own, because there is
// only one transaction and the outer one decides.
func TestInTxRollsBackWhatANestedCallWrote(t *testing.T) {
	s, _, _ := newStore(t)
	claire := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, claire.ID, "Maison")

	err := s.InTx(ctx(), func(tx *Store) error {
		if err := tx.InTx(ctx(), func(inner *Store) error {
			return inner.UpdateCalendar(ctx(), domain.Calendar{ID: cal.ID, Name: "Chalet", Color: "#123456"})
		}); err != nil {
			return err
		}
		return errInterrupted
	})
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("InTx = %v; want the error fn returned", err)
	}
	got, err := s.CalendarByID(ctx(), cal.ID)
	if err != nil {
		t.Fatalf("CalendarByID: %v", err)
	}
	if got.Name != "Maison" {
		t.Errorf("calendar name = %q; a nested transaction committed on its own", got.Name)
	}
}

// ---------------------------------------------------------------------------
// Upgrading a database written by an earlier release
// ---------------------------------------------------------------------------

// The failure this section exists to catch is the one that cannot be caught by looking
// at a diff: a migration that applies cleanly to an empty database and quietly damages
// a full one. TestMigrationsAreIdempotent reopens a database this binary itself created
// a moment ago, which proves only that head can open head. What a household actually
// does is run 0.2.0 for a year and then drop a newer binary on top of the file, and
// nothing here proved that worked until this test.
//
// So: replay a database a shipped release really wrote, open it with the current
// binary, and check every row is still there and still means the same thing.

// releaseFixture is one such database, dumped to SQL text under testdata/.
type releaseFixture struct {
	// release is the version of the binary that wrote the file.
	release string
	file    string
	// version is the schema version the file was captured at — a fact about a release
	// that has already shipped, not a number anyone should keep current. Pinning it
	// here is what stops this test going quietly vacuous; see the guard below.
	version int
	// check reads the fixture's data back through the store API. Each fixture knows
	// its own family, so each brings its own.
	check func(t *testing.T, s *Store)
}

var releaseFixtures = []releaseFixture{
	{release: "v0.2.0", file: "testdata/v0.2.0.sql", version: 2, check: checkV020Family},
}

// upgradedAt is when the upgrade is pretended to happen: after the v0.2.0 fixture was
// captured (2026-07-27) and before the live invite in it expires (2026-08-03), so that
// the rows carrying deadlines can be read back through the API that enforces them.
var upgradedAt = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

func TestUpgradeFromReleasedDatabase(t *testing.T) {
	embedded, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	head := embedded[len(embedded)-1].version

	for _, fx := range releaseFixtures {
		t.Run(fx.release, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "almanack.db")

			// Load the dump on a bare connection. SQLite leaves foreign keys off
			// unless asked, so the file replays without any ordering ceremony; Open
			// turns enforcement on afterwards, which is what gives the
			// foreign_key_check below something to say.
			text, err := os.ReadFile(fx.file)
			if err != nil {
				t.Fatalf("read %s: %v", fx.file, err)
			}
			raw := openRawDB(t, path)
			if _, err := raw.Exec(string(text)); err != nil {
				t.Fatalf("replay %s: %v", fx.file, err)
			}
			tables := userTables(t, raw)
			before := rowCounts(t, raw, tables)
			fixtureApplied := appliedMigrations(t, raw)
			// Close before Open: the dump loaded in rollback-journal mode and a live
			// connection would keep Open from switching the file to WAL.
			if err := raw.Close(); err != nil {
				t.Fatalf("close raw handle: %v", err)
			}

			// --- the guard that keeps this test honest ---------------------------
			//
			// The fixture's whole value is that it is old. Regenerate it against a
			// newer head — the obvious thing to do when a migration breaks it — and
			// this becomes a slower copy of TestMigrationsAreIdempotent that nobody
			// notices has stopped testing anything. So the version is pinned in the
			// table above and compared against the file: move the file and this
			// fails, loudly, saying what to do instead.
			//
			// What is deliberately *not* asserted is that head is strictly ahead of
			// the fixture. Both releases so far ship schema 0002, so today this
			// exercises an empty upgrade path and proves the weaker "a v0.2.0
			// database opens, unchanged, under the current binary". The moment 0003
			// lands it becomes a real 0002 → 0003 upgrade, and every migration after
			// that joins in, with nothing here to edit. Failing on equality instead
			// would mean this test could not be written until the next migration was.
			fixtureVersion := maxVersion(fixtureApplied)
			if fixtureVersion != fx.version {
				t.Fatalf("%s is at schema version %d but the table says %d.\n"+
					"If the fixture was regenerated at head, put it back: it is meant to stay at %d, "+
					"and a fixture at head turns this test into a no-op. A newer release wanting its "+
					"own fixture should add a second file and a second row in releaseFixtures.",
					fx.file, fixtureVersion, fx.version, fx.version)
			}
			if head < fx.version {
				t.Fatalf("this binary knows migrations up to %d but %s was written at %d; "+
					"an applied migration has been deleted rather than superseded (CONVENTIONS.md §8)",
					head, fx.file, fx.version)
			}
			if head == fx.version {
				t.Logf("head is still schema %d, the version %s shipped: no migration is being "+
					"exercised yet, only that the data survives being reopened", head, fx.release)
			}

			// --- the upgrade -----------------------------------------------------
			s, err := Open(path, testLocation(t), clock.NewFake(upgradedAt))
			if err != nil {
				t.Fatalf("open a %s database with this binary: %v", fx.release, err)
			}
			defer s.Close()

			applied := appliedMigrations(t, s.DB())
			for _, m := range embedded {
				if _, ok := applied[m.version]; !ok {
					t.Errorf("migration %s was not applied to the %s database", m.name, fx.release)
				}
			}
			if len(applied) != len(embedded) {
				t.Errorf("schema_migrations holds %d rows, want the %d embedded migrations",
					len(applied), len(embedded))
			}
			// The rows the old binary wrote keep their original timestamps: a
			// migration run must add rows, never rewrite the record of what came
			// before it.
			for v, at := range fixtureApplied {
				if applied[v] != at {
					t.Errorf("schema_migrations row %d says applied_at %q, was %q before the upgrade",
						v, applied[v], at)
				}
			}

			// --- the file is still a sound database ------------------------------
			if got := pragmaRows(t, s.DB(), `PRAGMA integrity_check`); len(got) != 1 || got[0] != "ok" {
				t.Errorf("integrity_check after upgrade = %v; want [ok]", got)
			}
			if got := pragmaRows(t, s.DB(), `PRAGMA foreign_key_check`); len(got) != 0 {
				t.Errorf("foreign_key_check after upgrade reported %d violation(s): %v", len(got), got)
			}

			// --- nothing was lost ------------------------------------------------
			//
			// schema_migrations is the one table expected to grow. Every other table
			// must come out of the upgrade with exactly the rows it went in with:
			// expand/contract migrations add columns and tables, they do not touch
			// the family's rows. A migration that legitimately backfills data will
			// fail here, which is the right moment to argue for it out loud.
			after := rowCounts(t, s.DB(), tables)
			for _, tbl := range tables {
				if tbl == "schema_migrations" {
					continue
				}
				if after[tbl] != before[tbl] {
					t.Errorf("table %s has %d rows after the upgrade, had %d before",
						tbl, after[tbl], before[tbl])
				}
			}

			// --- and it still reads correctly ------------------------------------
			fx.check(t, s)

			// --- reopening changes nothing ---------------------------------------
			if err := s.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			s2, err := Open(path, testLocation(t), clock.NewFake(upgradedAt.Add(24*time.Hour)))
			if err != nil {
				t.Fatalf("reopen after upgrade: %v", err)
			}
			defer s2.Close()
			if again := appliedMigrations(t, s2.DB()); !maps.Equal(again, applied) {
				t.Errorf("schema_migrations changed on reopen: %v, was %v", again, applied)
			}
		})
	}
}

// checkV020Family reads the v0.2.0 fixture's household back through the public store
// API. Row counts prove nothing was deleted; this proves the rows still mean what they
// meant — which is the half a migration that rebuilds a table with the columns in a new
// order gets wrong.
//
// The ids are literals because the fixture is a fixed file: they are exactly as stable
// as its text, and naming them is clearer than looking every row up by title.
func checkV020Family(t *testing.T, s *Store) {
	const (
		mumID, dadID, leoID, granID    = 1, 2, 3, 4
		familyCal, parentsCal, kidsCal = 1, 2, 3
		dentistID, holidayID           = 1, 3
		swimmingID, movedSwimID        = 4, 5
		swimRecurrence                 = 1
	)

	// --- people ---------------------------------------------------------------
	users, err := s.ListUsers(ctx())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	wantNames := []string{"Dad", "Gran", "Leo", "Mum"}
	if len(users) != len(wantNames) {
		t.Fatalf("ListUsers returned %d users, want %d", len(users), len(wantNames))
	}
	for i, want := range wantNames {
		if users[i].DisplayName != want {
			t.Errorf("user %d is %q, want %q", i, users[i].DisplayName, want)
		}
	}
	gran, err := s.UserByEmail(ctx(), "gran@example.org")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	if gran.ID != granID || gran.Lang != domain.LangFR || gran.WeekStart != time.Sunday {
		t.Errorf("Gran = %+v; want id %d, French, week starting Sunday", gran, granID)
	}
	leo, err := s.UserByID(ctx(), leoID)
	if err != nil {
		t.Fatalf("UserByID(Leo): %v", err)
	}
	if leo.TimeFormat != "12h" || !leo.HasAvatar {
		t.Errorf("Leo = %+v; want the 12h clock and an avatar", leo)
	}

	// A password hash is the row most obviously ruined by a careless rewrite, and the
	// damage is invisible until the family cannot sign in. Verifying it end to end is
	// the only check that proves the bytes came back identical.
	hash, err := s.UserPasswordHash(ctx(), leoID)
	if err != nil {
		t.Fatalf("UserPasswordHash: %v", err)
	}
	if ok, err := auth.VerifyPassword(hash, "password"); err != nil || !ok {
		t.Errorf("Leo can no longer sign in after the upgrade: VerifyPassword = %v, %v", ok, err)
	}

	// BLOBs: the columns a text-only dump-and-reload would mangle.
	avatar, err := s.Avatar(ctx(), leoID)
	if err != nil {
		t.Fatalf("Avatar: %v", err)
	}
	image, err := s.CalendarImage(ctx(), familyCal)
	if err != nil {
		t.Fatalf("CalendarImage: %v", err)
	}
	if len(avatar) != 67 || !bytes.HasPrefix(avatar, []byte("\x89PNG\r\n\x1a\n")) {
		t.Errorf("avatar is %d bytes starting %x; want the 67-byte PNG the fixture stored",
			len(avatar), avatar[:min(8, len(avatar))])
	}
	if !bytes.Equal(avatar, image) {
		t.Error("the calendar cover image no longer matches the avatar bytes the fixture stored")
	}

	// --- calendars, members, labels -------------------------------------------
	cals, err := s.ListCalendarsForUser(ctx(), mumID)
	if err != nil {
		t.Fatalf("ListCalendarsForUser: %v", err)
	}
	if len(cals) != 3 {
		t.Errorf("Mum is in %d calendars, want 3", len(cals))
	}
	members, err := s.ListMembers(ctx(), familyCal)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 4 {
		t.Errorf("Family has %d members, want 4", len(members))
	}
	granMember, err := s.Membership(ctx(), familyCal, granID)
	if err != nil {
		t.Fatalf("Membership: %v", err)
	}
	if !granMember.Muted || !granMember.ParticipatingOnly {
		t.Errorf("Gran's Family membership = %+v; the mute she set has been reset", granMember)
	}
	labels, err := s.ListLabels(ctx(), familyCal)
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	if len(labels) != domain.LabelsPerCalendar {
		t.Fatalf("Family has %d labels, want %d", len(labels), domain.LabelsPerCalendar)
	}
	if labels[2].Name != "Holidays" || labels[2].Color != "#e67e22" {
		t.Errorf("the renamed label came back as %+v; want Holidays/#e67e22", labels[2])
	}

	// --- events ----------------------------------------------------------------
	dentist, err := s.EventByID(ctx(), dentistID)
	if err != nil {
		t.Fatalf("EventByID(dentist): %v", err)
	}
	// 16:30 Paris on 2026-07-27 is 14:30Z. An hour lost here is the whole point of
	// storing instants as UTC text.
	if want := time.Date(2026, 7, 27, 14, 30, 0, 0, time.UTC); !dentist.StartsAt.Equal(want) {
		t.Errorf("Leo's dentist starts at %v, want %v", dentist.StartsAt, want)
	}
	if dentist.Title != "Leo's dentist" || dentist.Location != "Bridge Street Dental" {
		t.Errorf("dentist = %+v", dentist)
	}
	parts, err := s.ListParticipants(ctx(), dentistID)
	if err != nil {
		t.Fatalf("ListParticipants: %v", err)
	}
	if !slices.Equal(parts, []int64{mumID, leoID}) {
		t.Errorf("dentist participants = %v, want [%d %d]", parts, mumID, leoID)
	}

	holiday, err := s.EventByID(ctx(), holidayID)
	if err != nil {
		t.Fatalf("EventByID(holiday): %v", err)
	}
	// An all-day event carries dates and no instants; end_date is inclusive. A
	// migration that "helpfully" normalised these to midnight instants would show up
	// here as a seven-day holiday becoming six, or as a timezone appearing.
	if !holiday.AllDay || !holiday.StartsAt.IsZero() || !holiday.EndsAt.IsZero() {
		t.Errorf("Seaside holiday = %+v; an all-day event must carry no instants", holiday)
	}
	if holiday.StartDate != domain.NewDate(2026, time.August, 6) ||
		holiday.EndDate != domain.NewDate(2026, time.August, 12) {
		t.Errorf("Seaside holiday runs %v..%v, want 2026-08-06..2026-08-12",
			holiday.StartDate, holiday.EndDate)
	}

	// search_norm is maintained by the store on write, so nothing re-derives it after
	// a migration: if the column were dropped and recreated, search would go silently
	// empty rather than fail.
	found, err := s.SearchEvents(ctx(), []int64{familyCal, parentsCal, kidsCal}, "dentist", nil, nil)
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(found) != 1 || found[0].ID != dentistID {
		t.Errorf("searching for \"dentist\" found %d events, want just the dentist", len(found))
	}

	// --- the series, its override and its cancellation -------------------------
	from, to := domain.NewDate(2026, time.August, 1), domain.NewDate(2026, time.August, 31)
	res, err := s.EventsInRange(ctx(), []int64{kidsCal}, from, to)
	if err != nil {
		t.Fatalf("EventsInRange: %v", err)
	}
	var swim *SeriesRow
	for i := range res.Series {
		if res.Series[i].Event.ID == swimmingID {
			swim = &res.Series[i]
		}
	}
	if swim == nil {
		t.Fatalf("the Swimming series is gone: EventsInRange returned %d series", len(res.Series))
	}
	for _, e := range res.Singles {
		if e.ID == movedSwimID {
			t.Error("the edited occurrence came back as a standalone event; it must arrive inside its series")
		}
	}
	if swim.Recurrence.Freq != domain.FreqWeekly || swim.Recurrence.Interval != 1 ||
		!slices.Equal(swim.Recurrence.ByWeekday, []time.Weekday{time.Tuesday}) {
		t.Errorf("Swimming recurrence = %+v; want weekly on Tuesdays", swim.Recurrence)
	}

	dates := recur.Expand(swim.Recurrence, from, to)
	wantDates := []domain.Date{
		domain.NewDate(2026, time.August, 4),
		domain.NewDate(2026, time.August, 11),
		domain.NewDate(2026, time.August, 18),
		domain.NewDate(2026, time.August, 25),
	}
	if !slices.Equal(dates, wantDates) {
		t.Fatalf("Swimming expands to %v, want %v", dates, wantDates)
	}
	if len(swim.Overrides) != 2 {
		t.Errorf("Swimming has %d exceptions, want 2 (one moved, one cancelled)", len(swim.Overrides))
	}

	// Walk the expansion the way a caller does, applying the exceptions: 4 August was
	// moved to the evening, 18 August was cancelled, the rest are the series as
	// written. Losing an event_overrides row does not lose data visibly — it silently
	// resurrects a cancelled occurrence and un-edits a moved one — so this is checked
	// through its effect rather than its row count.
	type occ struct {
		date  domain.Date
		title string
	}
	var visible []occ
	for _, d := range dates {
		ref, excepted := swim.Overrides[d]
		switch {
		case excepted && ref == nil:
			continue
		case excepted:
			edited, ok := swim.OverrideEvents[*ref]
			if !ok {
				t.Fatalf("override for %v points at event %d, which is not in the result", d, *ref)
			}
			if want := time.Date(2026, 8, 4, 17, 0, 0, 0, time.UTC); !edited.StartsAt.Equal(want) {
				t.Errorf("the moved occurrence starts at %v, want %v (19:00 Paris)", edited.StartsAt, want)
			}
			visible = append(visible, occ{d, edited.Title})
		default:
			visible = append(visible, occ{d, swim.Event.Title})
		}
	}
	wantVisible := []occ{
		{domain.NewDate(2026, time.August, 4), "Swimming (later than usual)"},
		{domain.NewDate(2026, time.August, 11), "Swimming"},
		{domain.NewDate(2026, time.August, 25), "Swimming"},
	}
	if !slices.Equal(visible, wantVisible) {
		t.Errorf("August's swimming = %v, want %v", visible, wantVisible)
	}

	// --- reminders --------------------------------------------------------------
	all, err := s.ListAllReminders(ctx())
	if err != nil {
		t.Fatalf("ListAllReminders: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListAllReminders returned %d, want 3", len(all))
	}
	timed, err := s.ListReminders(ctx(), ptrTo(int64(dentistID)), nil, mumID)
	if err != nil {
		t.Fatalf("ListReminders(dentist): %v", err)
	}
	if len(timed) != 1 || timed[0].OffsetMinutes == nil || *timed[0].OffsetMinutes != 1440 {
		t.Errorf("Mum's dentist reminder = %+v; want one at 1440 minutes before", timed)
	}
	// The all-day shape: days_before plus a wall-clock time, which is the pair that
	// cannot be expressed as an offset from midnight.
	allDay, err := s.ListReminders(ctx(), ptrTo(int64(holidayID)), nil, dadID)
	if err != nil {
		t.Fatalf("ListReminders(holiday): %v", err)
	}
	if len(allDay) != 1 || allDay[0].DaysBefore == nil || *allDay[0].DaysBefore != 2 ||
		allDay[0].AtTimeLocal != "09:00" {
		t.Errorf("Dad's holiday reminder = %+v; want two days before at 09:00", allDay)
	}
	series, err := s.ListReminders(ctx(), nil, ptrTo(int64(swimRecurrence)), dadID)
	if err != nil {
		t.Fatalf("ListReminders(series): %v", err)
	}
	if len(series) != 1 || series[0].OffsetMinutes == nil || *series[0].OffsetMinutes != 30 {
		t.Errorf("Dad's swimming reminder = %+v; want one at 30 minutes before", series)
	}

	// --- the outbox, the log and the small tables ------------------------------
	unsent, err := s.ListUnsentBefore(ctx(), upgradedAt.AddDate(1, 0, 0))
	if err != nil {
		t.Fatalf("ListUnsentBefore: %v", err)
	}
	var unsentIDs []int64
	for _, q := range unsent {
		unsentIDs = append(unsentIDs, q.ID)
	}
	// Rows 1 and 3 went out and were skipped respectively; only 4 and 2 are still
	// owed, and they must survive an upgrade or the family misses them silently.
	if !slices.Equal(unsentIDs, []int64{4, 2}) {
		t.Errorf("still-owed notifications = %v, want [4 2]", unsentIDs)
	}
	// A column added after these rows were written has to read back as "this has
	// not happened", not as an error and not as a zero that means something else.
	// email_sent_at (0003) is NULL for every row v0.2.0 wrote, and that is the
	// truth: none of them had an email leg recorded separately.
	for _, q := range unsent {
		if !q.EmailSentAt.IsZero() {
			t.Errorf("notification %d came out of the upgrade claiming its email went at %s",
				q.ID, q.EmailSentAt)
		}
	}

	activity, err := s.ListActivity(ctx(), []int64{familyCal, parentsCal, kidsCal}, 100, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(activity) != 11 {
		t.Errorf("activity log has %d entries, want 11", len(activity))
	}
	if len(activity) > 0 && (activity[0].Action != domain.ActionMemberJoined || activity[0].Title != "Gran") {
		t.Errorf("newest activity = %+v; want Gran joining", activity[0])
	}

	sess, sessUser, err := s.SessionByToken(ctx(),
		"0f9a2c1e5b7d4a3f8e6c0b5d2a91746c3e8f0a1b2c3d4e5f60718293a4b5c6d7")
	if err != nil {
		t.Fatalf("SessionByToken: %v", err)
	}
	if sessUser.ID != mumID || sess.UserID != mumID {
		t.Errorf("Mum's session came back as user %d/%d", sess.UserID, sessUser.ID)
	}

	inv, invCal, err := s.InviteByToken(ctx(),
		"3c6d5f2e1b0a9d8c7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d", upgradedAt)
	if err != nil {
		t.Fatalf("InviteByToken: %v", err)
	}
	if invCal.ID != familyCal || inv.RevokedAt != nil {
		t.Errorf("the live invite came back as %+v for calendar %d", inv, invCal.ID)
	}

	subs, err := s.ListPushSubscriptions(ctx(), mumID)
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(subs) != 2 {
		t.Errorf("Mum has %d push subscriptions, want 2", len(subs))
	}

	prefs, err := s.Prefs(ctx(), leoID)
	if err != nil {
		t.Fatalf("Prefs: %v", err)
	}
	if prefs.DigestEnabled || !prefs.DailySummaryMode || prefs.SummaryTime != "21:15" {
		t.Errorf("Leo's notification prefs = %+v; want the evening summary he chose", prefs)
	}

	holidays, err := s.HolidayOverrides(ctx())
	if err != nil {
		t.Fatalf("HolidayOverrides: %v", err)
	}
	added := holidays[domain.NewDate(2026, time.May, 8)]
	suppressed, hasSuppressed := holidays[domain.NewDate(2026, time.November, 11)]
	if added == nil || *added != "Victoire 1945 (jour de pont)" {
		t.Errorf("the added holiday came back as %v", added)
	}
	// NULL means "suppress this computed holiday". A migration that turned NULL into
	// the empty string would put 11 November back on the calendar.
	if !hasSuppressed || suppressed != nil {
		t.Errorf("the suppressed holiday came back as %v (present: %v)", suppressed, hasSuppressed)
	}

	if v, err := s.GetMeta(ctx(), "planner_horizon"); err != nil || v != "2026-08-26" {
		t.Errorf("meta planner_horizon = %q, %v; want 2026-08-26", v, err)
	}
}

// openRawDB opens the database file with no pragmas at all, which is how a fixture is
// loaded and how the file is inspected before Open has had a chance to change it.
func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, "file:"+escapeURIPath(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return db
}

// userTables lists the application's tables, SQLite's own excluded.
func userTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the database has no tables")
	}
	return out
}

func rowCounts(t *testing.T, db *sql.DB, tables []string) map[string]int {
	t.Helper()
	out := make(map[string]int, len(tables))
	for _, tbl := range tables {
		var n int
		// The name comes from sqlite_master, not from a caller, so quoting it is
		// enough; there is no parameter form for an identifier.
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + tbl + `"`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

// appliedMigrations returns version -> applied_at, so a test can tell "already there"
// from "applied again just now".
func appliedMigrations(t *testing.T, db *sql.DB) map[int]string {
	t.Helper()
	rows, err := db.Query(`SELECT version, applied_at FROM schema_migrations`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var v int
		var at string
		if err := rows.Scan(&v, &at); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		out[v] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return out
}

func maxVersion(applied map[int]string) int {
	highest := 0
	for v := range applied {
		if v > highest {
			highest = v
		}
	}
	return highest
}

// pragmaRows runs a pragma that reports problems as rows and returns the first column
// of each. integrity_check answers with a single "ok"; foreign_key_check answers with
// nothing at all when there is nothing wrong.
func pragmaRows(t *testing.T, db *sql.DB, pragma string) []string {
	t.Helper()
	rows, err := db.Query(pragma)
	if err != nil {
		t.Fatalf("%s: %v", pragma, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s: %v", pragma, err)
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		dst := make([]any, len(cols))
		for i := range cells {
			dst[i] = &cells[i]
		}
		if err := rows.Scan(dst...); err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
		out = append(out, fmt.Sprint(cells...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", pragma, err)
	}
	return out
}

func ptrTo[T any](v T) *T { return &v }

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

// countRows is for the teardown tests, which are about rows nothing in the API can see
// any more: an orphaned recurrence has no calendar to be listed under, and a queued
// notification is only readable through the planner's own queries.
func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestDeletingACalendarLeavesNothingBehind covers the two tables no cascade reaches
// from the calendar row: recurrences, which have no calendar_id and are reached only
// through events.recurrence_id (ON DELETE SET NULL, so the cascade lets go of them
// rather than following them), and the outbox, whose payload is denormalised and never
// re-checked against the calendar it came from.
func TestDeletingACalendarLeavesNothingBehind(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	doomed := mustCalendar(t, s, u.ID, "Maison")
	kept := mustCalendar(t, s, u.ID, "Travail")

	// The same shape in both calendars, so the delete has to be scoped and not merely
	// thorough: a series with a reminder on the pattern, a one-off with a reminder of
	// its own, and a notification already materialised for each.
	seed := func(cal domain.Calendar, title string) (series, single domain.Event) {
		t.Helper()
		label := firstLabel(t, s, cal.ID)
		starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)
		series, err := s.CreateEvent(ctx(), domain.Event{
			CalendarID: cal.ID, Title: title, StartsAt: starts, EndsAt: starts.Add(time.Hour),
			LabelID: label.ID, CreatedBy: u.ID,
		}, &domain.Recurrence{
			Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
			DTStart: domain.MustParseDate("2026-08-04"),
		})
		if err != nil {
			t.Fatalf("create series in %s: %v", cal.Name, err)
		}
		single, err = s.CreateEvent(ctx(), domain.Event{
			CalendarID: cal.ID, Title: title + " (une fois)", StartsAt: starts, EndsAt: starts.Add(time.Hour),
			LabelID: label.ID, CreatedBy: u.ID,
		}, nil)
		if err != nil {
			t.Fatalf("create event in %s: %v", cal.Name, err)
		}
		thirty := 30
		if err := s.ReplaceReminders(ctx(), nil, series.RecurrenceID, u.ID,
			[]domain.Reminder{{OffsetMinutes: &thirty}}); err != nil {
			t.Fatalf("reminder on the series in %s: %v", cal.Name, err)
		}
		if err := s.ReplaceReminders(ctx(), &single.ID, nil, u.ID,
			[]domain.Reminder{{OffsetMinutes: &thirty}}); err != nil {
			t.Fatalf("reminder on the event in %s: %v", cal.Name, err)
		}
		for _, e := range []domain.Event{series, single} {
			// The format internal/events.ReminderSourceRef writes.
			if err := s.EnqueueNotification(ctx(), domain.QueuedNotification{
				UserID: u.ID, Kind: domain.KindReminder,
				SourceRef: fmt.Sprintf("reminder:%d:2026-08-04:1", e.ID),
				Payload:   `{"title":"` + title + `"}`, DueAt: baseTime.Add(time.Hour),
			}); err != nil {
				t.Fatalf("enqueue for %s: %v", cal.Name, err)
			}
		}
		return series, single
	}
	doomedSeries, _ := seed(doomed, "Piscine")
	seed(kept, "Réunion")

	// A row that was already delivered is history, not pending work, and must survive.
	sent := domain.QueuedNotification{
		UserID: u.ID, Kind: domain.KindReminder,
		SourceRef: fmt.Sprintf("reminder:%d:2026-07-28:1", doomedSeries.ID),
		Payload:   `{"title":"Piscine"}`, DueAt: baseTime.Add(-time.Hour),
	}
	if err := s.EnqueueNotification(ctx(), sent); err != nil {
		t.Fatalf("enqueue the delivered row: %v", err)
	}
	if err := s.MarkSent(ctx(), queuedIDBySourceRef(t, s, sent.SourceRef), baseTime); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	if err := s.DeleteCalendar(ctx(), doomed.ID); err != nil {
		t.Fatalf("DeleteCalendar: %v", err)
	}

	if _, err := s.RecurrenceByID(ctx(), *doomedSeries.RecurrenceID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the recurrence outlived its calendar: %v", err)
	}
	if n := countRows(t, s, "recurrences"); n != 1 {
		t.Errorf("%d recurrences left; want only the surviving calendar's", n)
	}
	// The reminders hang off the recurrence and the event, so both cascades have to
	// have fired: two rows are the kept calendar's.
	if n := countRows(t, s, "reminders"); n != 2 {
		t.Errorf("%d reminders left; want only the surviving calendar's two", n)
	}
	if all, _ := s.ListAllReminders(ctx()); len(all) != 2 {
		t.Errorf("the planner still walks %d reminders", len(all))
	}

	pending, err := s.ListUnsentBefore(ctx(), baseTime.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("ListUnsentBefore: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("%d notifications still queued; want only the surviving calendar's two", len(pending))
	}
	for _, p := range pending {
		if strings.Contains(p.Payload, "Piscine") {
			t.Errorf("a notification for the deleted calendar is still queued: %+v", p)
		}
	}
	if n := countRows(t, s, "notification_queue"); n != 3 {
		t.Errorf("%d queue rows left; want two pending plus the delivered one", n)
	}
}

// queuedIDBySourceRef finds a queued row the way no store method does, because
// EnqueueNotification deliberately reports nothing about what it inserted.
func queuedIDBySourceRef(t *testing.T, s *Store, ref string) int64 {
	t.Helper()
	var id int64
	if err := s.DB().QueryRow(`SELECT id FROM notification_queue WHERE source_ref = ?`, ref).Scan(&id); err != nil {
		t.Fatalf("find queued row %q: %v", ref, err)
	}
	return id
}

// TestRemovingAMemberTakesTheirRowsWithThem: event_participants and reminders are keyed
// on users rather than on membership, so deleting the membership row alone leaves an
// ex-member attached to other people's events and their reminders waiting to fire again
// the moment somebody re-invites them.
func TestRemovingAMemberTakesTheirRowsWithThem(t *testing.T) {
	s, _, _ := newStore(t)
	creator := mustUser(t, s, "claire@example.test", "Claire")
	leaver := mustUser(t, s, "marc@example.test", "Marc")
	shared := mustCalendar(t, s, creator.ID, "Maison")
	elsewhere := mustCalendar(t, s, leaver.ID, "Travail")
	if err := s.AddMember(ctx(), shared.ID, leaver.ID); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	starts := time.Date(2026, 8, 4, 14, 30, 0, 0, time.UTC)
	series, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: shared.ID, Title: "Piscine", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: firstLabel(t, s, shared.ID).ID, CreatedBy: creator.ID,
	}, &domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
		DTStart: domain.MustParseDate("2026-08-04"),
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	own, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: elsewhere.ID, Title: "Réunion", StartsAt: starts, EndsAt: starts.Add(time.Hour),
		LabelID: firstLabel(t, s, elsewhere.ID).ID, CreatedBy: leaver.ID,
	}, nil)
	if err != nil {
		t.Fatalf("CreateEvent elsewhere: %v", err)
	}
	if err := s.SetParticipants(ctx(), series.ID, []int64{creator.ID, leaver.ID}); err != nil {
		t.Fatalf("SetParticipants: %v", err)
	}
	if err := s.SetParticipants(ctx(), own.ID, []int64{leaver.ID}); err != nil {
		t.Fatalf("SetParticipants elsewhere: %v", err)
	}

	thirty := 30
	rs := []domain.Reminder{{OffsetMinutes: &thirty}}
	// One of each shape the leaver can own: on the event, on the pattern behind it,
	// and one in a calendar they are staying in.
	if err := s.ReplaceReminders(ctx(), &series.ID, nil, leaver.ID, rs); err != nil {
		t.Fatalf("reminder on the event: %v", err)
	}
	if err := s.ReplaceReminders(ctx(), nil, series.RecurrenceID, leaver.ID, rs); err != nil {
		t.Fatalf("reminder on the series: %v", err)
	}
	if err := s.ReplaceReminders(ctx(), &own.ID, nil, leaver.ID, rs); err != nil {
		t.Fatalf("reminder elsewhere: %v", err)
	}
	if err := s.ReplaceReminders(ctx(), &series.ID, nil, creator.ID, rs); err != nil {
		t.Fatalf("the creator's reminder: %v", err)
	}

	if err := s.RemoveMember(ctx(), shared.ID, leaver.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	parts, err := s.ListParticipants(ctx(), series.ID)
	if err != nil {
		t.Fatalf("ListParticipants: %v", err)
	}
	if len(parts) != 1 || parts[0] != creator.ID {
		t.Errorf("participants = %v; the ex-member is still on the event", parts)
	}
	for _, scope := range []struct {
		name         string
		eventID      *int64
		recurrenceID *int64
	}{
		{"event", &series.ID, nil},
		{"series", nil, series.RecurrenceID},
	} {
		if got, _ := s.ListReminders(ctx(), scope.eventID, scope.recurrenceID, leaver.ID); len(got) != 0 {
			t.Errorf("the ex-member kept %d reminders on the %s", len(got), scope.name)
		}
	}
	if got, _ := s.ListReminders(ctx(), &series.ID, nil, creator.ID); len(got) != 1 {
		t.Errorf("the creator's reminder = %+v; removing somebody else must not touch it", got)
	}
	if got, _ := s.ListParticipants(ctx(), own.ID); len(got) != 1 || got[0] != leaver.ID {
		t.Errorf("participants elsewhere = %v; only the shared calendar was left", got)
	}
	if got, _ := s.ListReminders(ctx(), &own.ID, nil, leaver.ID); len(got) != 1 {
		t.Errorf("the ex-member's reminders elsewhere = %+v; want the one they still own", got)
	}

	// The one that matters: being invited back must not resurrect anything.
	if err := s.AddMember(ctx(), shared.ID, leaver.ID); err != nil {
		t.Fatalf("AddMember on the way back in: %v", err)
	}
	if got, _ := s.ListReminders(ctx(), &series.ID, nil, leaver.ID); len(got) != 0 {
		t.Errorf("re-inviting resurrected %d reminders on the event", len(got))
	}
	if got, _ := s.ListReminders(ctx(), nil, series.RecurrenceID, leaver.ID); len(got) != 0 {
		t.Errorf("re-inviting resurrected %d reminders on the series", len(got))
	}
	if got, _ := s.ListParticipants(ctx(), series.ID); len(got) != 1 {
		t.Errorf("re-inviting put the ex-member back on the event: %v", got)
	}

	// Removing somebody who is not there is still an error, and still changes nothing.
	stranger := mustUser(t, s, "stranger@example.test", "Stranger")
	if err := s.RemoveMember(ctx(), shared.ID, stranger.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removing a non-member = %v; want domain.ErrNotFound", err)
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

	// A series that ended before the window, and one that starts after it. The ended
	// one stops well before it rather than just before it: a series is read back for
	// seriesTailDays afterwards in case its last occurrence reaches into the window.
	endedUntil := domain.MustParseDate("2026-06-15")
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

// TestEventsInRangeKeepsSeriesWhoseOccurrenceMovedOutside is the store half of the
// disappearing occurrence. An override can put an occurrence outside the series' own
// dtstart..until envelope, and the copy carrying it is deliberately kept out of the
// singles query — so if this query drops the series, nothing in the application ever
// sees that occurrence again.
func TestEventsInRangeKeepsSeriesWhoseOccurrenceMovedOutside(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)

	// A series and one moved occurrence, described by the dates the override uses.
	mk := func(title string, rec domain.Recurrence, occDate, movedTo string) int64 {
		t.Helper()
		start := rec.DTStart.At(17, 30, s.loc).UTC()
		series, err := s.CreateEvent(ctx(), domain.Event{
			CalendarID: cal.ID, Title: title, StartsAt: start, EndsAt: start.Add(time.Hour),
			LabelID: label.ID, CreatedBy: u.ID,
		}, &rec)
		if err != nil {
			t.Fatalf("create series %q: %v", title, err)
		}
		moved := domain.MustParseDate(movedTo).At(17, 30, s.loc).UTC()
		copyEv, err := s.CreateEvent(ctx(), domain.Event{
			CalendarID: cal.ID, Title: title + " (déplacé)", StartsAt: moved, EndsAt: moved.Add(time.Hour),
			LabelID: label.ID, CreatedBy: u.ID,
		}, nil)
		if err != nil {
			t.Fatalf("create override copy for %q: %v", title, err)
		}
		if err := s.SetOverride(ctx(), *series.RecurrenceID, domain.MustParseDate(occDate), &copyEv.ID); err != nil {
			t.Fatalf("SetOverride for %q: %v", title, err)
		}
		return *series.RecurrenceID
	}

	endOfTerm := domain.MustParseDate("2026-06-30")
	// Ended in June, last occurrence moved forward into July.
	after := mk("Piscine", domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
		DTStart: domain.MustParseDate("2026-06-02"), Until: &endOfTerm,
	}, "2026-06-30", "2026-07-07")
	// Starts in August, first occurrence moved back into July.
	before := mk("Danse", domain.Recurrence{
		Freq: domain.FreqWeekly, Interval: 1, ByWeekday: []time.Weekday{time.Tuesday},
		DTStart: domain.MustParseDate("2026-08-04"),
	}, "2026-08-04", "2026-07-28")

	res, err := s.EventsInRange(ctx(), []int64{cal.ID},
		domain.MustParseDate("2026-07-01"), domain.MustParseDate("2026-07-31"))
	if err != nil {
		t.Fatalf("EventsInRange: %v", err)
	}
	got := map[int64]SeriesRow{}
	for _, row := range res.Series {
		got[row.Recurrence.ID] = row
	}
	if _, ok := got[after]; !ok {
		t.Error("the series whose last occurrence moved past its until is missing")
	}
	if _, ok := got[before]; !ok {
		t.Error("the series whose first occurrence moved before its dtstart is missing")
	}
	for id, row := range got {
		if len(row.OverrideEvents) != 1 {
			t.Errorf("series %d came back without the copy that carries the moved occurrence: %v", id, row.OverrideEvents)
		}
	}
	// The copies stay out of Singles: they belong to their series, and drawing them
	// twice is what the NOT EXISTS there prevents.
	if len(res.Singles) != 0 {
		t.Errorf("Singles = %+v; the override copies must arrive inside their series", res.Singles)
	}
}

// TestEventsInRangeIgnoresCancellationsOutsideTheWindow is the other side of that
// widening. A cancellation removes an occurrence, so it can never make one appear in a
// window the series does not reach — and a series that ended long ago must not be read
// back on every month view for the rest of the family's life because someone once
// skipped a lesson.
func TestEventsInRangeIgnoresCancellationsOutsideTheWindow(t *testing.T) {
	s, _, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)

	start := domain.MustParseDate("2026-01-06").At(17, 30, s.loc).UTC()
	until := domain.MustParseDate("2026-01-27")
	series, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine", StartsAt: start, EndsAt: start.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID,
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-01-06"), Until: &until})
	if err != nil {
		t.Fatalf("create series: %v", err)
	}
	if err := s.SetOverride(ctx(), *series.RecurrenceID, domain.MustParseDate("2026-01-20"), nil); err != nil {
		t.Fatalf("SetOverride (cancel): %v", err)
	}

	res, err := s.EventsInRange(ctx(), []int64{cal.ID},
		domain.MustParseDate("2026-07-01"), domain.MustParseDate("2026-07-31"))
	if err != nil {
		t.Fatalf("EventsInRange: %v", err)
	}
	if len(res.Series) != 0 {
		t.Errorf("Series = %+v; a series that ended in January with a cancellation in it has nothing to show in July", res.Series)
	}
}

// ---------------------------------------------------------------------------
// A range read is one snapshot
// ---------------------------------------------------------------------------

// TestReadTransactionSnapshotsWithoutTakingTheWriteLock pins the two properties the
// range read is built on, and they pull in opposite directions.
//
// The first is the point of the thing: statements inside a read transaction all see the
// database as it was when the first of them ran, so a commit that lands half-way through
// cannot be half-observed.
//
// The second is what makes the first affordable. modernc.org/sqlite turns
// sql.TxOptions{ReadOnly: true} into a plain deferred BEGIN, skipping the
// `_txlock=immediate` the DSN asks for; a read transaction therefore takes no write lock,
// and a writer on another connection carries on. Get that wrong and every month view
// serialises every writer behind it for as long as the read takes — much worse than the
// bug the transaction is here to close. So the write below happens while the read
// transaction is open, on a second connection, and is asserted to succeed.
func TestReadTransactionSnapshotsWithoutTakingTheWriteLock(t *testing.T) {
	s, _, path := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)

	add := func(st *Store, title string, day int) {
		t.Helper()
		start := time.Date(2026, 8, day, 14, 30, 0, 0, time.UTC)
		if _, err := st.CreateEvent(ctx(), domain.Event{
			CalendarID: cal.ID, Title: title, StartsAt: start, EndsAt: start.Add(time.Hour),
			LabelID: label.ID, CreatedBy: u.ID,
		}, nil); err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
	}
	add(s, "Dentiste", 4)

	// A second store on the same file is a second connection with its own pool, which
	// is what another request holding the write lock actually looks like.
	writer, err := Open(path, testLocation(t), clock.NewFake(baseTime))
	if err != nil {
		t.Fatalf("open a second store on the same file: %v", err)
	}
	defer writer.Close()

	count := func(q querier) int {
		t.Helper()
		var n int
		if err := q.QueryRowContext(ctx(), `SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n
	}

	var before, inside int
	if err := s.readTx(ctx(), func(q querier) error {
		// The first statement is what fixes the snapshot; nothing committed after it
		// may appear to the ones that follow.
		before = count(q)
		add(writer, "Piscine", 5)
		inside = count(q)
		return nil
	}); err != nil {
		t.Fatalf("readTx: %v", err)
	}

	if before != 1 {
		t.Fatalf("the read transaction opened on %d events; want 1", before)
	}
	if inside != before {
		t.Errorf("a second statement in the read transaction saw %d events where the first saw %d: "+
			"the read is not a snapshot", inside, before)
	}
	if after := count(s.q); after != 2 {
		t.Errorf("outside the read transaction there are %d events; want 2 — the concurrent write "+
			"was meant to have landed", after)
	}
}

// TestEventsInRangeIsOneReadTransaction watches the driver rather than the results,
// because the bug this closes is not visible in either.
//
// EventsInRange reads five related tables. Run against the pool they are five
// independent statements, and an edit committing between any two of them is observed
// half-applied: an occurrence drawn twice, once as itself and once inside its series, or
// missing from both. The results of any single call look perfectly ordinary either way,
// so the only honest assertion is the structural one — one BEGIN, read-only, with all
// five statements inside it.
func TestEventsInRangeIsOneReadTransaction(t *testing.T) {
	s, clk, path := newStore(t)
	loc := s.loc
	u := mustUser(t, s, "claire@example.test", "Claire")
	marc := mustUser(t, s, "marc@example.test", "Marc")
	cal := mustCalendar(t, s, u.ID, "Maison")
	label := firstLabel(t, s, cal.ID)

	// Enough of a calendar that all five queries have something to do: the last two
	// return early on an empty id list, and a test that never reaches them would pass
	// with them still on the pool.
	seriesStart := time.Date(2026, 8, 4, 16, 0, 0, 0, time.UTC)
	series, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine", StartsAt: seriesStart, EndsAt: seriesStart.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID, Participants: []int64{u.ID, marc.ID},
	}, &domain.Recurrence{Freq: domain.FreqWeekly, Interval: 1,
		ByWeekday: []time.Weekday{time.Tuesday}, DTStart: domain.MustParseDate("2026-08-04")})
	if err != nil {
		t.Fatalf("create series: %v", err)
	}
	movedStart := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)
	moved, err := s.CreateEvent(ctx(), domain.Event{
		CalendarID: cal.ID, Title: "Piscine (20h)", StartsAt: movedStart, EndsAt: movedStart.Add(time.Hour),
		LabelID: label.ID, CreatedBy: u.ID, Participants: []int64{marc.ID},
	}, nil)
	if err != nil {
		t.Fatalf("create the moved occurrence: %v", err)
	}
	if err := s.SetOverride(ctx(), *series.RecurrenceID, domain.MustParseDate("2026-08-11"), &moved.ID); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close the seeding store: %v", err)
	}

	traced, tr := tracedStore(t, path, loc, clk)
	// Warm the pool first: opening a connection replays the DSN pragmas, and that is
	// not part of what the read does.
	if err := traced.Ping(ctx()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	tr.reset()

	res, err := traced.EventsInRange(ctx(), []int64{cal.ID},
		domain.MustParseDate("2026-08-01"), domain.MustParseDate("2026-08-31"))
	if err != nil {
		t.Fatalf("EventsInRange: %v", err)
	}
	if len(res.Series) != 1 || len(res.Series[0].OverrideEvents) != 1 {
		t.Fatalf("the traced read did not return the series and its moved occurrence: %+v", res)
	}

	entries := tr.entries()
	if len(entries) == 0 {
		t.Fatal("the driver saw nothing at all")
	}
	if entries[0].op != "begin" {
		t.Fatalf("the first thing EventsInRange did at the driver was %v, not a BEGIN: "+
			"its statements are running against the pool, so an edit committing between "+
			"two of them is observed half-applied", entries[0])
	}
	if entries[0].arg != "read-only" {
		t.Errorf("the range read began a %q transaction; it must be read-only, or (with "+
			"_txlock=immediate in the DSN) every month view takes SQLite's write lock and "+
			"queues every writer behind it", entries[0].arg)
	}
	if last := entries[len(entries)-1]; last.op != "commit" && last.op != "rollback" {
		t.Errorf("the read transaction was never ended; the last thing the driver saw was %v", last)
	}

	begins := 0
	var stmts []string
	for _, e := range entries {
		switch e.op {
		case "begin":
			begins++
		case "sql":
			stmts = append(stmts, e.arg)
		}
	}
	if begins != 1 {
		t.Errorf("EventsInRange began %d transactions; want exactly 1", begins)
	}
	// In order: the singles, the series templates, their exceptions, the copies those
	// exceptions point at, and everyone's participation.
	want := []string{
		"FROM events e",
		"JOIN recurrences r",
		"FROM event_overrides",
		"FROM events WHERE id IN",
		"FROM event_participants",
	}
	if len(stmts) != len(want) {
		t.Fatalf("EventsInRange issued %d statements, want %d: %q", len(stmts), len(want), stmts)
	}
	for i, fragment := range want {
		if !strings.Contains(stmts[i], fragment) {
			t.Errorf("statement %d does not contain %q, so the five queries are not the ones "+
				"expected inside the transaction:\n%s", i+1, fragment, stmts[i])
		}
	}
}

// traceEntry is one thing the tracing driver below saw: a transaction beginning
// ("read-only" or "write"), a statement, or a transaction ending.
type traceEntry struct {
	op  string
	arg string
}

// tracer collects those in order. It is shared by every connection of one pool, and the
// range read uses one connection, so the order it records is the order that connection
// saw.
type tracer struct {
	mu   sync.Mutex
	seen []traceEntry
}

func (tr *tracer) record(op, arg string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.seen = append(tr.seen, traceEntry{op: op, arg: arg})
}

func (tr *tracer) reset() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.seen = nil
}

func (tr *tracer) entries() []traceEntry {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return slices.Clone(tr.seen)
}

// tracingDriverSeq keeps the registered driver names unique, since database/sql panics
// on a duplicate and `go test -count=2` would otherwise hit one.
var tracingDriverSeq atomic.Int64

// tracedStore opens an already-migrated database through a driver that reports what it
// is asked to do, and returns a Store wired to it exactly as Open wires the real one.
//
// It is deliberately not a variant of Open: the point is to run the production DSN,
// `_txlock=immediate` included, through the production pool settings, so that what the
// trace shows is what a real deployment does.
func tracedStore(t *testing.T, path string, loc *time.Location, clk clock.Clock) (*Store, *tracer) {
	t.Helper()

	// sql.Open does not connect, so this is only a way to reach the registered driver
	// value without importing the package that registers it.
	probe, err := sql.Open(driverName, dsn(path))
	if err != nil {
		t.Fatalf("reach the sqlite driver: %v", err)
	}
	inner := probe.Driver()
	if err := probe.Close(); err != nil {
		t.Fatalf("close the probe: %v", err)
	}

	tr := &tracer{}
	name := fmt.Sprintf("sqlite-tracing-%d", tracingDriverSeq.Add(1))
	sql.Register(name, tracingDriver{inner: inner, tr: tr})

	db, err := sql.Open(name, dsn(path))
	if err != nil {
		t.Fatalf("open %s through the tracing driver: %v", path, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	t.Cleanup(func() { db.Close() })

	return &Store{db: db, q: db, loc: loc, clk: clk}, tr
}

type tracingDriver struct {
	inner driver.Driver
	tr    *tracer
}

func (d tracingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return tracingConn{inner: c, tr: d.tr}, nil
}

// tracingConn implements the context-aware halves of driver.Conn as well as the plain
// ones, because database/sql only falls back to Prepare when they are absent — and a
// prepared statement would hide the SQL from the trace.
type tracingConn struct {
	inner driver.Conn
	tr    *tracer
}

func (c tracingConn) Prepare(query string) (driver.Stmt, error) { return c.inner.Prepare(query) }
func (c tracingConn) Close() error                              { return c.inner.Close() }

func (c tracingConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c tracingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.inner.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	mode := "write"
	if opts.ReadOnly {
		mode = "read-only"
	}
	c.tr.record("begin", mode)
	return tracingTx{inner: tx, tr: c.tr}, nil
}

func (c tracingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.inner.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c tracingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.tr.record("sql", query)
	return c.inner.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c tracingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.tr.record("sql", query)
	return c.inner.(driver.ExecerContext).ExecContext(ctx, query, args)
}

type tracingTx struct {
	inner driver.Tx
	tr    *tracer
}

func (t tracingTx) Commit() error   { t.tr.record("commit", ""); return t.inner.Commit() }
func (t tracingTx) Rollback() error { t.tr.record("rollback", ""); return t.inner.Rollback() }

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
		// The letters the neighbours use. A family address book collects them from
		// one Danish friend and one Icelandic pen pal, and typing the ASCII spelling
		// is the only way most keyboards can reach them.
		{"Søren Kjærgård", "soren kjaergard"},
		{"Straße", "strasse"},
		{"Þorbjörg Eiðsdóttir", "thorbjorg eidsdottir"},
		{"Đorđe", "dorde"},
	}
	for _, tc := range cases {
		if got := foldSearch(tc.in); got != tc.want {
			t.Errorf("foldSearch(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	// foldAccents is documented as correct without a caller having lowercased first,
	// so the uppercase half of the table is checked on its own — it folds to the
	// lowercase spelling, like every other entry. strings.ToLower maps most of these
	// away before foldSearch ever sees them.
	if got := foldAccents("ØÆŒÇÐÞĐẞ"); got != "oaeoecdthdss" {
		t.Errorf("foldAccents(uppercase) = %q; want %q", got, "oaeoecdthdss")
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
	// The email leg is recorded on its own, before the row is finished: the push
	// leg may still be owed, and the retry that owes it must not mail again.
	if err := s.MarkEmailSent(ctx(), first, sendAt); err != nil {
		t.Fatalf("MarkEmailSent: %v", err)
	}
	stillDue, err := s.DueNotifications(ctx(), sendAt, 10)
	if err != nil || len(stillDue) == 0 {
		t.Fatalf("DueNotifications after the email leg = %+v, %v; the row is not finished yet", stillDue, err)
	}
	if !stillDue[0].EmailSentAt.Equal(sendAt) {
		t.Errorf("EmailSentAt = %s, want %s", stillDue[0].EmailSentAt, sendAt)
	}
	// Calling it twice keeps the first answer: it is when the mail went, not when
	// somebody last asked.
	if err := s.MarkEmailSent(ctx(), first, sendAt.Add(time.Hour)); err != nil {
		t.Fatalf("MarkEmailSent again: %v", err)
	}
	if again, _ := s.DueNotifications(ctx(), sendAt, 10); len(again) == 0 || !again[0].EmailSentAt.Equal(sendAt) {
		t.Errorf("EmailSentAt was rewritten by a second call: %+v", again)
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

	entries, err := s.ListActivity(ctx(), []int64{cal.ID}, 10, 0)
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
	older, err := s.ListActivity(ctx(), []int64{cal.ID}, 10, entries[0].ID)
	if err != nil || len(older) != 2 {
		t.Fatalf("ListActivity before the newest = %+v, %v; want 2", older, err)
	}
	if limited, _ := s.ListActivity(ctx(), []int64{cal.ID}, 1, 0); len(limited) != 1 {
		t.Fatal("limit is not honoured")
	}

	// Several calendars at once, which is how the feed is actually read.
	both, err := s.ListActivity(ctx(), []int64{cal.ID, other.ID}, 10, 0)
	if err != nil || len(both) != 4 {
		t.Fatalf("ListActivity across calendars = %+v, %v; want 4", both, err)
	}
	if entries, _ := s.ListActivity(ctx(), nil, 10, 0); len(entries) != 0 {
		t.Fatalf("ListActivity with no calendars = %+v", entries)
	}

	// Forward from a cursor is the planner's read: everything since, oldest first.
	since, err := s.ListActivityAfter(ctx(), []int64{cal.ID, other.ID}, entries[1].ID, 10)
	if err != nil || len(since) != 2 {
		t.Fatalf("ListActivityAfter = %+v, %v; want 2", since, err)
	}
	if since[0].Title != "Vacances" || since[1].Title != "Marc" {
		t.Fatalf("ListActivityAfter is not oldest-first: %+v", since)
	}
	if none, _ := s.ListActivityAfter(ctx(), []int64{cal.ID, other.ID}, both[0].ID, 10); len(none) != 0 {
		t.Fatalf("ListActivityAfter the newest = %+v, want none", none)
	}

	// A day of it, which is what the daily summary counts. The window is half-open:
	// the entry landing exactly on the upper bound belongs to the next one.
	day, err := s.ListActivityBetween(ctx(), []int64{cal.ID}, baseTime, baseTime.Add(2*time.Hour), 10)
	if err != nil || len(day) != 2 {
		t.Fatalf("ListActivityBetween = %+v, %v; want 2", day, err)
	}
	if day[0].Title != "Piscine" {
		t.Fatalf("ListActivityBetween is not newest-first: %+v", day)
	}
}

// TestActivityPagesThroughASharedSecond: instants are stored to the second, and a
// family can easily make two changes inside one — a create and the edit that follows
// it. Paging on the instant walked over everything sharing the second the page ended
// on, and those entries never appeared again.
func TestActivityPagesThroughASharedSecond(t *testing.T) {
	s, clk, _ := newStore(t)
	u := mustUser(t, s, "claire@example.test", "Claire")
	cal := mustCalendar(t, s, u.ID, "Maison")

	clk.Set(baseTime) // never moves: every entry below carries the same instant
	titles := []string{"Dentiste", "Piscine", "Vacances"}
	for _, title := range titles {
		if err := s.LogActivity(ctx(), domain.Activity{
			CalendarID: cal.ID, UserID: u.ID, Action: domain.ActionEventCreated, Title: title,
		}); err != nil {
			t.Fatalf("LogActivity: %v", err)
		}
	}

	// Read the feed a page at a time, the way the scrolling client asks for it.
	var seen []string
	var cursor int64
	for range len(titles) + 1 {
		page, err := s.ListActivity(ctx(), []int64{cal.ID}, 1, cursor)
		if err != nil {
			t.Fatalf("ListActivity: %v", err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].Title)
		cursor = page[len(page)-1].ID
	}
	want := []string{"Vacances", "Piscine", "Dentiste"}
	if !slices.Equal(seen, want) {
		t.Fatalf("paging one at a time saw %v, want %v", seen, want)
	}

	// The planner reads the same second forwards, and must not lose a row either.
	since, err := s.ListActivityAfter(ctx(), []int64{cal.ID}, 0, 10)
	if err != nil || len(since) != len(titles) {
		t.Fatalf("ListActivityAfter = %+v, %v; want %d", since, err, len(titles))
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
