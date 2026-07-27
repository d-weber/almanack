package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"almanack/internal/clock"
	"almanack/internal/config"
	"almanack/internal/httpapi"
	"almanack/internal/store"
)

// Backups are the last line of defence for a family's calendar, so this command is
// deliberately paranoid: it verifies what it wrote before it counts, and it never
// leaves a half-written file where a restore could find it.
//
// The sequence — VACUUM INTO a temporary name, integrity-check the *output*, fsync,
// atomically rename — matters in every part. VACUUM INTO produces a transactionally
// consistent snapshot of a live WAL database, but an interrupted one leaves a corrupt
// partial file behind, and cleaning that up is the caller's job. Checking the copy
// rather than the source is what turns silent corruption into a loud failure: a
// non-zero exit here is the signal the operator alerts on.

const backupTimeLayout = "20060102-150405"

// uriPath percent-encodes the characters that would otherwise be read as URI syntax,
// so a data directory containing '?' or '#' opens the file it names rather than a
// different, empty database.
func uriPath(p string) string {
	r := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")
	return r.Replace(p)
}

type backupResult struct {
	Path    string
	Bytes   int64
	Elapsed time.Duration
	Pruned  int
}

// runBackup takes a snapshot and records the outcome where /healthz can find it.
//
// Recording matters as much as the snapshot: the health endpoint has always known
// how to report "the last backup is too old", but nothing wrote the value, so the
// check could never fire. A server whose backup timer was never installed reported
// itself healthy indefinitely — the exact silent failure the whole design is
// arranged to prevent.
func runBackup(ctx context.Context, cfg config.Config, dir string, prune bool) (backupResult, error) {
	res, err := takeBackup(ctx, cfg, dir, prune)
	outcome := "ok"
	if err != nil {
		outcome = err.Error()
	}
	// No database, no breadcrumb. store.Open creates and fully migrates the file when it
	// is not there, so recording an outcome against a data path that has gone — the
	// unmounted volume takeBackup stats for a few lines above — used to leave a fresh
	// empty calendar exactly where the family's had been, and the next `almanack serve`
	// started on it without complaint. The non-zero exit is the whole signal in that
	// case, and the deployment contract already says to alert on it.
	if _, statErr := os.Stat(cfg.DataPath); statErr != nil {
		slog.Warn("not recording the backup result: there is no database at the data path",
			"path", cfg.DataPath, "error", statErr)
		return res, err
	}
	if noteErr := recordBackupOutcome(ctx, cfg, outcome); noteErr != nil {
		slog.Warn("could not record the backup result for /healthz", "error", noteErr)
	}
	return res, err
}

// recordBackupOutcome opens the live database briefly to leave a breadcrumb. A
// failure here is reported but never masks the backup's own result.
func recordBackupOutcome(ctx context.Context, cfg config.Config, outcome string) error {
	st, err := store.Open(cfg.DataPath, cfg.FamilyTZ, clock.Real{})
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.SetMeta(ctx, httpapi.MetaLastBackupAt, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return st.SetMeta(ctx, httpapi.MetaLastBackupResult, outcome)
}

func takeBackup(ctx context.Context, cfg config.Config, dir string, prune bool) (backupResult, error) {
	start := time.Now()
	if dir == "" {
		dir = cfg.BackupDir
	}
	if dir == "" {
		return backupResult{}, fmt.Errorf("no backup directory: pass one as an argument or set ALMANACK_BACKUP_DIR")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return backupResult{}, fmt.Errorf("create backup directory %s: %w", dir, err)
	}
	if err := removeStaleTemps(dir); err != nil {
		return backupResult{}, err
	}

	stamp := time.Now().UTC().Format(backupTimeLayout)
	final := filepath.Join(dir, "almanack-"+stamp+".db")
	// The PID is in the temporary name so that two runs starting in the same second —
	// the hourly timer and an operator at a shell — cannot end up writing to, and
	// removing, one file.
	tmp := fmt.Sprintf("%s.%d.tmp", final, os.Getpid())
	// VACUUM INTO refuses to overwrite, and a same-second rerun would otherwise fail
	// on the leftover from the previous attempt.
	_ = os.Remove(tmp)

	// sql.Open would happily create a database at a path that does not exist — on an
	// unmounted volume that means silently backing up nothing.
	if _, err := os.Stat(cfg.DataPath); err != nil {
		return backupResult{}, fmt.Errorf("database %s: %w", cfg.DataPath, err)
	}
	src, err := sql.Open("sqlite", "file:"+uriPath(cfg.DataPath)+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return backupResult{}, fmt.Errorf("open database: %w", err)
	}
	defer src.Close()

	// A snapshot is the whole calendar: every address, every password hash. VACUUM INTO
	// creates the file itself, under the process umask — 0022 on a stock system, which
	// leaves it world-readable — and the off-host sync preserves the mode it finds.
	//
	// The umask is what makes the file private; the chmod below only makes sure of it.
	// Chmod alone was not enough, and looked as though it were: it runs after VACUUM
	// INTO returns, so on a large database the partial snapshot sat world-readable for
	// the whole of the copy, and a descriptor opened during that window keeps reading
	// the finished file afterwards — revoking the mode does not revoke an open handle.
	// The containing directory is 0750, but MkdirAll leaves an existing directory's
	// permissions alone, so an operator's own `mkdir -p` is all it takes for that
	// window to be reachable.
	err = withRestrictiveUmask(func() error {
		_, err := src.ExecContext(ctx, "VACUUM INTO ?", tmp)
		return err
	})
	if err != nil {
		_ = os.Remove(tmp)
		return backupResult{}, fmt.Errorf("snapshot to %s: %w", tmp, err)
	}

	// Belt to the umask's braces, and the only protection on a platform without one.
	// Before the rename, so the file is never readable under its final name either.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return backupResult{}, fmt.Errorf("restrict permissions on %s: %w", tmp, err)
	}

	if err := verify(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return backupResult{}, err
	}

	if err := fsyncFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return backupResult{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return backupResult{}, fmt.Errorf("publish snapshot: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return backupResult{}, err
	}

	info, err := os.Stat(final)
	if err != nil {
		return backupResult{}, fmt.Errorf("stat snapshot: %w", err)
	}
	res := backupResult{Path: final, Bytes: info.Size(), Elapsed: time.Since(start)}

	if prune {
		n, err := pruneBackups(dir, cfg, filepath.Base(final))
		if err != nil {
			return res, fmt.Errorf("prune old snapshots: %w", err)
		}
		res.Pruned = n
	}
	return res, nil
}

// verify runs SQLite's own integrity check against the snapshot. Checking the copy is
// the point: a backup that faithfully preserves a corrupt page is not a backup.
func verify(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+uriPath(path)+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("open snapshot for verification: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	defer rows.Close()

	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("integrity check: %w", err)
		}
		if line != "ok" {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("snapshot failed its integrity check — THE DATABASE IS DAMAGED, restore from an older generation:\n  %s",
			strings.Join(problems, "\n  "))
	}

	// integrity_check validates the b-trees, not the relationships between them.
	// Orphan rows are something the application itself can produce, and a snapshot
	// is the last place they should pass unnoticed.
	fkRows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	var broken []string
	for fkRows.Next() {
		var table, parent string
		var rowid, fkid sql.NullInt64
		if err := fkRows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			fkRows.Close()
			return fmt.Errorf("foreign key check: %w", err)
		}
		broken = append(broken, fmt.Sprintf("%s row %d references a missing %s", table, rowid.Int64, parent))
	}
	fkRows.Close()
	if err := fkRows.Err(); err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	if len(broken) > 0 {
		return fmt.Errorf("snapshot has %d dangling reference(s) — the database is inconsistent:\n  %s",
			len(broken), strings.Join(broken, "\n  "))
	}

	// A structurally sound file that is not this application's database would also
	// pass integrity_check, so confirm the schema is present too.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		return fmt.Errorf("snapshot has no schema_migrations table: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("snapshot contains no applied migrations")
	}
	return nil
}

// pruneBackups keeps generations rather than a flat window: hourly for a couple of
// days, then one a day, one a week, one a month. Corruption is often noticed late,
// and a flat "keep 14 days" quietly destroys every clean copy while nobody is looking.
func pruneBackups(dir string, cfg config.Config, keepAlways string) (int, error) {
	// Every limit at zero reads naturally as "do not prune", and used to mean the
	// opposite: no bucket kept anything, so the sweep removed every snapshot in the
	// directory — including the one this run had just written and already reported
	// as a success.
	if cfg.KeepHourly <= 0 && cfg.KeepDaily <= 0 && cfg.KeepWeekly <= 0 && cfg.KeepMonthly <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}

	type snap struct {
		name string
		when time.Time
	}
	var snaps []snap
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "almanack-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "almanack-"), ".db")
		when, err := time.Parse(backupTimeLayout, stamp)
		if err != nil {
			continue // not ours; leave it alone
		}
		snaps = append(snaps, snap{name: e.Name(), when: when})
	}
	// Newest first, so the first snapshot seen in each bucket is the one kept.
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].when.After(snaps[j].when) })

	keep := map[string]bool{}
	// Whatever the retention arithmetic says, the snapshot this run just verified
	// is not a candidate for deletion.
	if keepAlways != "" {
		keep[keepAlways] = true
	}
	buckets := []struct {
		limit  int
		format string
	}{
		{cfg.KeepHourly, "2006010215"},
		{cfg.KeepDaily, "20060102"},
		{cfg.KeepWeekly, "2006W"},   // completed below with the ISO week
		{cfg.KeepMonthly, "200601"}, //
	}
	for i, b := range buckets {
		if b.limit <= 0 {
			continue
		}
		seen := map[string]bool{}
		for _, s := range snaps {
			var key string
			if i == 2 {
				y, w := s.when.ISOWeek()
				key = fmt.Sprintf("%dW%02d", y, w)
			} else {
				key = s.when.Format(b.format)
			}
			if seen[key] {
				continue
			}
			if len(seen) >= b.limit {
				break
			}
			seen[key] = true
			keep[s.name] = true
		}
	}

	removed := 0
	for _, s := range snaps {
		if keep[s.name] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, s.name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// staleTempAfter is how long a partial file must have gone untouched before it is taken
// for abandoned rather than in progress. A snapshot of a family calendar takes seconds;
// an hour is generous enough that only a killed run qualifies.
const staleTempAfter = time.Hour

// removeStaleTemps clears partial files left by an interrupted run, so they can never
// be mistaken for a snapshot or shipped off-host by a sync job.
//
// Only files that have not been written to for an hour: a partial belonging to a run
// that is still going is not stale, and deleting it used to break that run in the worst
// way available. VACUUM INTO carried on filling the unlinked inode, verify then opened
// the path and created a fresh empty database there — which passes integrity_check — and
// the run failed at the schema count. The operator got a failure mail and a 503 from
// /healthz over a backup that was doing nothing wrong.
func removeStaleTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !isPartialSnapshot(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < staleTempAfter {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("remove stale partial %s: %w", e.Name(), err)
		}
	}
	return nil
}

// isPartialSnapshot recognises this command's own temporary files, in both the current
// spelling (almanack-<stamp>.db.<pid>.tmp) and the one that carried no PID.
func isPartialSnapshot(name string) bool {
	return strings.HasPrefix(name, "almanack-") && strings.HasSuffix(name, ".tmp")
}

func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	return nil
}

// fsyncDir makes the rename itself durable: without it a crash can leave the
// directory entry unwritten even though the file's contents reached the disk.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}
