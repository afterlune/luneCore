//go:build windows

package tool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func configureCommandProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	command.WaitDelay = time.Second
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		systemRoot := os.Getenv("SystemRoot")
		if systemRoot != "" {
			taskkillPath := filepath.Join(systemRoot, "System32", "taskkill.exe")
			if err := exec.Command(taskkillPath, "/F", "/T", "/PID", strconv.Itoa(command.Process.Pid)).Run(); err == nil {
				return nil
			} else {
				if killErr := command.Process.Kill(); killErr == nil {
					return nil
				} else if errors.Is(killErr, os.ErrProcessDone) {
					return os.ErrProcessDone
				} else {
					return errors.Join(fmt.Errorf("kill command process tree: %w", err), killErr)
				}
			}
		}
		err := command.Process.Kill()
		if err == nil {
			return nil
		}
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return fmt.Errorf("stop command process tree: %w", err)
	}
}
