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
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/sandbox"
	publictool "github.com/afterlune/luneCore/tool"
)

type commandExecutorFunc func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error)

func (f commandExecutorFunc) Execute(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
	return f(ctx, request)
}

func TestSandboxCommandStreamsOutputAndPreservesExitCode(t *testing.T) {
	helper := sandboxHelperForTest(t)
	command := "printf 'phase-one\\n'; sleep 0.1; printf 'phase-two\\n'; exit 7"
	if runtime.GOOS == "windows" {
		command = "Write-Output phase-one; Start-Sleep -Milliseconds 100; Write-Output phase-two; exit 7"
	}
	var mu sync.Mutex
	var chunks []string
	request := publictool.CommandRequest{Command: command, Workspace: t.TempDir(), OnOutput: func(data []byte) error { mu.Lock(); chunks = append(chunks, string(data)); mu.Unlock(); return nil }}
	request.Sandbox = publictool.SandboxPolicy{PermissionMode: "workspace", Workspace: request.Workspace}
	result, err := (SandboxedCommandExecutor{HelperPath: helper}).Execute(context.Background(), request)
	mu.Lock()
	output := strings.Join(chunks, "")
	mu.Unlock()
	if err == nil || result.ExitCode == nil || *result.ExitCode != 7 || !strings.Contains(output, "phase-one") || !strings.Contains(output, "phase-two") {
		t.Fatalf("result=%+v output=%q error=%v", result, output, err)
	}
}

func TestSandboxHelperEOFStopsCommandChildren(t *testing.T) {
	workspace := t.TempDir()
	helper := sandboxHelperForTest(t)
	command := "(printf started > started; sleep 0.4; printf finished > finished) & wait"
	if runtime.GOOS == "windows" {
		started := strings.ReplaceAll(filepath.Join(workspace, "started"), "'", "''")
		finished := strings.ReplaceAll(filepath.Join(workspace, "finished"), "'", "''")
		command = fmt.Sprintf("$job = Start-Job -ScriptBlock { Set-Content -LiteralPath '%s' started; Start-Sleep -Milliseconds 400; Set-Content -LiteralPath '%s' finished }; Wait-Job $job", started, finished)
	}
	request := publictool.CommandRequest{Command: command, Workspace: workspace, Sandbox: publictool.SandboxPolicy{PermissionMode: "workspace", Workspace: workspace, TempDir: t.TempDir()}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, helper, "run")
	captured := &limitedBuffer{limit: maxCommandOutput}
	process.Stdout, process.Stderr = captured, captured
	stdin, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(sandbox.HelperRequest{ProtocolVersion: sandbox.HelperProtocolVersion, CommandRequest: request})
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(workspace, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = stdin.Close()
			_ = process.Wait()
			text, _ := captured.String()
			t.Fatalf("sandbox child did not start: %s", text)
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = stdin.Close()
	_ = process.Wait()
	if ctx.Err() != nil {
		t.Fatal("helper did not stop after its owner disconnected")
	}
	time.Sleep(450 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(workspace, "finished")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child survived helper EOF: %v", err)
	}
}

func TestSandboxConcurrentCommandsKeepGrants(t *testing.T) {
	helper := sandboxHelperForTest(t)
	workspace := t.TempDir()
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			command := fmt.Sprintf("sleep 0.1; printf complete > marker-%d", n)
			if runtime.GOOS == "windows" {
				command = fmt.Sprintf("Start-Sleep -Milliseconds 100; Set-Content -LiteralPath marker-%d complete", n)
			}
			request := publictool.CommandRequest{Command: command, Workspace: workspace, Sandbox: publictool.SandboxPolicy{PermissionMode: "workspace", Workspace: workspace}}
			result, err := (SandboxedCommandExecutor{HelperPath: helper}).Execute(context.Background(), request)
			if err != nil || result.ExitCode == nil || *result.ExitCode != 0 {
				t.Errorf("command %d: result=%+v error=%v", n, result, err)
			}
		}(n)
	}
	wg.Wait()
	for n := 0; n < 4; n++ {
		data, err := os.ReadFile(filepath.Join(workspace, fmt.Sprintf("marker-%d", n)))
		if err != nil || !strings.Contains(string(data), "complete") {
			t.Fatalf("command %d marker=%q error=%v", n, data, err)
		}
	}
}

func TestSandboxHelperRejectsOldProtocol(t *testing.T) {
	helper := sandboxHelperForTest(t)
	process := exec.Command(helper, "run")
	process.Stdin = strings.NewReader(`{"Command":"echo must-not-run","Workspace":"."}` + "\n")
	output, err := process.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "unsupported sandbox helper protocol") {
		t.Fatalf("old protocol output=%q error=%v", output, err)
	}
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

func TestCommandToolRejectsUnmanagedBackgroundExecution(t *testing.T) {
	calls := 0
	tool := CommandTool{Workspace: t.TempDir(), Executor: commandExecutorFunc(func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error) {
		calls++
		return publictool.CommandResult{}, nil
	})}
	result := tool.Run(context.Background(), json.RawMessage(`{"command":"server","background":true}`))
	if result.Err == nil || !strings.Contains(result.Err.Error(), "managed background commands require") {
		t.Fatalf("background result = %#v, want managed-runtime error", result)
	}
	if calls != 0 {
		t.Fatalf("unmanaged executor ran %d commands", calls)
	}
}

func TestCommandToolFailsClosedWithoutSandboxHelper(t *testing.T) {
	result := (CommandTool{Workspace: t.TempDir(), SandboxHelperPath: filepath.Join(t.TempDir(), "missing-helper")}).Run(context.Background(), json.RawMessage(`{"command":"echo test"}`))
	if result.Err == nil || !strings.Contains(result.Err.Error(), "sandbox helper") {
		t.Fatalf("command result = %#v, want missing sandbox helper error", result)
	}
}

func TestCommandToolHasNoHostToolchainGrantByDefault(t *testing.T) {
	var received publictool.CommandRequest
	executor := commandExecutorFunc(func(_ context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
		received = request
		return publictool.CommandResult{Output: "ok"}, nil
	})
	result := (CommandTool{Workspace: t.TempDir(), Executor: executor}).Run(context.Background(), json.RawMessage(`{"command":"echo ok"}`))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if len(received.Sandbox.Grants) != 0 || len(received.Sandbox.CommandToolchainPaths) != 0 {
		t.Fatalf("default command grants = %#v, toolchain paths = %#v", received.Sandbox.Grants, received.Sandbox.CommandToolchainPaths)
	}
}

func TestCommandToolPassesPlanGrantsToSandboxExecutor(t *testing.T) {
	workspace := t.TempDir()
	extraPath := t.TempDir()
	toolchainPath := t.TempDir()
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
	commandTool := CommandTool{Workspace: workspace, Executor: executor, CommandToolchainPaths: []string{toolchainPath}}
	result := commandTool.Run(ctx, json.RawMessage(`{"command":"echo ok"}`))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if received.Sandbox.PermissionMode != string(PermissionModeWorkspace) || received.Sandbox.Workspace != workspace || !received.Sandbox.Network {
		t.Fatalf("sandbox policy = %#v", received.Sandbox)
	}
	if len(received.Sandbox.Grants) != 2 || received.Sandbox.Grants[0] != authorization.Paths[0] || received.Sandbox.Grants[1] != (publictool.PathGrant{Path: toolchainPath, Access: publictool.PathRead}) {
		t.Fatalf("sandbox grants = %#v", received.Sandbox.Grants)
	}
	if len(received.Sandbox.CommandToolchainPaths) != 1 || received.Sandbox.CommandToolchainPaths[0] != toolchainPath {
		t.Fatalf("sandbox toolchain paths = %#v", received.Sandbox.CommandToolchainPaths)
	}
	ask := WithPermissionMode(context.Background(), string(PermissionModeAuto))
	result = commandTool.Run(ask, json.RawMessage(`{"command":"echo ok"}`))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if received.Sandbox.PermissionMode != string(PermissionModeWorkspace) {
		t.Fatalf("auto mode sandbox = %q, want workspace after approval", received.Sandbox.PermissionMode)
	}
	fullAccess := WithPermissionMode(context.Background(), string(PermissionModeFullAccess))
	result = commandTool.Run(fullAccess, json.RawMessage(`{"command":"echo ok"}`))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if received.Sandbox.PermissionMode != string(PermissionModeFullAccess) || received.Sandbox.HomeDir == "" || !received.Sandbox.Network {
		t.Fatalf("full-access sandbox policy = %#v", received.Sandbox)
	}
}

func TestApprovedAskModeCommandUsesWorkspaceSandbox(t *testing.T) {
	var received publictool.CommandRequest
	executor := commandExecutorFunc(func(_ context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
		received = request
		return publictool.CommandResult{Output: "ok"}, nil
	})
	registered := CommandTool{Workspace: t.TempDir(), Executor: executor}
	registry, err := NewRegistry(ToolRegistration{
		Tool:     registered,
		Metadata: model.ToolMetadata{PermissionClass: model.ToolPermissionCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeAuto); err != nil {
		t.Fatal(err)
	}
	approvals := 0
	registry.SetPermissionApprovalHandler(func(context.Context, PermissionRequest) bool {
		approvals++
		return true
	})
	result := registry.Run(context.Background(), "command", json.RawMessage(`{"command":"echo ok"}`))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if approvals != 1 {
		t.Fatalf("ask-mode command approvals = %d, want one", approvals)
	}
	if received.Sandbox.PermissionMode != string(PermissionModeWorkspace) {
		t.Fatalf("approved ask-mode sandbox = %q, want workspace", received.Sandbox.PermissionMode)
	}
}

func TestHighRiskAndOpaqueCommandsNeedExactPlanApproval(t *testing.T) {
	commands := []string{
		"rm -rf ./build",
		"find . -delete",
		"rsync --delete source/ target/",
		"erase /s /q .\\*",
		"ErAsE /q /S *.log",
		`cmd.exe /c erase /s /q .\*`,
		`C:\Windows\System32\cmd.exe /k "erase /s /q .\*"`,
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
			planContext := publictool.WithNoInteractiveDecisions(context.Background(), true)
			result := tool.Run(planContext, arguments)
			var blocker *publictool.ExecutionBlockerError
			if !errors.As(result.Err, &blocker) {
				t.Fatalf("unapproved command error = %v, want an execution blocker", result.Err)
			}
			if calls != 0 {
				t.Fatalf("unapproved command reached executor %d times", calls)
			}

			ctx := publictool.WithPlanAuthorization(planContext, publictool.PlanAuthorization{
				ApprovedHighRiskCommands: []string{command},
			})
			result = tool.Run(ctx, arguments)
			if result.Err != nil || calls != 1 {
				t.Fatalf("approved command result = %#v, executor calls = %d", result, calls)
			}

			fullAccessCtx := WithPermissionMode(context.Background(), string(PermissionModeFullAccess))
			result = tool.Run(fullAccessCtx, arguments)
			if result.Err != nil || calls != 2 {
				t.Fatalf("full-access command result = %#v, executor calls = %d", result, calls)
			}
		})
	}
}

func TestExtremeDangerCommandsAreAlwaysBlocked(t *testing.T) {
	commands := []string{
		"mkfs.ext4 /dev/sdb", "diskpart clean", "Set-MpPreference -DisableRealtimeMonitoring $true",
		"iptables -F", `bcdedit /delete {current}`, "dd if=/dev/zero of=/dev/sdb", "format C:",
		"efibootmgr -B", "systemctl disable firewalld", "sudo id", "su -", "rm -rf /", "rm -rf /etc", `Remove-Item -Recurse C:\Windows`,
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			calls := 0
			tool := CommandTool{Workspace: t.TempDir(), Executor: commandExecutorFunc(func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error) {
				calls++
				return publictool.CommandResult{Output: "ran"}, nil
			})}
			arguments, _ := json.Marshal(struct {
				Command string `json:"command"`
			}{Command: command})
			contexts := map[string]context.Context{
				"ordinary":    context.Background(),
				"full-access": WithPermissionMode(context.Background(), string(PermissionModeFullAccess)),
				"exact plan":  publictool.WithPlanAuthorization(publictool.WithNoInteractiveDecisions(context.Background(), true), publictool.PlanAuthorization{ApprovedHighRiskCommands: []string{command}}),
			}
			for name, ctx := range contexts {
				if result := tool.Run(ctx, arguments); result.Err == nil {
					t.Errorf("%s mode allowed extreme command", name)
				}
			}
			if calls != 0 {
				t.Fatalf("extreme command reached executor %d times", calls)
			}
		})
	}
}

func TestInteractiveApprovalAllowsHighRiskButRejectsExtremeCommandBeforePrompt(t *testing.T) {
	calls, approvals := 0, 0
	registered := CommandTool{Workspace: t.TempDir(), Executor: commandExecutorFunc(func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error) {
		calls++
		return publictool.CommandResult{Output: "ran"}, nil
	})}
	registry, err := NewRegistry(ToolRegistration{Tool: registered, Metadata: model.ToolMetadata{PermissionClass: model.ToolPermissionCommand}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeWorkspace); err != nil {
		t.Fatal(err)
	}
	registry.SetPermissionApprovalHandler(func(context.Context, PermissionRequest) bool {
		approvals++
		return true
	})
	if result := registry.Run(context.Background(), "command", json.RawMessage(`{"command":"rm -rf ./build"}`)); result.Err != nil {
		t.Fatalf("approved high-risk command: %v", result.Err)
	}
	if calls != 1 || approvals != 1 {
		t.Fatalf("high-risk calls = %d, approvals = %d; want one each", calls, approvals)
	}
	if result := registry.Run(context.Background(), "command", json.RawMessage(`{"command":"mkfs.ext4 /dev/sdb"}`)); result.Err == nil {
		t.Fatal("extreme command was allowed")
	}
	if calls != 1 || approvals != 1 {
		t.Fatalf("extreme command reached execution or approval: calls=%d approvals=%d", calls, approvals)
	}
}

func sandboxHelperForTest(t *testing.T) string {
	t.Helper()
	if helper := os.Getenv("LUNE_SANDBOX_TEST_HELPER"); helper != "" {
		return helper
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate command test working directory: %v", err)
	}
	moduleRoot := ""
	for directory := filepath.Clean(workingDirectory); ; directory = filepath.Dir(directory) {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			moduleRoot = directory
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	if moduleRoot == "" {
		t.Fatalf("could not find luneCore go.mod above %q", workingDirectory)
	}
	name := "lune-sandbox"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	command := exec.Command("go", "build", "-trimpath", "-o", path, "./cmd/lune-sandbox")
	command.Dir = moduleRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build sandbox helper: %v\n%s", err, output)
	}
	return path
}
