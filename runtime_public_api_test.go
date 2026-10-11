package core_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
	"github.com/afterlune/luneCore/tool"
)

type externalProvider struct{}

func (externalProvider) Name() string { return "external" }

func (externalProvider) CreateResponse(context.Context, model.ModelRequest, model.ModelProfile) (*model.ModelResponse, error) {
	return externalResponse(), nil
}

func (externalProvider) StreamResponse(ctx context.Context, request model.ModelRequest, profile model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	response := externalResponse()
	if emit != nil {
		if err := emit(model.ModelStreamEvent{Type: model.ModelStreamCompleted, Response: response}); err != nil {
			return nil, err
		}
	}
	return response, nil
}

type externalTool struct{}

func (externalTool) Definition() tool.Definition {
	return model.FunctionTool{Name: "external_tool", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
}

func (externalTool) Run(context.Context, json.RawMessage) tool.Result {
	return tool.Result{Output: "external"}
}

type externalCommandExecutor struct{}

func (externalCommandExecutor) Execute(context.Context, tool.CommandRequest) (tool.CommandResult, error) {
	return tool.CommandResult{Output: "external command"}, nil
}

func externalResponse() *model.ModelResponse {
	return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{
		model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "hello"}}},
	}}
}

func TestExternalGoPackageCanEmbedAndExtendCore(t *testing.T) {
	client, err := core.New(core.Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(),
		ModelConfig: &model.Config{
			Providers:    map[string]model.ProviderConfig{"external": {Protocol: model.ProtocolResponses}},
			Models:       map[string]model.ModelProfile{"main": {Provider: "external", Model: "test-model"}},
			DefaultModel: "main",
		},
		ModelProviders: map[string]model.Provider{"external": externalProvider{}},
		Tools: []tool.Registration{{Tool: externalTool{}, Metadata: tool.Metadata{
			Category:        tool.Category{Name: "external", Description: "External tools."},
			PermissionClass: tool.PermissionWorkspaceRead, ReadOnly: true,
		}}},
		CommandExecutor: externalCommandExecutor{},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	defer client.Close()
	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := core.NewTaskInput("hello")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	eventCtx, cancelEvents := context.WithCancel(context.Background())
	events, err := runtime.SubscribeEvents(eventCtx, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	if _, err := runtime.SubmitTurn(context.Background(), core.TurnRequest{Input: input}, core.StartIfIdle); err != nil {
		t.Fatalf("SubmitTurn: %v", err)
	}
	finished := false
	for !finished {
		select {
		case event := <-events:
			finished = event.Kind == core.EventTaskFinished
		case <-time.After(5 * time.Second):
			t.Fatal("asynchronous task did not finish")
		}
	}
	cancelEvents()
	for range events {
	}
	closeDeadline := time.Now().Add(5 * time.Second)
	for {
		if err := client.Close(); err == nil {
			break
		}
		if time.Now().After(closeDeadline) {
			t.Fatal("Core did not become idle for Close")
		}
		time.Sleep(time.Millisecond)
	}
}
