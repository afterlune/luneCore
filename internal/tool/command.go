package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const (
	commandSchema    = `{"type":"object","properties":{"command":{"type":"string","description":"Shell command to run in the workspace"}},"required":["command"],"additionalProperties":false}`
	maxCommandOutput = 1 << 20
)

// CommandTool executes a command in the workspace. Shell commands can still
// access files outside the workspace; Workspace only sets their working dir.
type CommandTool struct {
	Workspace         string
	Timeout           time.Duration
	Executor          publictool.CommandExecutor
	AllowLocalCommand bool
}

func (CommandTool) Definition() model.FunctionTool {
	return functionDefinition("command", "Run a shell command in the workspace and return its stdout and stderr.", commandSchema)
}

func (t CommandTool) Run(ctx context.Context, raw json.RawMessage) Result {
	var args struct {
		Command string `json:"command"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid command arguments: %w", err)}
	}
	if strings.TrimSpace(args.Command) == "" {
		return Result{Err: errors.New("command must not be empty")}
	}
	workspace, err := canonicalWorkspace(t.Workspace)
	if err != nil {
		return Result{Err: err}
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if t.Executor != nil {
		invocationID := publictool.InvocationID(ctx)
		requestContext := publictool.WithInvocationID(commandCtx, invocationID)
		result, err := t.Executor.Execute(requestContext, publictool.CommandRequest{
			Command: args.Command, Workspace: workspace, InvocationID: invocationID,
		})
		if commandCtx.Err() != nil {
			return Result{Output: result.Output, Err: fmt.Errorf("command stopped: %w", commandCtx.Err())}
		}
		if err != nil {
			return Result{Output: result.Output, Err: fmt.Errorf("run command: %w", err)}
		}
		return Result{Output: result.Output}
	}
	if !t.AllowLocalCommand {
		return Result{Err: errors.New("no command executor is configured; inject a host executor or explicitly enable local shell execution")}
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(commandCtx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", args.Command)
	} else {
		cmd = exec.CommandContext(commandCtx, "/bin/sh", "-c", args.Command)
	}
	configureCommandProcess(cmd)
	cmd.Dir = workspace

	output := &limitedBuffer{limit: maxCommandOutput}
	cmd.Stdout = output
	cmd.Stderr = output
	runErr := cmd.Run()
	text, truncated := output.String()
	if truncated {
		text += "\n[command output truncated at 1 MiB]"
	}
	if commandCtx.Err() != nil {
		return Result{Output: text, Err: fmt.Errorf("command stopped: %w", commandCtx.Err())}
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return Result{Output: text, Err: fmt.Errorf("command exited with code %d", exitErr.ExitCode())}
		}
		return Result{Output: text, Err: fmt.Errorf("run command: %w", runErr)}
	}
	return Result{Output: text}
}

// limitedBuffer captures command output up to a fixed limit.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLength := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return originalLength, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return originalLength, nil
}

func (b *limitedBuffer) String() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.truncated
}
