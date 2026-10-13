package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const stateDirectoryLockName = ".lune-core.lock"

// ErrStateDirInUse indicates that another Core instance already owns the state directory.
var ErrStateDirInUse = errors.New("state directory is already in use by another Core instance")

type DirectoryLock struct {
	file *os.File
}

func AcquireStateDirectoryLock(stateDir string) (*DirectoryLock, error) {
	path := filepath.Join(stateDir, stateDirectoryLockName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", stateDirectoryLockName, err)
	}
	if err := lockStateDirectoryFile(file); err != nil {
		closeErr := file.Close()
		return nil, errors.Join(err, closeErr)
	}
	return &DirectoryLock{file: file}, nil
}

func (lock *DirectoryLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unlockStateDirectoryFile(lock.file)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
