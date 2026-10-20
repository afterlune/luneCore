package tool

import (
	"context"
	"strings"
	"testing"

	publictool "github.com/afterlune/luneCore/tool"
)

func TestDirectCommandExecutorRunsCommands(t *testing.T) {
	executor := DirectCommandExecutor{}
	dir := t.TempDir()

	res, err := executor.Execute(context.Background(), publictool.CommandRequest{
		Command:   "echo hello-direct",
		Workspace: dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Output, "hello-direct") {
		t.Fatalf("expected output to contain 'hello-direct', got %q", res.Output)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", res.ExitCode)
	}
}

func TestIsSandboxDisabled(t *testing.T) {
	if !IsSandboxDisabled(true) {
		t.Errorf("expected IsSandboxDisabled(true) to be true")
	}

	t.Setenv("LUNE_SANDBOX", "off")
	if !IsSandboxDisabled(false) {
		t.Errorf("expected LUNE_SANDBOX=off to disable sandbox")
	}

	t.Setenv("LUNE_SANDBOX", "")
	t.Setenv("LUNE_NO_SANDBOX", "1")
	if !IsSandboxDisabled(false) {
		t.Errorf("expected LUNE_NO_SANDBOX=1 to disable sandbox")
	}

	t.Setenv("LUNE_SANDBOX", "")
	t.Setenv("LUNE_NO_SANDBOX", "")
	if IsSandboxDisabled(false) {
		t.Errorf("expected sandbox to be enabled by default")
	}
}
