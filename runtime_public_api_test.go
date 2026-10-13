package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
	"github.com/afterlune/luneCore/tool"
)

type externalProvider struct{}

func (externalProvider) Name() string { return "external" }

func (externalProvider) CreateResponse(_ context.Context, request model.ModelRequest, _ model.ModelProfile) (*model.ModelResponse, error) {
	return externalResponseForRequest(request), nil
}

func (externalProvider) StreamResponse(ctx context.Context, request model.ModelRequest, profile model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	response := externalResponseForRequest(request)
	if emit != nil {
		if err := emit(model.ModelStreamEvent{Type: model.ModelStreamCompleted, Response: response}); err != nil {
			return nil, err
		}
	}
	return response, nil
}

type personalInstructionsProvider struct {
	requests []model.ModelRequest
}

func (*personalInstructionsProvider) Name() string { return "personal" }

func (p *personalInstructionsProvider) CreateResponse(_ context.Context, request model.ModelRequest, _ model.ModelProfile) (*model.ModelResponse, error) {
	p.requests = append(p.requests, request)
	return externalResponseForRequest(request), nil
}

func (p *personalInstructionsProvider) StreamResponse(ctx context.Context, request model.ModelRequest, profile model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err != nil {
		return nil, err
	}
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

func externalResponseForRequest(request model.ModelRequest) *model.ModelResponse {
	stage := "execution"
	for _, definition := range request.Tools {
		if function, ok := definition.(model.FunctionTool); ok {
			switch function.Name {
			case "prepare_task_criteria":
				stage = "criteria"
			case "submit_acceptance", "request_acceptance_evidence":
				stage = "acceptance"
			}
		}
	}
	if stage == "criteria" {
		arguments, _ := json.Marshal(map[string]any{
			"kind": "answer", "criteria": []map[string]string{{
				"id": "user_request", "description": "Answer the current user request.", "basis": "hello",
			}},
		})
		return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{
			model.OutputFunctionCall{CallID: "criteria-call", Name: "prepare_task_criteria", Arguments: string(arguments)},
		}}
	}
	if stage == "acceptance" {
		arguments, _ := json.Marshal(map[string]any{
			"candidate_id": "candidate-1", "results": []map[string]any{{
				"criterion_id": "user_request", "outcome": "passed", "evidence": []string{"candidate:candidate-1"}, "reason": "The candidate answers the user.",
			}},
		})
		return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{
			model.OutputFunctionCall{CallID: "acceptance-call", Name: "submit_acceptance", Arguments: string(arguments)},
		}}
	}
	return externalResponse()
}

func TestExternalGoPackageCanEmbedAndExtendCore(t *testing.T) {
	client, err := core.New(core.Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(),
		Memory: core.MemoryConfig{Disabled: true},
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

func TestHostPersonalInstructionsReachOnlyNormalTaskRequests(t *testing.T) {
	provider := &personalInstructionsProvider{}
	client, err := core.New(core.Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(),
		Memory:               core.MemoryConfig{Disabled: true},
		PersonalInstructions: "Prefers concise answers.",
		ModelConfig: &model.Config{
			Providers:    map[string]model.ProviderConfig{"personal": {Protocol: model.ProtocolResponses}},
			Models:       map[string]model.ModelProfile{"main": {Provider: "personal", Model: "test-model"}},
			DefaultModel: "main",
		},
		ModelProviders: map[string]model.Provider{"personal": provider},
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
	result, err := client.RunTask(context.Background(), core.TaskRequest{SessionID: session.ID, Input: input})
	if err != nil || result.Status != core.TaskCompleted {
		t.Fatalf("RunTask result=%#v error=%v", result, err)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("model requests = %d, want criteria, execution, and acceptance", len(provider.requests))
	}
	if strings.Contains(provider.requests[0].Instructions, "Prefers concise answers.") ||
		strings.Contains(provider.requests[2].Instructions, "Prefers concise answers.") {
		t.Fatal("personal instructions appeared in acceptance criteria or review")
	}
	if !strings.Contains(provider.requests[1].Instructions, "Prefers concise answers.") {
		t.Fatal("normal task request is missing host personal instructions")
	}
}
