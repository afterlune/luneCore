package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func subscribeTestEvents(t *testing.T, client *Core, sessionID string) <-chan Event {
	t.Helper()
	runtime, err := client.OpenSessionRuntime(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	cursor, err := client.sessions.LatestRuntimeEventCursor(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LatestRuntimeEventCursor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, err := runtime.SubscribeEvents(ctx, cursor)
	if err != nil {
		cancel()
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		for range events {
		}
	})
	return events
}

func interruptTestTask(t *testing.T, client *Core, sessionID string) bool {
	t.Helper()
	state, ok := client.StateForSession(sessionID)
	if !ok || state.ExecutionID == "" {
		return false
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	return runtime.Interrupt(state.ExecutionID)
}

func runtimeTestConfig(workspace, stateDir string, provider publicmodel.Provider) Config {
	return Config{
		Workspace: workspace, StateDir: stateDir,
		Memory: MemoryConfig{Disabled: true},
		ModelConfig: &publicmodel.Config{
			Providers:    map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Models:       map[string]publicmodel.ModelProfile{"host": {Provider: "host", Model: "test-model"}},
			DefaultModel: "host",
		},
		ModelProviders: map[string]publicmodel.Provider{"host": provider},
	}
}

type hostEchoTool struct {
	mu           sync.Mutex
	invocationID string
	calls        int
}

func (t *hostEchoTool) Definition() publictool.Definition {
	return publicmodel.FunctionTool{
		Name: "host_echo", Description: "Echo text from the host runtime.",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func (t *hostEchoTool) Run(ctx context.Context, _ json.RawMessage) publictool.Result {
	t.mu.Lock()
	t.calls++
	t.invocationID = publictool.InvocationID(ctx)
	t.mu.Unlock()
	return publictool.Result{Output: "host tool result"}
}

func newProgrammaticCore(t *testing.T, provider publicmodel.Provider, hostTools ...publictool.Registration) *Core {
	t.Helper()
	workspace, stateDir := t.TempDir(), t.TempDir()
	client, err := New(Config{
		Workspace: workspace, StateDir: stateDir,
		Memory: MemoryConfig{Disabled: true},
		ModelConfig: &publicmodel.Config{
			Providers:    map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Models:       map[string]publicmodel.ModelProfile{"host": {Provider: "host", Model: "test-model"}},
			DefaultModel: "host",
		},
		ModelProviders: map[string]publicmodel.Provider{"host": provider},
		Tools:          hostTools,
	})
	if err != nil {
		t.Fatalf("New programmatic Core: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Core: %v", err)
		}
	})
	return client
}
