//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockSessionFile(file *os.File) error {
	var overlapped windows.Overlapped
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return fmt.Errorf("%w: %v", ErrFileLockInUse, err)
	}
	if err != nil {
		return fmt.Errorf("lock session file: %w", err)
	}
	return nil
}

func unlockSessionFile(file *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("unlock session file: %w", err)
	}
	return nil
}
