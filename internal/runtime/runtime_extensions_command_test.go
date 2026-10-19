package runtime

import (
	"context"
	"encoding/json"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestCoreRoutesCommandsThroughInjectedExecutor(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("select-command", "select_tool_category", `{"category":"command"}`),
		hostToolCallResponse("command-call", "command", `{"command":"echo safe"}`),
		hostTextResponse("finished"),
	}}
	var received publictool.CommandRequest
	client, err := New(Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(),
		Memory: MemoryConfig{Disabled: true},
		ModelConfig: &publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Profiles:       map[string]publicmodel.ModelProfile{"host": {Provider: "host", Model: "test-model"}},
			DefaultProfile: "host",
		},
		ModelProviders: map[string]publicmodel.Provider{"host": provider},
		CommandExecutor: hostCommandExecutorFunc(func(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
			received = request
			if got := publictool.InvocationID(ctx); got != request.InvocationID {
				t.Errorf("command context invocation ID=%q, request=%q", got, request.InvocationID)
			}
			return publictool.CommandResult{Output: "safe output"}, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if err := client.SetPermissionMode(PermissionFullAccess); err != nil {
		t.Fatal(err)
	}
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, _ := NewTaskInput("run the safe command")
	if _, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: input}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if received.Command != "echo safe" || received.Workspace != client.Workspace() || received.InvocationID == "" {
		t.Fatalf("command request = %#v", received)
	}
}

type hostCommandExecutorFunc func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error)

func (f hostCommandExecutorFunc) Execute(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
	return f(ctx, request)
}
