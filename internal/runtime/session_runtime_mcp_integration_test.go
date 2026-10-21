package runtime

import (
	"context"
	"encoding/json"
	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"sync"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

type recordingMCPTaskProvider struct {
	delegate *scriptedHostProvider

	mu       sync.Mutex
	requests []publicmodel.ModelRequest
}

func (p *recordingMCPTaskProvider) Name() string { return "host" }

func (p *recordingMCPTaskProvider) CreateResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	p.record(request)
	return p.delegate.CreateResponse(ctx, request, profile)
}

func (p *recordingMCPTaskProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err == nil && emit != nil {
		err = emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamCompleted, Response: response})
	}
	return response, err
}

func (p *recordingMCPTaskProvider) record(request publicmodel.ModelRequest) {
	if request.Input != nil {
		input := *request.Input
		input.Items = append([]publicmodel.InputItem(nil), request.Input.Items...)
		request.Input = &input
	}
	p.mu.Lock()
	p.requests = append(p.requests, request)
	p.mu.Unlock()
}

func (p *recordingMCPTaskProvider) modelRequests() []publicmodel.ModelRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]publicmodel.ModelRequest(nil), p.requests...)
}

func TestMCPPromptAndResourceRunThroughCoreTask(t *testing.T) {
	server, expectedImageURL := newMCPPromptResourceFixture(t)
	provider := &recordingMCPTaskProvider{delegate: &scriptedHostProvider{
		responses: []*publicmodel.ModelResponse{hostTextResponse("The picture matches the reference.")},
	}}
	client := newProgrammaticCore(t, provider)
	attachHostMCPServer(t, client, "fixture", server.URL)
	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := runtime.GetMCPPrompt(context.Background(), "fixture", mcpFixturePromptName, nil)
	if err != nil {
		t.Fatalf("get MCP prompt: %v", err)
	}
	resource, err := runtime.ReadMCPResource(context.Background(), "fixture", mcpFixtureResourceURI)
	if err != nil {
		t.Fatalf("read MCP resource: %v", err)
	}
	if prompt.Description != mcpFixturePromptDescription || prompt.Input == nil || resource == nil {
		t.Fatalf("MCP prompt/resource results = %#v, %#v", prompt, resource)
	}
	items := append([]publicmodel.InputItem{}, prompt.Input.Items...)
	items = append(items, resource.Items...)
	input := publicmodel.NewResponseInputItems(items...)
	if len(input.Items) != 3 {
		t.Fatalf("combined MCP input has %d messages, want 3", len(input.Items))
	}
	assertMCPPromptResourceInput(t, input, expectedImageURL)
	taskInput, err := NewTaskInput(input)
	if err != nil {
		t.Fatalf("wrap MCP input: %v", err)
	}

	result, err := client.RunTask(context.Background(), TaskRequest{SessionID: session.ID, Input: taskInput})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if result.Status != TaskCompleted {
		t.Fatalf("task result = %#v", result)
	}

	requests := provider.modelRequests()
	if len(requests) != 1 {
		t.Fatalf("Core model requests = %d, want one", len(requests))
	}
	inputWithoutHost := model.NewResponseInputItems()
	for _, item := range requests[0].Input.Items {
		if msg, ok := item.(model.InputMessage); ok && agentgraph.IsHostContextMessage(msg) {
			continue
		}
		inputWithoutHost.Items = append(inputWithoutHost.Items, item)
	}
	assertMCPPromptResourceInput(t, inputWithoutHost, expectedImageURL)
}
