package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/afterlune/luneCore/internal/sandbox"
	publictool "github.com/afterlune/luneCore/tool"
)

const sandboxHelperName = "lune-sandbox"

// SandboxedCommandExecutor delegates command execution to the companion helper
// which installs the platform's kernel-enforced policy before starting a shell.
type SandboxedCommandExecutor struct {
	HelperPath string
}

func (e SandboxedCommandExecutor) Execute(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
	if ctx == nil {
		return publictool.CommandResult{}, errors.New("sandbox command context is required")
	}
	if request.Command == "" || request.Workspace == "" {
		return publictool.CommandResult{}, errors.New("sandbox command and workspace are required")
	}
	helper, err := e.helperPath()
	if err != nil {
		return publictool.CommandResult{}, err
	}
	tempDir, err := os.MkdirTemp("", "lune-sandbox-")
	if err != nil {
		return publictool.CommandResult{}, fmt.Errorf("create sandbox temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	request.Sandbox.TempDir = tempDir
	encoded, err := json.Marshal(sandbox.HelperRequest{ProtocolVersion: sandbox.HelperProtocolVersion, CommandRequest: request})
	if err != nil {
		return publictool.CommandResult{}, fmt.Errorf("encode sandbox request: %w", err)
	}
	executionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(executionCtx, helper, "run")
	configureCommandProcess(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		return publictool.CommandResult{}, fmt.Errorf("open sandbox helper input: %w", err)
	}
	defer stdin.Close()
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		// EOF tells the helper to stop its AppContainer child and restore ACLs
		// before it exits. Killing the helper tree directly could skip cleanup.
		command.Cancel = func() error {
			err := stdin.Close()
			if errors.Is(err, os.ErrClosed) {
				return os.ErrProcessDone
			}
			return err
		}
		command.WaitDelay = 30 * time.Second
	}
	command.Env = sandboxHelperEnvironment()
	output := &limitedBuffer{limit: maxCommandOutput}
	writer := &commandOutputWriter{buffer: output, callback: request.OnOutput, cancel: cancel}
	command.Stdout, command.Stderr = writer, writer
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return publictool.CommandResult{}, fmt.Errorf("start sandbox helper: %w", err)
	}
	if _, err := stdin.Write(append(encoded, '\n')); err != nil {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		return publictool.CommandResult{}, fmt.Errorf("send sandbox request: %w", err)
	}
	if err := command.Wait(); err != nil {
		text, truncated := output.String()
		if truncated {
			text += "\n[command output truncated at 1 MiB]"
		}
		if callbackErr := writer.Error(); callbackErr != nil {
			return publictool.CommandResult{Output: text, ExitCode: commandExitCode(command)}, fmt.Errorf("capture command output: %w", callbackErr)
		}
		if ctx.Err() != nil {
			return publictool.CommandResult{Output: text}, fmt.Errorf("sandbox command stopped: %w", ctx.Err())
		}
		return publictool.CommandResult{Output: text, ExitCode: commandExitCode(command)}, fmt.Errorf("sandbox command failed: %w", err)
	}
	text, truncated := output.String()
	if truncated {
		text += "\n[command output truncated at 1 MiB]"
	}
	return publictool.CommandResult{Output: text, ExitCode: commandExitCode(command)}, nil
}

func (e SandboxedCommandExecutor) helperPath() (string, error) {
	if path := strings.TrimSpace(e.HelperPath); path != "" {
		resolved, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve sandbox helper: %w", err)
		}
		if info, statErr := os.Stat(resolved); statErr != nil || info.IsDir() {
			return "", fmt.Errorf("sandbox helper %q is unavailable", resolved)
		}
		return resolved, nil
	}
	name := sandboxHelperName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if executable, executableErr := os.Executable(); executableErr == nil {
		sibling := filepath.Join(filepath.Dir(executable), name)
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New("kernel sandbox helper lune-sandbox is unavailable; command execution fails closed")
	}
	return path, nil
}

func sandboxHelperEnvironment() []string {
	allowed := []string{
		"PATH", "SystemRoot", "WINDIR", "ComSpec", "PATHEXT", "TEMP", "TMP",
		"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
		"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)",
	}
	var environment []string
	seen := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if value, exists := os.LookupEnv(name); exists {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func commandExitCode(command *exec.Cmd) *int {
	if command.ProcessState == nil {
		return nil
	}
	code := command.ProcessState.ExitCode()
	return &code
}

type commandOutputWriter struct {
	mu       sync.Mutex
	buffer   *limitedBuffer
	callback func([]byte) error
	err      error
	cancel   context.CancelFunc
}

func (w *commandOutputWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.callback != nil {
		if err := w.callback(data); err != nil {
			w.err = err
			w.cancel()
			return 0, err
		}
	}
	return w.buffer.Write(data)
}

func (w *commandOutputWriter) Error() error { w.mu.Lock(); defer w.mu.Unlock(); return w.err }
