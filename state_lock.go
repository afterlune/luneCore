package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const stateDirectoryLockName = ".lune-core.lock"

type stateDirectoryLock struct {
	file *os.File
}

func acquireStateDirectoryLock(stateDir string) (*stateDirectoryLock, error) {
	path := filepath.Join(stateDir, stateDirectoryLockName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", stateDirectoryLockName, err)
	}
	if err := lockStateDirectoryFile(file); err != nil {
		closeErr := file.Close()
		return nil, errors.Join(err, closeErr)
	}
	return &stateDirectoryLock{file: file}, nil
}

func (lock *stateDirectoryLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unlockStateDirectoryFile(lock.file)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
