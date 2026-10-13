package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	publictool "github.com/afterlune/luneCore/tool"
)

type commandExecutorFunc func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error)

func (f commandExecutorFunc) Execute(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
	return f(ctx, request)
}

func TestCommandToolDelegatesToHostExecutorWithStableInvocationID(t *testing.T) {
	workspace := t.TempDir()
	var received publictool.CommandRequest
	executor := commandExecutorFunc(func(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
		received = request
		if got := publictool.InvocationID(ctx); got != "tool_stable" {
			t.Errorf("context invocation ID = %q", got)
		}
		return publictool.CommandResult{Output: "sandbox result"}, nil
	})
	arguments := json.RawMessage(`{"command":"echo test"}`)
	ctx := publictool.WithInvocationID(context.Background(), "tool_stable")
	result := (CommandTool{Workspace: workspace, Executor: executor}).Run(ctx, arguments)
	if result.Err != nil || result.Output != "sandbox result" {
		t.Fatalf("CommandTool result = %#v", result)
	}
	if received.Command != "echo test" || received.Workspace != workspace || received.InvocationID != "tool_stable" {
		t.Fatalf("host command request = %#v", received)
	}
}

func TestCommandToolFailsClosedWithoutSandboxHelper(t *testing.T) {
	result := (CommandTool{Workspace: t.TempDir(), SandboxHelperPath: filepath.Join(t.TempDir(), "missing-helper")}).Run(context.Background(), json.RawMessage(`{"command":"echo test"}`))
	if result.Err == nil || !strings.Contains(result.Err.Error(), "sandbox helper") {
		t.Fatalf("command result = %#v, want missing sandbox helper error", result)
	}
}

func TestCommandToolPassesPlanGrantsToSandboxExecutor(t *testing.T) {
	workspace := t.TempDir()
	extraPath := t.TempDir()
	var received publictool.CommandRequest
	executor := commandExecutorFunc(func(_ context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
		received = request
		return publictool.CommandResult{Output: "ok"}, nil
	})
	authorization := publictool.PlanAuthorization{
		Paths:        []publictool.PathGrant{{Path: extraPath, Access: publictool.PathRead}},
		AllowNetwork: true,
	}
	ctx := publictool.WithPlanAuthorization(context.Background(), authorization)
	result := (CommandTool{Workspace: workspace, Executor: executor}).Run(ctx, json.RawMessage(`{"command":"echo ok"}`))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if received.Sandbox.PermissionMode != string(PermissionModeWorkspace) || received.Sandbox.Workspace != workspace || !received.Sandbox.Network {
		t.Fatalf("sandbox policy = %#v", received.Sandbox)
	}
	if len(received.Sandbox.Grants) != 1 || received.Sandbox.Grants[0] != authorization.Paths[0] {
		t.Fatalf("sandbox grants = %#v", received.Sandbox.Grants)
	}
	fullAccess := WithPermissionMode(context.Background(), string(PermissionModeFullAccess))
	result = (CommandTool{Workspace: workspace, Executor: executor}).Run(fullAccess, json.RawMessage(`{"command":"echo ok"}`))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if received.Sandbox.PermissionMode != string(PermissionModeFullAccess) || received.Sandbox.HomeDir == "" || !received.Sandbox.Network {
		t.Fatalf("full-access sandbox policy = %#v", received.Sandbox)
	}
}

func TestHighRiskAndOpaqueCommandsNeedExactPlanApproval(t *testing.T) {
	commands := []string{
		"rm -rf ./build",
		"find . -delete",
		"rsync --delete source/ target/",
		"mkfs.ext4 /dev/sdb",
		"Set-MpPreference -DisableRealtimeMonitoring $true",
		"python -c \"import os; os.remove('data')\"",
		"bash -c 'echo opaque'",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			workspace := t.TempDir()
			calls := 0
			executor := commandExecutorFunc(func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error) {
				calls++
				return publictool.CommandResult{Output: "ran"}, nil
			})
			tool := CommandTool{Workspace: workspace, Executor: executor}
			arguments, err := json.Marshal(struct {
				Command string `json:"command"`
			}{Command: command})
			if err != nil {
				t.Fatal(err)
			}
			result := tool.Run(context.Background(), arguments)
			var blocker *publictool.ExecutionBlockerError
			if !errors.As(result.Err, &blocker) {
				t.Fatalf("unapproved command error = %v, want an execution blocker", result.Err)
			}
			if calls != 0 {
				t.Fatalf("unapproved command reached executor %d times", calls)
			}

			ctx := publictool.WithPlanAuthorization(context.Background(), publictool.PlanAuthorization{
				ApprovedHighRiskCommands: []string{command},
			})
			result = tool.Run(ctx, arguments)
			if result.Err != nil || calls != 1 {
				t.Fatalf("approved command result = %#v, executor calls = %d", result, calls)
			}
		})
	}
}

func sandboxHelperForTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate command test source")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	name := "lune-sandbox"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	command := exec.Command("go", "build", "-o", path, "./cmd/lune-sandbox")
	command.Dir = moduleRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build sandbox helper: %v\n%s", err, output)
	}
	return path
}
