package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrSessionInUse indicates that another Core instance owns a session.
var ErrSessionInUse = errors.New("session is already in use by another Core instance")
var ErrFileLockInUse = errors.New("file is already locked")

// FileLock owns one file lock across processes.
type FileLock struct{ file *os.File }

// AcquireSessionLock takes an exclusive process lock for sessionID.
func AcquireSessionLock(stateDir, sessionID string) (*FileLock, error) {
	if stateDir == "" || sessionID == "" {
		return nil, errors.New("state directory and session id are required")
	}
	dir := filepath.Join(stateDir, ".session-locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create session lock directory: %w", err)
	}
	name := sha256.Sum256([]byte(sessionID))
	path := filepath.Join(dir, hex.EncodeToString(name[:])+".lock")
	lock, err := AcquireFileLock(path)
	if errors.Is(err, ErrFileLockInUse) {
		return nil, fmt.Errorf("%w: %s", ErrSessionInUse, sessionID)
	}
	if err != nil {
		return nil, err
	}
	return lock, nil
}

// AcquireFileLock takes an exclusive lock on path. The lock is released when
// Close is called or when the operating system closes the underlying handle.
func AcquireFileLock(path string) (*FileLock, error) { return acquireFileLock(path, false) }

// AcquireSharedFileLock allows live Core instances to coexist while excluding offline upgrades.
func AcquireSharedFileLock(path string) (*FileLock, error) { return acquireFileLock(path, true) }

func acquireFileLock(path string, shared bool) (*FileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockSessionFile(file, shared); err != nil {
		closeErr := file.Close()
		if errors.Is(err, ErrFileLockInUse) {
			return nil, errors.Join(ErrFileLockInUse, closeErr)
		}
		return nil, errors.Join(err, closeErr)
	}
	return &FileLock{file: file}, nil
}

// AcquireFileLockWithTimeout retries process lock contention until timeout.
func AcquireFileLockWithTimeout(path string, timeout time.Duration) (*FileLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		lock, err := AcquireFileLock(path)
		if !errors.Is(err, ErrFileLockInUse) {
			return lock, err
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return nil, err
		}
		remaining := time.Until(deadline)
		pause := 10 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		time.Sleep(pause)
	}
}

// Close releases the file lock. It is safe to call more than once.
func (lock *FileLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unlockSessionFile(lock.file)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
