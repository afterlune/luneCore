//go:build unix

package core

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func lockStateDirectoryFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("%w: %v", ErrStateDirInUse, err)
	}
	if err != nil {
		return fmt.Errorf("lock state directory file: %w", err)
	}
	return nil
}

func unlockStateDirectoryFile(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("unlock state directory file: %w", err)
	}
	return nil
}
