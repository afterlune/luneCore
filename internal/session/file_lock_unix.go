//go:build unix

package session

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func lockSessionFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("%w: %v", ErrFileLockInUse, err)
	}
	if err != nil {
		return fmt.Errorf("lock session file: %w", err)
	}
	return nil
}

func unlockSessionFile(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("unlock session file: %w", err)
	}
	return nil
}
