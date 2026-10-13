//go:build linux

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
