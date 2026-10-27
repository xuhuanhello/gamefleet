package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// DailyBackups is how many daily backups DailyBackup keeps.
const DailyBackups = 14

const dailyPrefix = "gamefleet-daily-"

// DailyBackup copies the database at path into dir as
// gamefleet-daily-<time>.db, checks the copy, and deletes all but the newest
// DailyBackups daily backups. It reads through a connection of its own, in
// another process from GameFleet's: a copy of a large database does not hold
// up GameFleet, which keeps its single connection for requests. The copy is
// written under a hidden name and renamed whole.
func DailyBackup(ctx context.Context, path, dir string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	name := dailyPrefix + now.UTC().Format("20060102-150405") + ".db"
	tmp := filepath.Join(dir, "."+name)
	final := filepath.Join(dir, name)
	os.Remove(tmp)
	if err := snapshot(ctx, path, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	// It holds every cluster's token: for GameFleet's user only.
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", err
	}
	pruneDaily(dir)
	return final, nil
}

// snapshot writes a consistent copy of the database at path to dest, and
// checks it.
func snapshot(ctx context.Context, path, dest string) error {
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("copy the database: %w", err)
	}
	copied, err := sql.Open("sqlite", "file:"+(&url.URL{Path: dest}).EscapedPath()+"?mode=ro")
	if err != nil {
		return err
	}
	defer copied.Close()
	var result string
	if err := copied.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("check the copy: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("the copy is damaged: %s", result)
	}
	return nil
}

func dailyNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), dailyPrefix) && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	// The time in the name sorts as text.
	slices.Sort(names)
	return names
}

func pruneDaily(dir string) {
	names := dailyNames(dir)
	for len(names) > DailyBackups {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// OffsiteMarker is the file in the backups directory that the job copying
// them off the machine touches once it succeeded; see docs/install.md.
const OffsiteMarker = "offsite-ok"

// LastOffsite returns when the job copying the backups off the machine last
// succeeded, zero when it never did.
func LastOffsite(dir string) (time.Time, error) {
	info, err := os.Stat(filepath.Join(dir, OffsiteMarker))
	if os.IsNotExist(err) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// NewestDailyBackup returns the newest daily backup in dir and when it was
// written, or "" when there is none.
func NewestDailyBackup(dir string) (string, time.Time, error) {
	names := dailyNames(dir)
	if len(names) == 0 {
		return "", time.Time{}, nil
	}
	path := filepath.Join(dir, names[len(names)-1])
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, err
	}
	return path, info.ModTime(), nil
}
