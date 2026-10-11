//go:build !unix && !windows

package core

import (
	"errors"
	"os"
)

func lockStateDirectoryFile(*os.File) error {
	return errors.New("state directory locking is unsupported on this operating system")
}

func unlockStateDirectoryFile(*os.File) error { return nil }
