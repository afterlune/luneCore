package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	publictool "github.com/afterlune/luneCore/tool"
)

// DirectCommandExecutor executes commands directly via the host shell without kernel sandboxing.
type DirectCommandExecutor struct{}

func (DirectCommandExecutor) Execute(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
	if ctx == nil {
		return publictool.CommandResult{}, errors.New("command context is required")
	}
	if strings.TrimSpace(request.Command) == "" || strings.TrimSpace(request.Workspace) == "" {
		return publictool.CommandResult{}, errors.New("command and workspace are required")
	}

	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.CommandContext(ctx, "cmd.exe", "/c", request.Command)
	} else {
		command = exec.CommandContext(ctx, "/bin/sh", "-c", request.Command)
	}

	command.Dir = request.Workspace
	configureCommandProcess(command)

	output := &limitedBuffer{limit: maxCommandOutput}
	writer := &commandOutputWriter{buffer: output, callback: request.OnOutput}
	command.Stdout, command.Stderr = writer, writer

	if err := command.Start(); err != nil {
		return publictool.CommandResult{}, fmt.Errorf("start command: %w", err)
	}

	err := command.Wait()
	text, truncated := output.String()
	if truncated {
		text += "\n[command output truncated at 1 MiB]"
	}

	if callbackErr := writer.Error(); callbackErr != nil {
		return publictool.CommandResult{Output: text, ExitCode: commandExitCode(command)}, fmt.Errorf("capture command output: %w", callbackErr)
	}
	if ctx.Err() != nil {
		return publictool.CommandResult{Output: text}, fmt.Errorf("command stopped: %w", ctx.Err())
	}
	if err != nil {
		return publictool.CommandResult{Output: text, ExitCode: commandExitCode(command)}, fmt.Errorf("command failed: %w", err)
	}

	exitCode := 0
	return publictool.CommandResult{Output: text, ExitCode: &exitCode}, nil
}

func IsSandboxDisabled(allowLocalCommand bool) bool {
	if allowLocalCommand {
		return true
	}
	val := strings.ToLower(strings.TrimSpace(os.Getenv("LUNE_SANDBOX")))
	if val == "off" || val == "0" || val == "false" || val == "no" || val == "disable" || val == "disabled" {
		return true
	}
	noSandbox := strings.ToLower(strings.TrimSpace(os.Getenv("LUNE_NO_SANDBOX")))
	return noSandbox == "1" || noSandbox == "true" || noSandbox == "yes" || noSandbox == "on"
}
