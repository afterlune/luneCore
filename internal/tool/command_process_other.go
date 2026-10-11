//go:build !unix && !windows

package tool

import (
	"os/exec"
	"time"
)

func configureCommandProcess(command *exec.Cmd) {
	command.WaitDelay = time.Second
}
