//go:build windows

package mcp

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

func configureMCPProcess(command *exec.Cmd) {
	command.WaitDelay = time.Second
}

func terminateMCPProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		taskkill := exec.Command(filepath.Join(systemRoot, "System32", "taskkill.exe"), "/F", "/T", "/PID", strconv.Itoa(command.Process.Pid))
		taskkill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := taskkill.Run(); err == nil {
			return nil
		} else if killErr := command.Process.Kill(); killErr == nil || errors.Is(killErr, os.ErrProcessDone) {
			return nil
		} else {
			return errors.Join(fmt.Errorf("terminate MCP process tree: %w", err), killErr)
		}
	}
	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
