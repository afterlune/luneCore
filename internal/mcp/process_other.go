//go:build !unix && !windows

package mcp

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func configureMCPProcess(command *exec.Cmd) { command.WaitDelay = time.Second }

func terminateMCPProcess(ctx context.Context, command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	return command.Process.Kill()
}
