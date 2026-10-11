//go:build windows

package core

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockStateDirectoryFile(file *os.File) error {
	var overlapped windows.Overlapped
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return fmt.Errorf("%w: %v", ErrStateDirInUse, err)
	}
	if err != nil {
		return fmt.Errorf("lock state directory file: %w", err)
	}
	return nil
}

func unlockStateDirectoryFile(file *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("unlock state directory file: %w", err)
	}
	return nil
}
