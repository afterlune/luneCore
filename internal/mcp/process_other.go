//go:build !unix && !windows

package mcp

import (
	"os"
	"os/exec"
	"time"
)

func configureMCPProcess(command *exec.Cmd) { command.WaitDelay = time.Second }

func terminateMCPProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	return command.Process.Kill()
}
