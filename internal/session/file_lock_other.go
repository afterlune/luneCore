//go:build !unix && !windows

package session

import (
	"errors"
	"os"
)

func lockSessionFile(*os.File, bool) error {
	return errors.New("session locking is unsupported on this operating system")
}

func unlockSessionFile(*os.File) error { return nil }
