package tool

import (
	"context"
	"encoding/json"
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

func TestCommandToolRequiresExplicitLocalExecution(t *testing.T) {
	result := (CommandTool{Workspace: t.TempDir()}).Run(context.Background(), json.RawMessage(`{"command":"echo test"}`))
	if result.Err == nil {
		t.Fatal("local command execution was enabled implicitly")
	}
}
