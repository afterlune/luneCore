// Package stateformat isolates execution upgrades from normal session execution.
package stateformat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/afterlune/luneCore/internal/session"
)

const Version = "2"
const CheckpointFile = "executions-v2.db"
const markerFile = "execution-format"
const lockFile = ".state.lock"

var ErrUpgradeRequired = errors.New("execution storage upgrade required; stop all Lune instances and run lune migrate --state-dir <directory>")

type Report struct {
	BackupDirectory string
	DiscardedTasks  int
	AlreadyCurrent  bool
}

// Open holds a shared lifecycle lock until Core has closed all of its stores.
func Open(dir string) (*session.FileLock, error) {
	lock, err := session.AcquireSharedFileLock(filepath.Join(dir, lockFile))
	if err != nil {
		return nil, err
	}
	if err := check(dir); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func check(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, markerFile))
	if err == nil {
		if strings.TrimSpace(string(data)) != Version {
			return ErrUpgradeRequired
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, name := range []string{"sessions.db", "checkpoints-v10.db"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Size() > 0 {
			return ErrUpgradeRequired
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return writeMarker(dir, false)
}

func writeMarker(dir string, replace bool) error {
	file, err := os.CreateTemp(dir, ".execution-format-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.WriteString(Version + "\n"); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	target := filepath.Join(dir, markerFile)
	// The upgrader owns the exclusive state lock; absence after interruption is
	// deliberately treated as upgrade-required, so replacement is retry-safe.
	if replace {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(name, target); err != nil {
		// Concurrent fresh opens may install the same marker first. On Windows
		// rename does not replace that file; never remove it under a shared lock.
		if !replace {
			if data, readErr := os.ReadFile(target); readErr == nil && strings.TrimSpace(string(data)) == Version {
				return nil
			}
		}
		return err
	}
	return nil
}

// Upgrade never resumes old work. It backs up the session database before any
// migration, retains legacy checkpoint files, and only installs the marker last.
func Upgrade(ctx context.Context, dir string, transform func(json.RawMessage) (json.RawMessage, error)) (Report, error) {
	if ctx == nil || transform == nil {
		return Report{}, errors.New("context and session transformation are required")
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Report{}, err
	}
	if _, err := os.Stat(dir); err != nil {
		return Report{}, err
	}
	lock, err := session.AcquireFileLock(filepath.Join(dir, lockFile))
	if err != nil {
		return Report{}, fmt.Errorf("stop all Core instances before upgrade: %w", err)
	}
	defer lock.Close()
	if data, err := os.ReadFile(filepath.Join(dir, markerFile)); err == nil {
		version := strings.TrimSpace(string(data))
		if version == Version {
			return Report{AlreadyCurrent: true}, nil
		}
		if version != "1" {
			return Report{}, fmt.Errorf("unsupported execution format %q; refusing downgrade", version)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Report{}, err
	}
	path := filepath.Join(dir, "sessions.db")
	if _, err := os.Stat(path); err != nil {
		return Report{}, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return Report{}, err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	var hasSessions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&hasSessions); err != nil {
		return Report{}, err
	}
	var locks []*session.FileLock
	defer func() {
		for _, l := range locks {
			_ = l.Close()
		}
	}()
	if hasSessions > 0 {
		rows, err := db.QueryContext(ctx, `SELECT id FROM sessions`)
		if err != nil {
			return Report{}, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return Report{}, err
			}
			ids = append(ids, id)
		}
		rowErr := errors.Join(rows.Err(), rows.Close())
		if rowErr != nil {
			return Report{}, rowErr
		}
		for _, id := range ids {
			l, err := session.AcquireSessionLock(dir, id)
			if err != nil {
				return Report{}, err
			}
			locks = append(locks, l)
		}
	}
	backup, err := os.MkdirTemp(dir, "upgrade-backup-")
	if err != nil {
		return Report{}, err
	}
	backupPath := filepath.Join(backup, "sessions.db")
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, backupPath); err != nil {
		return Report{}, fmt.Errorf("back up session storage: %w", err)
	}
	if err := os.Chmod(backupPath, 0600); err != nil {
		return Report{}, err
	}
	if err := db.Close(); err != nil {
		return Report{}, err
	}
	store, err := session.Open(path)
	if err != nil {
		return Report{BackupDirectory: backup}, err
	}
	defer store.Close()
	discarded, err := store.UpgradeExecutions(ctx, transform)
	report := Report{BackupDirectory: backup, DiscardedTasks: discarded}
	if err != nil {
		return report, err
	}
	if err := store.Close(); err != nil {
		return report, err
	}
	if err := writeMarker(dir, true); err != nil {
		return report, err
	}
	return report, nil
}
