package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"almanack/internal/clock"
	"almanack/internal/config"
	"almanack/internal/domain"
	"almanack/internal/httpapi"
	"almanack/internal/store"
)

// backupBase is the instant every backup test reasons from. It is a Monday noon, so the
// ISO week boundaries the retention buckets turn on are unambiguous, and it is fixed so
// that a suite running at 23:59 on the last day of a month keeps the same generations as
// one running at lunchtime.
var backupBase = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func testTZ(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatalf("load Europe/Paris: %v", err)
	}
	return loc
}

// liveDatabase builds a database with one recognisable event in it and leaves it open
// until the test ends. Open is the state a real backup runs against — a server still
// serving, with recent writes sitting in the -wal sibling rather than in the main file —
// and a snapshot that misses them is the failure this whole command exists to prevent.
func liveDatabase(t *testing.T) (*store.Store, config.Config, domain.Event) {
	t.Helper()
	root := t.TempDir()
	return liveDatabaseIn(t, filepath.Join(root, "data"), filepath.Join(root, "snapshots"))
}

func liveDatabaseIn(t *testing.T, dataDir, backupDir string) (*store.Store, config.Config, domain.Event) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	cfg := config.Config{
		DataPath:  filepath.Join(dataDir, "almanack.db"),
		BackupDir: backupDir,
		FamilyTZ:  testTZ(t),
	}
	st, err := store.Open(cfg.DataPath, cfg.FamilyTZ, clock.NewFake(backupBase))
	if err != nil {
		t.Fatalf("open source database: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	user, err := st.CreateUser(ctx, domain.User{
		Email: "alex@example.org", DisplayName: "Alex", Color: "#336699",
		Lang: domain.LangFR, WeekStart: time.Monday, TimeFormat: "24h",
	}, "argon2id$fake$alex")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cal, err := st.CreateCalendar(ctx, domain.Calendar{Name: "Maison", Color: "#3b7ddd", CreatorID: user.ID})
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}
	labels, err := st.ListLabels(ctx, cal.ID)
	if err != nil || len(labels) == 0 {
		t.Fatalf("list labels: %v", err)
	}
	ev, err := st.CreateEvent(ctx, domain.Event{
		CalendarID: cal.ID,
		Title:      "Dentiste pour Camille",
		StartsAt:   backupBase.Add(24 * time.Hour),
		EndsAt:     backupBase.Add(25 * time.Hour),
		LabelID:    labels[0].ID,
		CreatedBy:  user.ID,
	}, nil)
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	return st, cfg, ev
}

// rawDatabase builds a SQLite file from statements without going through the store.
// verify's job includes recognising files that are not this application's database, and
// the only way to test that is to hand it some.
func rawDatabase(t *testing.T, path string, stmts ...string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+uriPath(path))
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create %s: %s: %v", path, s, err)
		}
	}
	return path
}

// snapshotName is how takeBackup names a generation, so fixtures and the code under test
// agree on the format by construction rather than by a copied string literal.
func snapshotName(at time.Time) string {
	return "almanack-" + at.UTC().Format(backupTimeLayout) + ".db"
}

// snapshotFixture writes a directory of files named like snapshots. Retention reads the
// timestamp out of the name and never opens the file, so the contents are irrelevant and
// the times can be exact.
func snapshotFixture(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("snapshot"), 0o600); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	return dir
}

func remaining(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func sortedNames(times []time.Time) []string {
	var names []string
	for _, at := range times {
		names = append(names, snapshotName(at))
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// Taking a snapshot
// ---------------------------------------------------------------------------

// The test that proves a backup is a backup. Everything else here — the file exists, it
// is a SQLite file, it passes integrity_check — would pass just as well for a snapshot of
// the wrong database, or of one taken before the write reached the WAL. Only reading the
// row back out settles it.
func TestBackupRoundTripsContent(t *testing.T) {
	ctx := context.Background()
	_, cfg, want := liveDatabase(t)

	res, err := takeBackup(ctx, cfg, clock.Real{}, "", false)
	if err != nil {
		t.Fatalf("takeBackup: %v", err)
	}
	if res.Bytes <= 0 {
		t.Errorf("snapshot reported %d bytes", res.Bytes)
	}

	// Verified from the outside as well: takeBackup checks its own output, so a test
	// that trusted the returned error would be testing nothing but itself.
	if err := verify(ctx, res.Path); err != nil {
		t.Fatalf("the published snapshot does not pass an integrity check: %v", err)
	}

	snap, err := store.Open(res.Path, cfg.FamilyTZ, clock.NewFake(backupBase))
	if err != nil {
		t.Fatalf("open the snapshot as a database: %v", err)
	}
	defer snap.Close()

	got, err := snap.EventByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("read the event back out of the snapshot: %v", err)
	}
	if got.Title != want.Title {
		t.Errorf("title in the snapshot = %q, want %q", got.Title, want.Title)
	}
	if !got.StartsAt.Equal(want.StartsAt) {
		t.Errorf("starts_at in the snapshot = %s, want %s", got.StartsAt, want.StartsAt)
	}
	if got.CalendarID != want.CalendarID || got.LabelID != want.LabelID {
		t.Errorf("event landed in the snapshot detached from its calendar or label: %+v", got)
	}
}

// The name is the only record of when a generation was taken — the retention sweep parses
// it straight back out of the filename — so the format is a contract, not decoration.
// Nothing partial may share the directory with it either: the operator's off-host sync is
// told to copy almanack-*.db and never *.tmp.
func TestBackupNamesSnapshotsByTimestamp(t *testing.T) {
	_, cfg, _ := liveDatabase(t)

	res, err := takeBackup(context.Background(), cfg, clock.Real{}, "", false)
	if err != nil {
		t.Fatalf("takeBackup: %v", err)
	}

	name := filepath.Base(res.Path)
	if !strings.HasPrefix(name, "almanack-") || !strings.HasSuffix(name, ".db") {
		t.Fatalf("snapshot name %q is not almanack-<stamp>.db", name)
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, "almanack-"), ".db")
	if _, err := time.Parse(backupTimeLayout, stamp); err != nil {
		t.Errorf("snapshot name %q does not parse back as a timestamp: %v", name, err)
	}
	if got := remaining(t, cfg.BackupDir); len(got) != 1 || got[0] != name {
		t.Errorf("backup directory holds %v, want only %q", got, name)
	}
}

// A run interrupted between VACUUM INTO and the rename leaves a partial file that is not a
// snapshot. It must not survive to be shipped off-host, and it must not block the next
// run: VACUUM INTO refuses to write to a path that already exists, so a leftover from a
// crashed attempt would otherwise fail every subsequent backup of the same second.
func TestBackupClearsPartialFilesFromAnInterruptedRun(t *testing.T) {
	_, cfg, _ := liveDatabase(t)
	if err := os.MkdirAll(cfg.BackupDir, 0o750); err != nil {
		t.Fatalf("create backup directory: %v", err)
	}
	stale := filepath.Join(cfg.BackupDir, "almanack-20260101-000000.db.tmp")
	if err := os.WriteFile(stale, []byte("half a database"), 0o600); err != nil {
		t.Fatalf("write the stale partial: %v", err)
	}
	// Old enough that no run could still be writing it. Age is the whole distinction
	// between a leftover and a colleague — see the test below.
	aged := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(stale, aged, aged); err != nil {
		t.Fatalf("age the stale partial: %v", err)
	}

	res, err := takeBackup(context.Background(), cfg, clock.Real{}, "", false)
	if err != nil {
		t.Fatalf("takeBackup: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale partial survived the run (stat error %v)", err)
	}
	if got := remaining(t, cfg.BackupDir); len(got) != 1 || got[0] != filepath.Base(res.Path) {
		t.Errorf("backup directory holds %v, want only the new snapshot", got)
	}
}

// The sweep above used to take every *.db.tmp regardless of age, which made two backups
// running at once destroy each other. A snapshot of a large database can easily outlast
// the gap to the next hourly run, and the failure was worse than a deleted file: VACUUM
// INTO went on writing to the unlinked inode, verify then opened the *path* — creating a
// fresh, empty database, which passes integrity_check — and the run failed at the schema
// count. Exit non-zero, OnFailure= mail to the owner, /healthz at 503 until the next
// hour, over a backup that was never in trouble.
func TestBackupLeavesAConcurrentRunsPartialAlone(t *testing.T) {
	_, cfg, _ := liveDatabase(t)
	if err := os.MkdirAll(cfg.BackupDir, 0o750); err != nil {
		t.Fatalf("create backup directory: %v", err)
	}
	// Named for an hour ago, but written now: an earlier run still working on it.
	inFlight := filepath.Join(cfg.BackupDir, "almanack-"+time.Now().UTC().Add(-time.Hour).Format(backupTimeLayout)+".db.tmp")
	if err := os.WriteFile(inFlight, []byte("a snapshot in progress"), 0o600); err != nil {
		t.Fatalf("write the in-flight partial: %v", err)
	}

	if _, err := takeBackup(context.Background(), cfg, clock.Real{}, "", false); err != nil {
		t.Fatalf("takeBackup: %v", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Errorf("a partial file younger than an hour was deleted out from under the run writing it: %v", err)
	}
}

// A snapshot is a complete copy of the family's calendar — every event, every address,
// every password hash. VACUUM INTO creates it under the process umask, which on the
// backup timer's unit is the system default, and the off-host sync preserves the mode.
func TestBackupSnapshotIsNotReadableByOthers(t *testing.T) {
	_, cfg, _ := liveDatabase(t)

	res, err := takeBackup(context.Background(), cfg, clock.Real{}, "", false)
	if err != nil {
		t.Fatalf("takeBackup: %v", err)
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("stat the snapshot: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("snapshot mode = %#o, want %#o", got, 0o600)
	}
}

// TestSnapshotIsPrivateFromTheMomentItExists covers the half a chmod cannot.
//
// VACUUM INTO creates the file and then fills it, so the mode it is created with is the
// mode it holds for the whole of the copy — long enough to matter on a calendar with
// photographs in it. Tightening it afterwards leaves that window open, and does nothing
// at all about a descriptor another process opened during it. The test above looks at
// the finished artifact, which is why it passed while the window was there.
//
// Watching for the partial file mid-write would be a race that passes by seeing nothing,
// so this asserts the mechanism instead: inside withRestrictiveUmask, a file created the
// way SQLite creates one — mode 0666 through the umask — comes out private. The
// deliberately permissive umask around it stands in for the stock 0022, so this fails on
// a machine that happens to run 0077 too.
func TestSnapshotIsPrivateFromTheMomentItExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.db")

	err := withRestrictiveUmask(func() error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		return f.Close()
	})
	if err != nil {
		t.Fatalf("create under the restrictive umask: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if leaked := info.Mode().Perm() &^ 0o700; leaked != 0 {
		t.Errorf("a file created the way VACUUM INTO creates one was readable by group or others (%#o); "+
			"it is world-readable for as long as the snapshot takes to write", leaked)
	}
}

// Two runs in quick succession share a second, and therefore a name. Both must still
// succeed and leave a verifiable snapshot behind; an hourly timer that fires twice, or a
// nervous operator running the command by hand, must not produce a failure.
func TestBackupSucceedsTwiceInARow(t *testing.T) {
	ctx := context.Background()
	_, cfg, _ := liveDatabase(t)

	for i := range 2 {
		res, err := takeBackup(ctx, cfg, clock.Real{}, "", false)
		if err != nil {
			t.Fatalf("takeBackup run %d: %v", i+1, err)
		}
		if err := verify(ctx, res.Path); err != nil {
			t.Fatalf("snapshot from run %d is not intact: %v", i+1, err)
		}
	}
	for _, name := range remaining(t, cfg.BackupDir) {
		if strings.HasSuffix(name, ".tmp") {
			t.Errorf("a partial file was left behind: %s", name)
		}
	}
}

// sql.Open creates a database at a path that does not exist, so without the stat a
// snapshot taken while the data volume was unmounted would succeed, verify an empty
// file, and report a healthy backup of nothing.
func TestBackupFailsWhenTheDatabaseIsMissing(t *testing.T) {
	cfg := config.Config{
		DataPath: filepath.Join(t.TempDir(), "gone.db"),
		FamilyTZ: testTZ(t),
	}
	dir := t.TempDir()

	if _, err := takeBackup(context.Background(), cfg, clock.Real{}, dir, false); err == nil {
		t.Fatal("takeBackup succeeded with no database to back up")
	}
	if got := remaining(t, dir); len(got) != 0 {
		t.Errorf("a failed run left %v in the backup directory", got)
	}
}

// Writing snapshots into whatever the process's working directory happens to be is worse
// than refusing, so an unconfigured destination is an error rather than a default.
func TestBackupNeedsADirectory(t *testing.T) {
	_, cfg, _ := liveDatabase(t)
	cfg.BackupDir = ""

	if _, err := takeBackup(context.Background(), cfg, clock.Real{}, "", false); err == nil {
		t.Fatal("takeBackup accepted an empty destination")
	}
}

// docs/deployment.md: "Exits non-zero if the snapshot is not intact, which is the signal
// to alert on." A snapshot that fails verification must never be published under a name
// the restore procedure would trust — the operator would find out only during a restore,
// which is the one moment there is nothing left to fall back on.
func TestBackupRefusesToPublishASnapshotThatFailsVerification(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		DataPath: rawDatabase(t, filepath.Join(root, "impostor.db"), `CREATE TABLE t (a)`),
		FamilyTZ: testTZ(t),
	}
	dir := filepath.Join(root, "snapshots")

	if _, err := takeBackup(context.Background(), cfg, clock.Real{}, dir, false); err == nil {
		t.Fatal("takeBackup published a snapshot of a database that is not this application's")
	}
	if got := remaining(t, dir); len(got) != 0 {
		t.Errorf("the rejected snapshot was left behind as %v", got)
	}
}

// A data directory containing '?' or '#' is read as URI syntax by SQLite, and the backup
// would then open — and verify, and report as a success — a different, empty database.
func TestURIPath(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"an ordinary path is untouched", "/srv/almanack/almanack.db", "/srv/almanack/almanack.db"},
		{"a fragment", "/srv/almanack#2/almanack.db", "/srv/almanack%232/almanack.db"},
		{"a query", "/srv/why?/almanack.db", "/srv/why%3f/almanack.db"},
		{"a percent, which must be escaped without escaping its own escape", "/srv/100%/x.db", "/srv/100%25/x.db"},
		{"all three at once", "/srv/a%b?c#d.db", "/srv/a%25b%3fc%23d.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uriPath(tc.in); got != tc.want {
				t.Errorf("uriPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The escaping is only worth anything if the whole command works on such a path, so this
// runs the real thing against a data directory and a backup directory that both contain
// characters SQLite would otherwise read as URI syntax.
func TestBackupHandlesAwkwardPathCharacters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	_, cfg, want := liveDatabaseIn(t, filepath.Join(root, "data#2"), filepath.Join(root, "snap?shots"))

	res, err := takeBackup(ctx, cfg, clock.Real{}, "", false)
	if err != nil {
		t.Fatalf("takeBackup: %v", err)
	}
	snap, err := store.Open(res.Path, cfg.FamilyTZ, clock.NewFake(backupBase))
	if err != nil {
		t.Fatalf("open the snapshot: %v", err)
	}
	defer snap.Close()
	got, err := snap.EventByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("read the event back out of the snapshot: %v", err)
	}
	if got.Title != want.Title {
		t.Errorf("title in the snapshot = %q, want %q", got.Title, want.Title)
	}
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

// damage overwrites bytes in the middle of a database file and leaves the header alone,
// so SQLite still opens it happily. That is what silent corruption looks like from the
// outside, and noticing it is the entire reason verify checks the copy.
func damage(t *testing.T, path string) {
	t.Helper()
	const offset = 4096*3 + 50
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() < offset+256 {
		t.Fatalf("%s is %d bytes: too small to damage a page other than the header", path, info.Size())
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xa5}, 200), offset); err != nil {
		t.Fatalf("damage %s: %v", path, err)
	}
}

// verify is the alerting contract: a non-zero exit means an operator gets woken up, and
// silence means the family's calendar is recoverable. Each case here is a way a file can
// look like a backup and not be one.
func TestVerify(t *testing.T) {
	// A table full enough to span several pages, so damaging one hits a b-tree rather
	// than free space.
	const bulk = `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 500)
	              INSERT INTO t(b) SELECT 'row ' || i || ' padding padding padding padding' FROM n`

	cases := []struct {
		name  string
		build func(t *testing.T, path string)
		want  string // substring the failure must contain; empty means it must pass
	}{
		{
			name: "a migrated database passes",
			build: func(t *testing.T, path string) {
				st, err := store.Open(path, testTZ(t), clock.NewFake(backupBase))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				st.Close()
			},
		},
		{
			name: "a file that is not a database at all",
			build: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("almanack backups are not tarballs"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			want: "integrity check",
		},
		{
			name: "a database whose pages have been damaged",
			build: func(t *testing.T, path string) {
				rawDatabase(t, path,
					`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`,
					`INSERT INTO schema_migrations VALUES (1)`,
					`CREATE TABLE t (a INTEGER PRIMARY KEY, b TEXT)`,
					bulk,
					`CREATE INDEX t_b ON t (b)`)
				damage(t, path)
			},
			want: "THE DATABASE IS DAMAGED",
		},
		{
			// integrity_check validates b-trees, not the relationships between them, and
			// orphan rows are something the application itself could produce.
			name: "a database with a dangling reference",
			build: func(t *testing.T, path string) {
				rawDatabase(t, path,
					`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`,
					`INSERT INTO schema_migrations VALUES (1)`,
					`CREATE TABLE calendars (id INTEGER PRIMARY KEY)`,
					`CREATE TABLE events (id INTEGER PRIMARY KEY, calendar_id INTEGER REFERENCES calendars(id))`,
					`INSERT INTO events (id, calendar_id) VALUES (1, 99)`)
			},
			want: "dangling reference",
		},
		{
			// A structurally sound file that is somebody else's database would pass every
			// check above, and restoring it would replace the calendar with a stranger's.
			name: "a sound database that is not this application's",
			build: func(t *testing.T, path string) {
				rawDatabase(t, path, `CREATE TABLE t (a)`)
			},
			want: "schema_migrations",
		},
		{
			name: "a database no migration has ever been applied to",
			build: func(t *testing.T, path string) {
				rawDatabase(t, path, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`)
			},
			want: "no applied migrations",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.db")
			tc.build(t, path)

			err := verify(context.Background(), path)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("verify rejected a good snapshot: %v", err)
			case tc.want != "" && err == nil:
				t.Fatal("verify accepted the snapshot; it should have been rejected")
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("verify said %q, which does not mention %q", err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The /healthz breadcrumb
// ---------------------------------------------------------------------------

// The health endpoint has always known how to report "the last backup is too old", and
// for a while nothing wrote the value it reads — so a server whose backup timer was never
// installed reported itself healthy indefinitely.
func TestRunBackupRecordsSuccessForHealthz(t *testing.T) {
	ctx := context.Background()
	st, cfg, _ := liveDatabase(t)

	if _, err := runBackup(ctx, cfg, clock.Real{}, "", false); err != nil {
		t.Fatalf("runBackup: %v", err)
	}

	result, err := st.GetMeta(ctx, httpapi.MetaLastBackupResult)
	if err != nil {
		t.Fatalf("read the recorded result: %v", err)
	}
	if result != "ok" {
		t.Errorf("recorded result = %q, want %q", result, "ok")
	}
	at, err := st.GetMeta(ctx, httpapi.MetaLastBackupAt)
	if err != nil {
		t.Fatalf("read the recorded time: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("recorded time %q is not RFC 3339: %v", at, err)
	}
}

// A failure has to be recorded too. The operator of a family server is not watching
// stderr; /healthz is where they find out, and a failed backup that leaves the previous
// "ok" in place is indistinguishable from a healthy one.
func TestRunBackupRecordsFailureForHealthz(t *testing.T) {
	ctx := context.Background()
	st, cfg, _ := liveDatabase(t)
	cfg.BackupDir = ""

	if _, err := runBackup(ctx, cfg, clock.Real{}, "", false); err == nil {
		t.Fatal("runBackup succeeded with no destination")
	}

	result, err := st.GetMeta(ctx, httpapi.MetaLastBackupResult)
	if err != nil {
		t.Fatalf("read the recorded result: %v", err)
	}
	if result == "ok" || result == "" {
		t.Errorf("recorded result = %q, want the reason the backup failed", result)
	}
}

// Recording the outcome must not bring a database into existence. store.Open creates and
// fully migrates the file when it is not there, and the breadcrumb was written on the
// failure path too — so a backup run against a data volume that had failed to mount
// exited non-zero, correctly, and then left a fresh empty calendar at the path the
// family's used to be at. The next `almanack serve` started on it happily and /healthz
// went green. It also defeated the stat in takeBackup, whose whole purpose is to refuse
// to back up nothing.
func TestFailedBackupDoesNotRecreateAMissingDatabase(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	cfg := config.Config{
		DataPath: filepath.Join(dataDir, "almanack.db"),
		FamilyTZ: testTZ(t),
	}

	if _, err := runBackup(context.Background(), cfg, clock.Real{}, filepath.Join(root, "snapshots"), false); err == nil {
		t.Fatal("runBackup succeeded with no database to back up")
	}
	if got := remaining(t, dataDir); len(got) != 0 {
		t.Errorf("a failed run left %v where the database should be", got)
	}
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

// THE regression test, at the seam. Every limit at zero reads naturally as "do not
// prune", and used to mean the opposite: no bucket kept anything, so the sweep removed
// every snapshot in the directory.
func TestPruneKeepsEverythingWhenNoRetentionIsConfigured(t *testing.T) {
	fresh := snapshotName(backupBase)
	dir := snapshotFixture(t,
		fresh,
		snapshotName(backupBase.Add(-time.Hour)),
		snapshotName(backupBase.Add(-30*24*time.Hour)),
	)
	before := remaining(t, dir)

	n, err := pruneBackups(dir, config.Config{}, fresh)
	if err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	if n != 0 {
		t.Errorf("pruneBackups removed %d snapshot(s) with no retention configured", n)
	}
	if got := remaining(t, dir); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("directory is now %v, want %v", got, before)
	}
}

// The same regression, as the operator meets it: `almanack backup <dir> --prune` on a
// configuration whose retention settings are all zero. Version 0.2.0 had to fix this
// deleting every snapshot including the one it had just written, verified, and printed
// to the terminal as a success — a backup command reporting success while destroying the
// backups is the worst failure this program has.
func TestBackupWithPruneKeepsTheSnapshotItJustWrote(t *testing.T) {
	_, cfg, _ := liveDatabase(t)
	if err := os.MkdirAll(cfg.BackupDir, 0o750); err != nil {
		t.Fatalf("create backup directory: %v", err)
	}
	older := snapshotName(backupBase.Add(-48 * time.Hour))
	if err := os.WriteFile(filepath.Join(cfg.BackupDir, older), []byte("snapshot"), 0o600); err != nil {
		t.Fatalf("write the older snapshot: %v", err)
	}

	res, err := takeBackup(context.Background(), cfg, clock.Real{}, "", true)
	if err != nil {
		t.Fatalf("takeBackup: %v", err)
	}
	if res.Pruned != 0 {
		t.Errorf("the run reported %d snapshot(s) pruned with no retention configured", res.Pruned)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("the run reported success and then deleted its own snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.BackupDir, older)); err != nil {
		t.Errorf("the older generation was deleted too: %v", err)
	}
}

// Pruning has to remove old generations, and has to be incapable of removing the one the
// run just verified. A file dated after the run's own snapshot separates the two: the
// hourly bucket fills up on it, so only the explicit guard saves the fresh snapshot, and
// the guard is passed a bare filename where the sweep compares bare filenames. A run that
// dated the guard differently from the directory listing would look exactly like this
// test passing on the count and failing on the file.
//
// The command stamps its own snapshot from the wall clock, so the file that has to
// outrank it is dated from the wall clock too; every other retention test here is
// anchored to a fake one.
func TestBackupPrunesOldGenerationsWithoutTouchingItsOwn(t *testing.T) {
	_, cfg, _ := liveDatabase(t)
	cfg.KeepHourly = 1
	if err := os.MkdirAll(cfg.BackupDir, 0o750); err != nil {
		t.Fatalf("create backup directory: %v", err)
	}
	future := snapshotName(time.Now().UTC().AddDate(1, 0, 0))
	stale := snapshotName(backupBase.AddDate(-1, 0, 0))
	for _, name := range []string{future, stale} {
		if err := os.WriteFile(filepath.Join(cfg.BackupDir, name), []byte("snapshot"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	res, err := takeBackup(context.Background(), cfg, clock.Real{}, "", true)
	if err != nil {
		t.Fatalf("takeBackup: %v", err)
	}
	if res.Pruned != 1 {
		t.Errorf("reported %d snapshot(s) pruned, want 1", res.Pruned)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("the run pruned the snapshot it had just reported as a success: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.BackupDir, future)); err != nil {
		t.Errorf("the future-dated generation was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.BackupDir, stale)); !os.IsNotExist(err) {
		t.Errorf("last year's generation survived an hourly retention of one (stat error %v)", err)
	}
}

// The directory on the command line wins over the configured one. An operator taking an
// ad-hoc snapshot before an upgrade names a destination precisely so that it does not
// join the hourly timer's directory, where retention would eventually remove it.
func TestBackupWritesWhereItIsTold(t *testing.T) {
	_, cfg, _ := liveDatabase(t)
	explicit := filepath.Join(t.TempDir(), "before-the-upgrade")

	res, err := takeBackup(context.Background(), cfg, clock.Real{}, explicit, false)
	if err != nil {
		t.Fatalf("takeBackup: %v", err)
	}
	if filepath.Dir(res.Path) != explicit {
		t.Errorf("snapshot written to %s, want a file in %s", res.Path, explicit)
	}
	if _, err := os.Stat(cfg.BackupDir); !os.IsNotExist(err) {
		t.Errorf("the configured directory was used anyway (stat error %v)", err)
	}
}

// Retention keeps generations rather than a flat window, because corruption is often
// noticed late and "keep the last 14 days" quietly destroys every clean copy while nobody
// is looking. Each case below is one bucket's arithmetic against the same history.
//
// keepAlways is empty in the bucket cases so that what is measured is the arithmetic
// alone: the run's own snapshot is the newest here, so any bucket that works keeps it
// without needing the guard. The guard has its own test.
func TestPruneGenerations(t *testing.T) {
	// Anchored to a fake clock rather than the wall clock: these buckets turn on
	// calendar boundaries, and a suite that ran at midnight would keep a different set.
	now := clock.NewFake(backupBase).Now()

	var (
		fresh      = now                           // Mon 2026-07-27 12:00, the snapshot this run wrote
		at1130     = now.Add(-30 * time.Minute)    // Mon 11:30
		at1100     = now.Add(-time.Hour)           // Mon 11:00, same hour as 11:30
		at1000     = now.Add(-2 * time.Hour)       // Mon 10:00
		sunday     = now.Add(-24 * time.Hour)      // Sun 2026-07-26
		saturday   = now.Add(-48 * time.Hour)      // Sat 2026-07-25, same ISO week as Sunday
		weekStart  = now.Add(-7 * 24 * time.Hour)  // Mon 2026-07-20, opening that same ISO week
		weekBefore = now.Add(-14 * 24 * time.Hour) // Mon 2026-07-13, the ISO week before it
		june       = now.Add(-27 * 24 * time.Hour) // Tue 2026-06-30
		may        = now.Add(-57 * 24 * time.Hour) // Sun 2026-05-31
	)
	all := []time.Time{fresh, at1130, at1100, at1000, sunday, saturday, weekStart, weekBefore, june, may}

	cases := []struct {
		name       string
		cfg        config.Config
		keepAlways string
		kept       []time.Time
	}{
		{
			name:       "no retention configured deletes nothing",
			cfg:        config.Config{},
			keepAlways: snapshotName(fresh),
			kept:       all,
		},
		{
			name: "hourly keeps the newest snapshot in each of the last N hours",
			cfg:  config.Config{KeepHourly: 3},
			kept: []time.Time{fresh, at1130, at1000},
		},
		{
			name: "daily keeps the newest snapshot in each of the last N days",
			cfg:  config.Config{KeepDaily: 2},
			kept: []time.Time{fresh, sunday},
		},
		{
			// The Saturday and the previous Monday share an ISO week with the Sunday, so
			// only the Sunday survives it. Weeks start on Monday, as everywhere else here.
			name: "weekly keeps the newest snapshot in each of the last N ISO weeks",
			cfg:  config.Config{KeepWeekly: 3},
			kept: []time.Time{fresh, sunday, weekBefore},
		},
		{
			name: "monthly keeps the newest snapshot in each of the last N months",
			cfg:  config.Config{KeepMonthly: 2},
			kept: []time.Time{fresh, june},
		},
		{
			// The buckets are a union, not a sequence of filters. An hourly limit of two
			// must not be able to delete the only surviving copy of last month.
			name: "the generations add to each other rather than override",
			cfg:  config.Config{KeepHourly: 2, KeepDaily: 2, KeepWeekly: 2, KeepMonthly: 2},
			kept: []time.Time{fresh, at1130, sunday, june},
		},
		{
			// A generation is a period, not a file. However generous the limits, two
			// snapshots taken in the same hour are one hourly generation and the older is
			// dropped — which is what stops a timer that fires every five minutes from
			// pushing last month's copy out of the directory.
			name: "however generous the limits, one snapshot per period is kept",
			cfg:  config.Config{KeepHourly: 99, KeepDaily: 99, KeepWeekly: 99, KeepMonthly: 99},
			kept: []time.Time{fresh, at1130, at1000, sunday, saturday, weekStart, weekBefore, june, may},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := snapshotFixture(t, sortedNames(all)...)

			n, err := pruneBackups(dir, tc.cfg, tc.keepAlways)
			if err != nil {
				t.Fatalf("pruneBackups: %v", err)
			}
			want := sortedNames(tc.kept)
			if got := remaining(t, dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("kept\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if n != len(all)-len(tc.kept) {
				t.Errorf("reported %d removed, want %d", n, len(all)-len(tc.kept))
			}
		})
	}
}

// Whatever the arithmetic says, the snapshot this run verified is not a candidate for
// deletion. A file dated in the future — a clock stepped back by a botched time sync, or
// a generation copied in from another host — makes the fresh snapshot no longer the
// newest, and an hourly bucket of one would then fill up before ever reaching it.
func TestPruneNeverDeletesTheSnapshotJustWritten(t *testing.T) {
	future := backupBase.Add(24 * time.Hour)
	fresh := backupBase
	older := backupBase.Add(-time.Hour)
	dir := snapshotFixture(t, snapshotName(future), snapshotName(fresh), snapshotName(older))

	n, err := pruneBackups(dir, config.Config{KeepHourly: 1}, snapshotName(fresh))
	if err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	want := sortedNames([]time.Time{future, fresh})
	if got := remaining(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kept %v, want %v", got, want)
	}
	if n != 1 {
		t.Errorf("reported %d removed, want 1", n)
	}
}

// Retention deletes only files it can prove it wrote. A partial counted as a generation
// would be kept as one and could be restored from; an operator's own compressed copy, or
// anything else sharing the directory, is not this command's to delete.
func TestPruneLeavesFilesThatAreNotSnapshotsAlone(t *testing.T) {
	keep := snapshotName(backupBase)
	drop := snapshotName(backupBase.Add(-time.Hour))
	strangers := []string{
		snapshotName(backupBase.Add(-2*time.Hour)) + ".tmp", // an interrupted run's partial
		"almanack-20260726-120000.db.gz",                    // the operator's own archive
		"almanack-yesterday.db",                             // ours by name, but not a timestamp
		"notes.txt",
	}
	dir := snapshotFixture(t, append([]string{keep, drop}, strangers...)...)
	// A directory named like a snapshot must not be removed either.
	nested := filepath.Join(dir, "almanack-20250101-000000.db")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatalf("create the nested directory: %v", err)
	}

	n, err := pruneBackups(dir, config.Config{KeepHourly: 1}, "")
	if err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	if n != 1 {
		t.Errorf("reported %d removed, want 1", n)
	}
	want := append([]string{keep, filepath.Base(nested)}, strangers...)
	sort.Strings(want)
	if got := remaining(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kept %v, want %v", got, want)
	}
}

// The pre-migration snapshot is written for real, into a directory of its own, and the
// file it leaves is a usable database.
//
// docs/deployment.md and docs/install.md promised this snapshot for a long time before
// anything took one, and the same paragraphs said a rollback was "putting the old binary
// back". It is not — a binary refuses to open a schema newer than it knows — so the copy
// named here was the only way out of a bad release, and there was none (#22).
func TestThePreMigrationSnapshotIsWrittenAndUsable(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		DataPath:  filepath.Join(dir, "almanack.db"),
		BackupDir: filepath.Join(dir, "backups"),
		FamilyTZ:  testTZ(t),
		TZName:    "Europe/Paris",
	}
	clk := clock.NewFake(backupBase)

	// A database that already exists, as it would when a release is being upgraded.
	st, err := store.Open(cfg.DataPath, cfg.FamilyTZ, clk)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Close()

	// The versions the hook is handed do not have to be real for it to do its work; the
	// store's own tests pin when it is called and with what.
	if err := preMigrationSnapshot(cfg, clk)(context.Background(), 2, 7); err != nil {
		t.Fatalf("take the pre-migration snapshot: %v", err)
	}

	preDir := filepath.Join(cfg.BackupDir, "pre-migration")
	var snapshots []string
	entries, err := os.ReadDir(preDir)
	if err != nil {
		t.Fatalf("no pre-migration directory: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".db") {
			snapshots = append(snapshots, e.Name())
		}
	}
	if len(snapshots) != 1 {
		t.Fatalf("pre-migration snapshots = %v, want exactly one", snapshots)
	}

	// It has to be a database somebody could actually restore, not merely a file.
	snap := filepath.Join(preDir, snapshots[0])
	if err := verify(context.Background(), snap); err != nil {
		t.Errorf("the pre-migration snapshot does not verify: %v", err)
	}
	// And private: it is the whole calendar, every address and every password hash.
	info, err := os.Stat(snap)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("snapshot mode = %o, want 600", mode)
	}

	// It is kept out of the ordinary backup directory, where the retention policy would
	// eventually delete it. This is the one copy whose value is set by an event rather
	// than by its age: a bad release may not be noticed for days.
	hourly, err := os.ReadDir(cfg.BackupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	for _, e := range hourly {
		if strings.HasSuffix(e.Name(), ".db") {
			t.Errorf("the pre-migration snapshot landed in the pruned directory as %s", e.Name())
		}
	}
}

// Creating a database is not an upgrade. There is no earlier state to roll back to, the
// file is empty until the first migration runs, and a snapshot of it would fail
// verification for the good reason that it holds no schema at all.
func TestAFreshDatabaseTakesNoPreMigrationSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		DataPath:  filepath.Join(dir, "almanack.db"),
		BackupDir: filepath.Join(dir, "backups"),
		FamilyTZ:  testTZ(t),
		TZName:    "Europe/Paris",
	}
	clk := clock.NewFake(backupBase)

	st, err := store.OpenWith(cfg.DataPath, cfg.FamilyTZ, clk, store.Options{
		BeforeMigrate: preMigrationSnapshot(cfg, clk),
	})
	if err != nil {
		t.Fatalf("creating a database refused to start: %v", err)
	}
	st.Close()

	if entries, err := os.ReadDir(filepath.Join(cfg.BackupDir, "pre-migration")); err == nil && len(entries) > 0 {
		t.Errorf("a fresh database wrote %d pre-migration snapshot(s)", len(entries))
	}
}

// With nowhere to put the snapshot the server does not start. Migrating without the
// fallback the documentation tells an operator to restore from is the one outcome that
// must not happen quietly.
func TestMigratingWithoutABackupDirRefusesToStart(t *testing.T) {
	cfg := config.Config{
		DataPath: filepath.Join(t.TempDir(), "almanack.db"),
		FamilyTZ: testTZ(t),
		TZName:   "Europe/Paris",
	}
	err := preMigrationSnapshot(cfg, clock.NewFake(backupBase))(context.Background(), 2, 7)
	if err == nil {
		t.Fatal("the migration proceeded with no backup directory configured")
	}
	if !strings.Contains(err.Error(), "ALMANACK_BACKUP_DIR") {
		t.Errorf("the error does not name the setting to fix: %v", err)
	}
}
