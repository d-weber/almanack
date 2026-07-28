// Package store is the application's data-access layer. It owns the schema, the
// migrations and every SQL statement in the program: no other package imports
// database/sql (see CONVENTIONS.md §2).
//
// # Storage conventions
//
// These are fixed by migrations/0001_init.sql and chosen so that a database file
// opened in 2040 explains itself without this source code:
//
//   - instants are TEXT, RFC 3339 UTC with a trailing Z ("2026-08-04T14:30:00Z").
//     They round-trip at second precision; sub-second parts are discarded on write.
//   - dates are TEXT "YYYY-MM-DD" — domain.Date is the Go side, and it is a
//     driver.Valuer/sql.Scanner already. A zero domain.Date is SQL NULL.
//   - booleans are INTEGER 0/1.
//
// Because every instant is written with the identical layout, lexicographic
// comparison of the TEXT is chronological comparison of the instant, which is what
// lets range queries stay plain SQL.
//
// # Who owns which fields
//
// The store owns row identity and the "when did this happen" columns: id,
// created_at, joined_at, updated_at and activity_log.at are always taken from the
// store's clock.Clock, never from the argument struct. Deadlines the caller chose —
// session and invite expiry, recurrence until, notification due_at — are taken from
// the argument, since only the caller knows the policy.
//
// # Missing rows
//
// Reads and row-identified mutations (Update*, Delete* by id) return
// domain.ErrNotFound when nothing matched. A handful of methods are deliberately
// idempotent instead, because their contract is "make it so" rather than "change
// this row": AddMember, DeleteSession, DeleteUserSessions, DeleteOverride,
// SetOverride, RepointOverrides, UpsertPushSubscription, DeletePushSubscription,
// UpdatePrefs, SetHolidayOverride, SetMeta and EnqueueNotification. Each says so in
// its own doc comment.
//
// # Transactions
//
// Store.InTx runs a function against a Store whose statements all go through one
// transaction, so a caller can make a sequence of these methods atomic without any of
// them knowing. internal/events uses it for the scoped edits, which are several writes
// each and must not be interrupted half-way; nothing in this package needs to be
// rewritten for it, and nothing outside it has to hold a *sql.Tx.
//
// Reads get the same treatment only where an answer is assembled from several tables:
// EventsInRange runs its five queries inside Store.readTx, a read-only transaction, so
// the month it draws is the database at one instant rather than at five. Every other read
// here is a single statement, which SQLite already runs atomically.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"almanack/internal/clock"
	"almanack/internal/domain"

	sqlitedrv "modernc.org/sqlite"
)

// driverName is the name modernc.org/sqlite registers itself under. It is a pure-Go
// SQLite, which is what keeps this project buildable with `go build` alone — no cgo,
// no C toolchain, no cross-compilation ceremony.
const driverName = "sqlite"

// maxOpenConns caps the pool.
//
// modernc.org/sqlite is a translation of SQLite rather than a binding, and it is less
// forgiving under concurrency than the cgo driver. Four connections is the compromise
// this project settled on: WAL lets those four read in parallel, `_txlock=immediate`
// (see dsn) makes every write transaction take the write lock up front so two writers
// queue instead of deadlocking mid-transaction on a lock upgrade, and busy_timeout
// gives the loser five seconds to win. For a household of at most ten people that is
// several orders of magnitude more concurrency than the workload needs.
//
// The rule this used to imply — that code inside a transaction must never call a Store
// method, because asking a four-connection pool for a second connection while holding
// one is a deadlock rather than a slowdown — is now enforced by construction instead of
// by discipline: inside InTx every Store method runs on the transaction's own
// connection, and Store.tx joins the transaction in progress rather than beginning a
// second one. Nothing in this package reaches for the pool while a transaction is open.
const maxOpenConns = 4

// busyTimeoutMS is how long SQLite waits for a lock before returning SQLITE_BUSY.
const busyTimeoutMS = 5000

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is the handle every other package uses to reach the database.
//
// Every query runs against q, which is the pool itself for the Store that Open returns
// and one open transaction for the copy InTx hands out. That is the whole of the
// transaction mechanism: the methods below cannot tell the difference, so any sequence
// of them can be made atomic without being rewritten as SQL.
type Store struct {
	db  *sql.DB
	q   querier
	loc *time.Location
	clk clock.Clock
}

// Open opens (creating it if needed) the SQLite database at path, verifies the
// connection pragmas, and applies any pending migrations.
//
// loc is the family timezone: it is the only correct frame in which to ask "what day
// is this instant on", and EventsInRange uses it to turn a window of dates into a
// window of instants. clk is the sole source of "now" for everything the store
// timestamps.
func Open(path string, loc *time.Location, clk clock.Clock) (*Store, error) {
	return OpenWith(path, loc, clk, Options{})
}

// Options are the things Open can be asked to do beyond opening.
type Options struct {
	// BeforeMigrate is called once, before any pending migration is applied, with the
	// version the file is at and the one it is about to reach. It is not called when
	// there is nothing to apply, which is every ordinary restart.
	//
	// It exists for the pre-migration snapshot: rolling a release back means restoring
	// the database as it was before that release migrated it, and the only moment such
	// a copy can be taken is this one. An error from it stops the migration and the
	// open — deliberately, because a failed snapshot is exactly the moment not to
	// proceed. The hook lives here rather than in the caller because the caller cannot
	// see the version numbers without opening the file, and opening it is what applies
	// the migrations.
	BeforeMigrate func(ctx context.Context, from, to int) error
}

// OpenWith is Open with the options above.
func OpenWith(path string, loc *time.Location, clk clock.Clock, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if loc == nil {
		return nil, errors.New("store: nil location")
	}
	if clk == nil {
		return nil, errors.New("store: nil clock")
	}

	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	// Connections live for the process lifetime: every new one has to replay the DSN
	// pragmas, and recycling them buys nothing against a local file.
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	ctx := context.Background()
	if err := verifyPragmas(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	s := &Store{db: db, q: db, loc: loc, clk: clk}
	if err := s.migrate(ctx, opts.BeforeMigrate); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

// dsn builds the connection string. The pragmas are per-connection and the driver
// replays them on every connection it opens:
//
//   - journal_mode(WAL): readers never block the writer, and a crash cannot leave a
//     half-written page in the main file.
//   - busy_timeout: wait for a lock instead of failing instantly.
//   - foreign_keys(1): SQLite disables FK enforcement by default, and every ON DELETE
//     CASCADE in the schema is load-bearing.
//
// _txlock=immediate is not a pragma: it makes database/sql's BeginTx issue
// BEGIN IMMEDIATE, taking the write lock at the start of the transaction. Without it a
// transaction that reads and then writes can fail its lock upgrade with SQLITE_BUSY,
// which busy_timeout does not retry.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Set("_txlock", "immediate")
	return "file:" + escapeURIPath(path) + "?" + q.Encode()
}

// escapeURIPath percent-encodes the three characters that mean something else inside a
// SQLite URI filename: '?' starts the query, '#' starts the fragment, and '%' starts an
// escape. Everything else — spaces, accents, non-ASCII — is passed through, because
// SQLite treats the rest of the path literally.
//
// The order below is irrelevant to correctness (Replacer makes a single pass and never
// rescans what it has written) but '%' is listed first because it is the one a reader
// will worry about.
func escapeURIPath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
}

// verifyPragmas refuses to hand back a Store whose connections are not configured the
// way dsn asked. A typo in a DSN parameter is silently ignored by SQLite, and the
// failure mode — foreign keys quietly unenforced, cascades quietly not happening — is
// the kind that is discovered years later in the data rather than at startup.
//
// It checks every connection the pool can hold, not just one, and leaves the pool warm.
func verifyPragmas(ctx context.Context, db *sql.DB) error {
	conns := make([]*sql.Conn, 0, maxOpenConns)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := range maxOpenConns {
		c, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("acquire connection %d: %w", i+1, err)
		}
		conns = append(conns, c)

		var journal string
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
			return fmt.Errorf("read journal_mode: %w", err)
		}
		if !strings.EqualFold(journal, "wal") {
			return fmt.Errorf("journal_mode is %q, want wal", journal)
		}
		var fk int
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			return fmt.Errorf("read foreign_keys: %w", err)
		}
		if fk != 1 {
			return fmt.Errorf("foreign_keys is %d, want 1", fk)
		}
		var busy int
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			return fmt.Errorf("read busy_timeout: %w", err)
		}
		if busy != busyTimeoutMS {
			return fmt.Errorf("busy_timeout is %d, want %d", busy, busyTimeoutMS)
		}
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the pool for the few operations that are not queries — the backup
// subcommand's VACUUM INTO, and tests that need to reach past the API on purpose.
// Nothing else should use it: SQL belongs in this package.
func (s *Store) DB() *sql.DB { return s.db }

// Ping checks the database is reachable. /healthz calls it.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Location returns the family timezone the store was opened with. It is the frame in
// which EventsInRange interprets its date window.
func (s *Store) Location() *time.Location { return s.loc }

// now is the single point where the store reads the clock, truncated to the second
// because that is the resolution the TEXT storage format keeps.
func (s *Store) now() time.Time { return s.clk.Now().UTC().Truncate(time.Second) }

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the embedded migrations, ordered by their numeric prefix.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var ms []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		digits := name
		if i := strings.IndexByte(name, '_'); i >= 0 {
			digits = name[:i]
		}
		v, err := strconv.Atoi(digits)
		if err != nil {
			return nil, fmt.Errorf("migration %q: name must start with a version number", name)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, name, v)
		}
		seen[v] = name
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		ms = append(ms, migration{version: v, name: name, sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	if len(ms) == 0 {
		return nil, errors.New("no embedded migrations found")
	}
	return ms, nil
}

// migrate applies every embedded migration the database has not seen, each in its own
// transaction so a failure leaves the schema at the last complete version.
//
// It first refuses to run at all if the database has been migrated by a newer binary.
// Rollback in this project is a symlink flip back to the previous release; that is only
// safe because the old binary stops here instead of running against a schema whose
// shape it does not know.
//
// This is the one part of the store that names the pool rather than s.q. Migrations run
// once, from Open, before any caller holds a Store — and each one owns the transaction
// it is applied in, which is what keeps a failure at the last complete version.
func (s *Store) migrate(ctx context.Context, beforeMigrate func(context.Context, int, int) error) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	newest := ms[len(ms)-1].version
	var maxApplied sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&maxApplied); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if maxApplied.Valid && maxApplied.Int64 > int64(newest) {
		return fmt.Errorf("database schema is at version %d but this binary only knows %d: refusing to start (downgrade the database or upgrade the binary)",
			maxApplied.Int64, newest)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	pending := make([]migration, 0, len(ms))
	for _, m := range ms {
		if !applied[m.version] {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	// Before the first statement of the first one, and only when there is something to
	// apply: an ordinary restart migrates nothing and must not write a snapshot every
	// time. A failure here stops the open, because a migration whose fallback could not
	// be taken is the one that should not run.
	if beforeMigrate != nil {
		from := 0
		if maxApplied.Valid {
			from = int(maxApplied.Int64)
		}
		if err := beforeMigrate(ctx, from, pending[len(pending)-1].version); err != nil {
			return fmt.Errorf("before migrating from schema %d to %d: %w",
				from, pending[len(pending)-1].version, err)
		}
	}

	for _, m := range pending {
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, mustInstant(s.now()),
		); err != nil {
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Plumbing shared by every file in the package
// ---------------------------------------------------------------------------

// querier is the read/write surface *sql.DB and *sql.Tx have in common, so helpers can
// run either standalone or inside a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, which lets one scan function
// serve single-row and multi-row queries.
type rowScanner interface {
	Scan(dest ...any) error
}

// InTx runs fn against a copy of the Store whose every query goes through one
// transaction, committing when fn returns nil and rolling back on any error or panic.
//
// It exists because the interesting writes in this application are sequences. Splitting
// a series ends the old one, creates its replacement, moves the exceptions across and
// copies everyone's reminders; each statement runs on the request's context, so a phone
// that loses its connection half-way through genuinely stops the sequence. Before this,
// that left the old series capped with no replacement — half a family's swimming
// lessons gone, with an error on screen that says nothing about it.
//
// The alternative was a bespoke store method per flow, which would have moved the
// orchestration — recurrence validation, re-anchoring a pattern, deciding which
// exceptions the new pattern can still reach — behind SQL-shaped APIs, and left the
// next flow that spans two writes with the same problem. A transaction-scoped Store
// keeps the decisions where they belong (internal/events) and makes atomicity something
// a caller can ask for in one line.
//
// fn must do its work through the *Store it is given: the outer one still writes
// through the pool, outside the transaction, and would survive the rollback.
//
// Nesting is safe — an InTx inside an InTx joins the transaction already open rather
// than beginning a second one, which on a four-connection pool holding an exclusive
// write lock would deadlock rather than merely queue. Note what joining means: the
// inner fn's error rolls the whole thing back, because there is only one thing.
//
// Because _txlock=immediate takes SQLite's write lock at BEGIN, a transaction here
// serialises every other writer for as long as it is open. Keep them to the statements
// that must land together: validate, compute and decide before calling, and never wait
// on anything slower than the database inside.
func (s *Store) InTx(ctx context.Context, fn func(*Store) error) error {
	if _, joined := s.q.(*sql.Tx); joined {
		return fn(s)
	}
	return s.tx(ctx, func(tx *sql.Tx) error { return fn(s.withTx(tx)) })
}

// withTx returns a shallow copy of s that runs its queries on tx. Everything else —
// the pool handle, the family timezone, the clock — is shared, because a transaction
// changes where the statements go and nothing else.
func (s *Store) withTx(tx *sql.Tx) *Store {
	scoped := *s
	scoped.q = tx
	return &scoped
}

// tx runs fn inside a transaction, committing when it returns nil and rolling back on
// any error or panic. It is the store's own plumbing; callers outside this package use
// InTx.
//
// When s is already transaction-scoped, fn is handed the transaction in progress and
// neither commits nor rolls it back: the outer InTx owns that decision, and a Store
// method that quietly committed half of somebody's transaction would be worse than the
// pool exhaustion this avoids.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	if open, joined := s.q.(*sql.Tx); joined {
		return fn(open)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit has succeeded
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", mapErr(err))
	}
	return nil
}

// readTx runs fn against one read transaction, so that every statement fn issues sees
// the database as it stood when the first of them ran.
//
// It exists for EventsInRange, which reads five related tables to draw a month. On the
// pool those are five independent statements, and an edit committing between two of them
// is observed half-applied — an occurrence drawn twice, once as itself and once inside
// its series, or missing from both. One BEGIN makes the answer self-consistent by
// construction rather than by the writers happening not to land there.
//
// The transaction is read-only, and that is the whole of why this is cheap:
// modernc.org/sqlite issues a plain deferred BEGIN when opts.ReadOnly is set, skipping
// the `_txlock=immediate` the DSN asks for (see dsn). So it takes no write lock, and in
// WAL a reader and the writer proceed together. Beginning a write transaction here
// instead would queue every writer behind every month view, which is a far worse thing
// than the bug this closes; there is a test that opens a read transaction and writes
// through a second connection while it is open.
//
// Like tx, it joins a transaction already in progress rather than beginning a second
// one — asking a four-connection pool for another connection while holding one is a
// deadlock, not a slowdown — and in that case leaves ending it to whoever opened it.
//
// This is not a convention for the package. Every other read here is a single statement,
// which SQLite already runs atomically; wrapping those would buy nothing and cost a
// round trip. Reach for it only where one answer is assembled from several tables.
func (s *Store) readTx(ctx context.Context, fn func(querier) error) error {
	if open, joined := s.q.(*sql.Tx); joined {
		return fn(open)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read transaction: %w", mapErr(err))
	}
	// Rolled back rather than committed: a read transaction has nothing to commit, and
	// the error from ending one carries no information a caller could act on.
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// SQLite extended result codes. They are stable numbers in SQLite's public interface,
// which makes them a sounder thing to switch on than the error text.
const (
	sqliteConstraintCheck      = 275  // SQLITE_CONSTRAINT_CHECK
	sqliteConstraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	sqliteConstraintNotNull    = 1299 // SQLITE_CONSTRAINT_NOTNULL
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintRowID      = 2579 // SQLITE_CONSTRAINT_ROWID
)

// mapErr translates a driver error into one of the domain sentinels, so handlers can
// pick a status code without knowing SQLite exists. The original error is kept in the
// chain: %w twice means both errors.Is(err, domain.ErrConflict) and the SQLite detail
// survive into the log.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	var se *sqlitedrv.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey, sqliteConstraintRowID:
			return fmt.Errorf("%w: %w", domain.ErrConflict, err)
		case sqliteConstraintCheck, sqliteConstraintNotNull, sqliteConstraintForeignKey:
			// A CHECK or FK failure means the caller handed us a row the schema
			// declares impossible — an all-day event carrying instants, a reminder
			// with two scopes, an event on a label that is not there.
			return fmt.Errorf("%w: %w", domain.ErrInvalid, err)
		}
	}
	return err
}

// isNotFound reports whether err means "no such row", whether or not it has already
// been through mapErr.
func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, domain.ErrNotFound)
}

// affected reports domain.ErrNotFound when a statement matched no row.
func affected(res sql.Result, err error) error {
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Column helpers
// ---------------------------------------------------------------------------

// mustInstant renders an instant for a NOT NULL column.
func mustInstant(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// putInstant renders an instant for a nullable column; the zero time is NULL, which is
// how the domain types spell "unset" (their `omitzero` JSON tags say the same).
func putInstant(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return mustInstant(t)
}

// putInstantPtr renders an optional instant for a nullable column.
func putInstantPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return mustInstant(*t)
}

// instantCol scans an RFC 3339 TEXT column into a time.Time, always in UTC. NULL scans
// as the zero time.
type instantCol struct{ dst *time.Time }

func (c instantCol) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*c.dst = time.Time{}
		return nil
	case string:
		return c.parse(v)
	case []byte:
		return c.parse(string(v))
	case time.Time:
		*c.dst = v.UTC()
		return nil
	default:
		return fmt.Errorf("cannot scan %T as an instant", src)
	}
}

func (c instantCol) parse(s string) error {
	if s == "" {
		*c.dst = time.Time{}
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("parse instant %q: %w", s, err)
	}
	*c.dst = t.UTC()
	return nil
}

// instantPtrCol scans a nullable instant into a *time.Time, leaving it nil for NULL.
// It is used where nil and "the zero instant" have to stay distinguishable, such as
// invites.revoked_at.
type instantPtrCol struct{ dst **time.Time }

func (c instantPtrCol) Scan(src any) error {
	if src == nil {
		*c.dst = nil
		return nil
	}
	var t time.Time
	if err := (instantCol{&t}).Scan(src); err != nil {
		return err
	}
	*c.dst = &t
	return nil
}

// datePtrCol scans a nullable YYYY-MM-DD column into a *domain.Date.
type datePtrCol struct{ dst **domain.Date }

func (c datePtrCol) Scan(src any) error {
	if src == nil {
		*c.dst = nil
		return nil
	}
	var d domain.Date
	if err := d.Scan(src); err != nil {
		return err
	}
	*c.dst = &d
	return nil
}

func boolArg(b bool) int {
	if b {
		return 1
	}
	return 0
}

func i64ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func intptr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func strptr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func putInt64Ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func putIntPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func putStrPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// placeholders returns "?, ?, ?" for n = 3. SQLite has no array parameter, so an
// IN clause has to be built to size.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// idArgs widens ids for passing to database/sql.
func idArgs(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// likeEscape neutralises the LIKE wildcards in a user-supplied needle. Every LIKE in
// this package pairs it with ESCAPE '\'.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// dedupeIDs returns ids sorted and without duplicates, so a caller passing the same id
// twice cannot trip a primary key.
func dedupeIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 0
	for i, v := range out {
		if i == 0 || v != out[n-1] {
			out[n] = v
			n++
		}
	}
	return out[:n]
}
