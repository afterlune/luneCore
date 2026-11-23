//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// Go registers inherited Unix descriptors with its poller when they are
// already nonblocking. This lets Close interrupt a pipe read or write.
func runtimePipe(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}
