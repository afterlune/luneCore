//go:build windows

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

func TestSandboxAppliesPlanPathGrantsAndFullAccessProfile(t *testing.T) {
	workspace := t.TempDir()
	approvedPath := t.TempDir()
	deniedPath := t.TempDir()
	approvedFile := filepath.Join(approvedPath, "approved.txt")
	deniedFile := filepath.Join(deniedPath, "private.txt")
	if err := os.WriteFile(approvedFile, []byte("plan-approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deniedFile, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := sandboxHelperForTest(t)
	commandTool := CommandTool{Workspace: workspace, Executor: SandboxedCommandExecutor{HelperPath: helper}}
	quote := func(path string) string { return strings.ReplaceAll(path, "'", "''") }
	marshalCommand := func(command string) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(struct {
			Command string `json:"command"`
		}{Command: command})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	approvedCommand := marshalCommand("Get-Content -LiteralPath '" + quote(approvedFile) + "'")
	approvedContext := publictool.WithPlanAuthorization(context.Background(), publictool.PlanAuthorization{
		Paths: []publictool.PathGrant{{Path: approvedPath, Access: publictool.PathRead}},
	})
	result := commandTool.Run(approvedContext, approvedCommand)
	if result.Err != nil || strings.TrimSpace(result.Output) != "plan-approved" {
		t.Fatalf("approved path result = %#v", result)
	}
	deniedCommand := marshalCommand("if (Test-Path -LiteralPath '" + quote(deniedFile) + "') { 'accessible' } else { 'blocked' }")
	result = commandTool.Run(context.Background(), deniedCommand)
	if result.Err != nil || !strings.Contains(result.Output, "blocked") || strings.Contains(result.Output, "accessible") {
		t.Fatalf("unapproved path result = %#v, want blocked", result)
	}

	fullAccessContext := WithPermissionMode(context.Background(), string(PermissionModeFullAccess))
	profileCommand := json.RawMessage(`{"command":"if (Test-Path -LiteralPath $HOME) { Get-ChildItem -LiteralPath $HOME -ErrorAction Stop | Select-Object -First 1 | Out-Null; 'profile-accessible' } else { exit 8 }"}`)
	result = commandTool.Run(fullAccessContext, profileCommand)
	if result.Err != nil || strings.TrimSpace(result.Output) != "profile-accessible" {
		t.Fatalf("full-access profile result = %#v", result)
	}
}

func TestCommandCancellationStopsShellChildrenPromptly(t *testing.T) {
	workspace := t.TempDir()
	startedPath := filepath.Join(workspace, "child-started")
	finishedPath := filepath.Join(workspace, "child-finished")
	startedLiteral := strings.ReplaceAll(startedPath, "'", "''")
	finishedLiteral := strings.ReplaceAll(finishedPath, "'", "''")
	script := fmt.Sprintf("$job = Start-Job -ScriptBlock { Set-Content -LiteralPath '%s' started; Start-Sleep -Milliseconds 600; Set-Content -LiteralPath '%s' finished }; Wait-Job $job", startedLiteral, finishedLiteral)
	command, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: script})
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		select {
		case result := <-resultChannel:
			t.Fatalf("command exited before the PowerShell child started: output=%q error=%v", result.Output, result.Err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("PowerShell child did not start")
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
		if result.Err != nil && (strings.Contains(result.Err.Error(), "restore AppContainer ACL") || strings.Contains(result.Err.Error(), "delete AppContainer profile")) {
			t.Fatalf("sandbox cleanup failed after cancellation: %v", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("command cancellation did not stop the PowerShell process tree promptly")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("command cancellation took %s; child process kept the command pipe open", elapsed)
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(finishedPath); err == nil {
		t.Fatal("PowerShell child continued running and wrote its completion marker after cancellation")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check child completion marker: %v", err)
	}
}
