//go:build !unix && !windows

package session

import (
	"errors"
	"os"
)

func lockStateDirectoryFile(*os.File) error {
	return errors.New("state directory locking is unsupported on this operating system")
}

func unlockStateDirectoryFile(*os.File) error { return nil }
