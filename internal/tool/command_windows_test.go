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
)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultChannel := make(chan Result, 1)
	go func() {
		resultChannel <- (CommandTool{Workspace: workspace, AllowLocalCommand: true}).Run(ctx, command)
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
