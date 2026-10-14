//go:build linux

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

func TestLinuxSeccompBlocksIoUringAndProcessGroupEscape(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for Linux seccomp integration probes")
	}
	helper := sandboxHelperForTest(t)
	executor := SandboxedCommandExecutor{HelperPath: helper}
	workspace := t.TempDir()
	ioUringDenied := `python3 -c 'import ctypes,errno; libc=ctypes.CDLL(None,use_errno=True); result=libc.syscall(425,8,None); assert result == -1 and ctypes.get_errno() == errno.EPERM'`
	commands := []struct {
		name    string
		command string
		network bool
	}{
		{
			name:    "network-disabled",
			command: `python3 -c 'import socket,errno; exec("try:\n socket.socket()\nexcept OSError as error:\n assert error.errno == errno.EPERM\nelse:\n raise SystemExit(11)")' && ` + ioUringDenied,
		},
		{
			name:    "network-enabled",
			command: `python3 -c 'import socket; sock=socket.socket(); sock.close()' && ` + ioUringDenied,
			network: true,
		},
		{
			name:    "process-detach",
			command: `python3 -c 'import os,errno; exec("for fn in (os.setsid, lambda: os.setpgid(0, 0)):\n try:\n  fn()\n except OSError as error:\n  assert error.errno == errno.EPERM\n else:\n  raise SystemExit(12)")'`,
		},
	}
	for _, test := range commands {
		t.Run(test.name, func(t *testing.T) {
			request := publictool.CommandRequest{
				Command: test.command, Workspace: workspace,
				Sandbox: publictool.SandboxPolicy{PermissionMode: "workspace", Workspace: workspace, Network: test.network},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := executor.Execute(ctx, request)
			if err != nil || result.ExitCode == nil || *result.ExitCode != 0 {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestCommandCancellationStopsShellChildrenPromptly(t *testing.T) {
	workspace := t.TempDir()
	startedPath := filepath.Join(workspace, "child-started")
	finishedPath := filepath.Join(workspace, "child-finished")
	command, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: "(printf started > child-started; sleep 0.3; printf finished > child-finished) & wait"})
	if err != nil {
		t.Fatal(err)
	}
	helper := sandboxHelperForTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultChannel := make(chan Result, 1)
	go func() {
		resultChannel <- (CommandTool{Workspace: workspace, Executor: SandboxedCommandExecutor{HelperPath: helper}}).Run(ctx, command)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("shell child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	cancel()
	select {
	case result := <-resultChannel:
		if !errors.Is(result.Err, context.Canceled) {
			t.Fatalf("command error = %v, want context canceled", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("command cancellation did not stop the shell process group promptly")
	}

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("command cancellation took %s; shell child kept the command pipe open", elapsed)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(finishedPath); err == nil {
		t.Fatal("shell child continued running and wrote its completion marker after cancellation")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check child completion marker: %v", err)
	}
}
