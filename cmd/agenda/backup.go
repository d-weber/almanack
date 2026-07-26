package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agenda/internal/config"
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

const backupTimeLayout = "20060102-1504"

type backupResult struct {
	Path    string
	Bytes   int64
	Elapsed time.Duration
	Pruned  int
}

func runBackup(ctx context.Context, cfg config.Config, dir string, prune bool) (backupResult, error) {
	start := time.Now()
	if dir == "" {
		dir = cfg.BackupDir
	}
	if dir == "" {
		return backupResult{}, fmt.Errorf("no backup directory: pass one as an argument or set AGENDA_BACKUP_DIR")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return backupResult{}, fmt.Errorf("create backup directory %s: %w", dir, err)
	}
	if err := removeStaleTemps(dir); err != nil {
		return backupResult{}, err
	}

	stamp := time.Now().UTC().Format(backupTimeLayout)
	final := filepath.Join(dir, "agenda-"+stamp+".db")
	tmp := final + ".tmp"
	// VACUUM INTO refuses to overwrite, and a same-minute rerun would otherwise fail
	// on the leftover from the previous attempt.
	_ = os.Remove(tmp)

	src, err := sql.Open("sqlite", "file:"+cfg.DataPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return backupResult{}, fmt.Errorf("open database: %w", err)
	}
	defer src.Close()

	if _, err := src.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		_ = os.Remove(tmp)
		return backupResult{}, fmt.Errorf("snapshot to %s: %w", tmp, err)
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
		n, err := pruneBackups(dir, cfg)
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
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
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
func pruneBackups(dir string, cfg config.Config) (int, error) {
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
		if e.IsDir() || !strings.HasPrefix(e.Name(), "agenda-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "agenda-"), ".db")
		when, err := time.Parse(backupTimeLayout, stamp)
		if err != nil {
			continue // not ours; leave it alone
		}
		snaps = append(snaps, snap{name: e.Name(), when: when})
	}
	// Newest first, so the first snapshot seen in each bucket is the one kept.
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].when.After(snaps[j].when) })

	keep := map[string]bool{}
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

// removeStaleTemps clears partial files left by an interrupted run, so they can never
// be mistaken for a snapshot or shipped off-host by a sync job.
func removeStaleTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".db.tmp") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return fmt.Errorf("remove stale partial %s: %w", e.Name(), err)
			}
		}
	}
	return nil
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
