package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
)

type scriptedHostProvider struct {
	mu        sync.Mutex
	responses []*publicmodel.ModelResponse
	requests  int
}

func (p *scriptedHostProvider) Name() string { return "host" }

func (p *scriptedHostProvider) CreateResponse(_ context.Context, request publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.responses) == 0 {
		return nil, errors.New("scripted provider ran out of responses")
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	p.requests++
	return response, nil
}

func (p *scriptedHostProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err != nil {
		return nil, err
	}
	if emit != nil {
		if err := emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamCompleted, Response: response}); err != nil {
			return nil, err
		}
	}
	return response, nil
}

type barrierHostProvider struct {
	entered chan string
	release chan struct{}
}

func (p *barrierHostProvider) Name() string { return "host" }

func (p *barrierHostProvider) CreateResponse(ctx context.Context, request publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	text := ""
	if request.Input != nil && request.Input.Text != nil {
		text = *request.Input.Text
	}
	select {
	case p.entered <- text:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return hostTextResponse("done"), nil
}

func (p *barrierHostProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err == nil && emit != nil {
		err = emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamCompleted, Response: response})
	}
	return response, err
}

type hostEchoTool struct {
	mu           sync.Mutex
	invocationID string
	calls        int
}

type hostCommandExecutorFunc func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error)

func (f hostCommandExecutorFunc) Execute(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
	return f(ctx, request)
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

func hostTextResponse(text string) *publicmodel.ModelResponse {
	return &publicmodel.ModelResponse{
		Status: "completed",
		Output: []publicmodel.ResponseOutputItem{publicmodel.OutputMessage{
			Role: publicmodel.RoleAssistant, Content: []publicmodel.OutputContentPart{publicmodel.OutputText{Text: text}},
		}},
	}
}

func hostToolCallResponse(callID, name, arguments string) *publicmodel.ModelResponse {
	return &publicmodel.ModelResponse{
		Status: "completed",
		Output: []publicmodel.ResponseOutputItem{publicmodel.OutputFunctionCall{CallID: callID, Name: name, Arguments: arguments}},
	}
}

func newProgrammaticCore(t *testing.T, provider publicmodel.Provider, hostTools ...publictool.Registration) *Core {
	t.Helper()
	workspace, stateDir := t.TempDir(), t.TempDir()
	client, err := New(Config{
		Workspace: workspace, StateDir: stateDir,
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

func TestCoreAcceptsHostProviderAndRegisteredTool(t *testing.T) {
	hostTool := &hostEchoTool{}
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("select-host", "select_tool_category", `{"category":"host"}`),
		hostToolCallResponse("host-call", "host_echo", `{}`),
		hostTextResponse("finished"),
	}}
	client := newProgrammaticCore(t, provider, publictool.Registration{
		Tool: hostTool,
		Metadata: publictool.Metadata{
			Category:        publictool.Category{Name: "host", Description: "Host supplied tools."},
			PermissionClass: publictool.PermissionWorkspaceRead, ReadOnly: true,
		},
	})
	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	var eventsMu sync.Mutex
	var events []Event
	client.SetEventHandler(func(event Event) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	})
	input, err := NewTaskInput("use the host tool")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.RunTask(context.Background(), TaskRequest{SessionID: session.ID, Input: input})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	var result publicmodel.ModelResponse
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Output) != 1 {
		t.Fatalf("response output = %#v", result.Output)
	}
	hostTool.mu.Lock()
	calls, invocationID := hostTool.calls, hostTool.invocationID
	hostTool.mu.Unlock()
	if calls != 1 || len(invocationID) < len("tool_") || invocationID[:len("tool_")] != "tool_" {
		t.Fatalf("host tool calls=%d invocationID=%q", calls, invocationID)
	}
	state, ok := client.StateForSession(session.ID)
	if !ok || state.Status != TaskCompleted {
		t.Fatalf("session task state = %#v, found=%v", state, ok)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	var started, finished bool
	for _, event := range events {
		if event.SessionID != session.ID {
			continue
		}
		if event.Kind == EventToolStarted && event.Tool.ID == "host-call" && event.Tool.InvocationID == invocationID {
			started = true
		}
		if event.Kind == EventToolFinished && event.Tool.ID == "host-call" && event.Tool.InvocationID == invocationID {
			finished = true
		}
	}
	if !started || !finished {
		t.Fatalf("host tool lifecycle events started=%v finished=%v", started, finished)
	}
}

func TestCoreRoutesCommandsThroughInjectedExecutor(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("select-command", "select_tool_category", `{"category":"command"}`),
		hostToolCallResponse("command-call", "command", `{"command":"echo safe"}`),
		hostTextResponse("finished"),
	}}
	var received publictool.CommandRequest
	client, err := New(Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(),
		ModelConfig: &publicmodel.Config{
			Providers:    map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Models:       map[string]publicmodel.ModelProfile{"host": {Provider: "host", Model: "test-model"}},
			DefaultModel: "host",
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
	if received.Command != "echo safe" || received.Workspace != client.Workspace() || !strings.HasPrefix(received.InvocationID, "tool_") {
		t.Fatalf("command request = %#v", received)
	}
}

func TestCoreRunsDifferentSessionsConcurrentlyAndSerializesEachSession(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 3), release: make(chan struct{}, 2)}
	client := newProgrammaticCore(t, provider)
	first, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() {
		input, _ := NewTaskInput("first session")
		_, err := client.RunTask(ctx, TaskRequest{SessionID: first.ID, Input: input})
		firstDone <- err
	}()
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("first session did not reach the provider")
	}
	secondDone := make(chan error, 1)
	go func() {
		input, _ := NewTaskInput("second session")
		_, err := client.RunTask(ctx, TaskRequest{SessionID: second.ID, Input: input})
		secondDone <- err
	}()
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("second session did not run while the first session was active")
	}
	input, _ := NewTaskInput("duplicate session task")
	if _, err := client.RunTask(ctx, TaskRequest{SessionID: first.ID, Input: input}); err == nil {
		t.Fatal("started a second task in the same session")
	}
	provider.release <- struct{}{}
	provider.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first task: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second task: %v", err)
	}
	if firstState, _ := client.StateForSession(first.ID); firstState.Status != TaskCompleted {
		t.Fatalf("first task status = %q", firstState.Status)
	}
	if secondState, _ := client.StateForSession(second.ID); secondState.Status != TaskCompleted {
		t.Fatalf("second task status = %q", secondState.Status)
	}
	if running := client.ActiveTasks(); len(running) != 0 {
		t.Fatalf("ActiveTasks after completion = %#v", running)
	}
}

func TestProgrammaticModelConfigOverridesFileAndStateDirOwnsMCPState(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	project := projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			"file-provider": {Protocol: model.ProtocolResponses, BaseURL: "https://file-provider.invalid", APIKey: "file-key"},
			"host":          {Protocol: model.ProtocolResponses, BaseURL: "https://old-host.invalid", APIKey: "file-key"},
		},
		Models: map[string]model.ModelProfile{
			"file-profile": {Provider: "file-provider", Model: "file-model"},
			"host-profile": {Provider: "host", Model: "old-model"},
		},
		DefaultModel: "file-profile", ContextWindow: 24000,
	}
	writeProjectSettingsConfig(t, workspace, project)
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostTextResponse("unused")}}
	client, err := New(Config{
		Workspace: workspace, StateDir: stateDir,
		ModelConfig: &publicmodel.Config{
			Providers:    map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Models:       map[string]publicmodel.ModelProfile{"host-profile": {Provider: "host", Model: "new-model"}},
			DefaultModel: "host-profile",
		},
		ModelProviders: map[string]publicmodel.Provider{"host": provider},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	profiles := client.ListModelProfiles()
	if len(profiles) != 2 || profiles[0].Profile != "file-profile" || profiles[1].Model != "new-model" {
		t.Fatalf("merged profiles = %#v", profiles)
	}
	if defaults := client.ProjectContextDefaults(); defaults.ContextWindow != 24000 {
		t.Fatalf("file project defaults = %#v", defaults)
	}
	if err := client.DisableMCPServer("not-configured"); err != nil {
		t.Fatalf("DisableMCPServer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "mcp-enabled.json")); err != nil {
		t.Fatalf("MCP state in custom StateDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".luneagent", "mcp-enabled.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected MCP state in workspace default directory, stat err=%v", err)
	}
}

func TestUnknownToolOutcomeCanBeSkippedAndReplayedWithoutExecution(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostTextResponse("unused")}}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runID := client.runtime.checkpointID(saved.ID)
	executionID := "execution-test"
	invocationID := "tool-unknown-outcome"
	call := model.OutputFunctionCall{CallID: "call-unknown", Name: "read_file", Arguments: `{}`}
	if _, _, err := client.sessions.StartToolInvocation(context.Background(), session.ToolInvocation{
		RunID: runID, InvocationID: invocationID, SessionID: saved.ID, ExecutionID: executionID, AgentID: "lead",
		CallID: call.CallID, Name: call.Name, Arguments: json.RawMessage(call.Arguments),
	}); err != nil {
		t.Fatalf("StartToolInvocation: %v", err)
	}
	childInvocationID := "tool-child-unknown-outcome"
	childRunID := runID + ":team-run:worker"
	if _, _, err := client.sessions.StartToolInvocation(context.Background(), session.ToolInvocation{
		RunID: childRunID, InvocationID: childInvocationID, SessionID: saved.ID, ExecutionID: executionID, AgentID: "worker",
		CallID: "child-call", Name: "mcp__server__write", Arguments: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("StartToolInvocation for child: %v", err)
	}
	if err := client.checkpoints.Create(context.Background(), graph.Checkpoint[agentgraph.State]{
		FormatVersion: graph.CheckpointFormatVersion, RunID: runID, MachineID: "lune-agent-v1", Revision: 1, NextID: 2,
		Invocations: []graph.Invocation[agentgraph.State]{{
			ID: "active-tool", Node: "execute_tool", Status: graph.InvocationReady,
			State: agentgraph.State{RunID: runID, ExecutionID: executionID, PendingInvocationID: invocationID, PendingIndex: 0,
				PendingCalls: []model.OutputFunctionCall{call}},
		}},
	}); err != nil {
		t.Fatalf("create pending checkpoint: %v", err)
	}
	requests, err := client.PendingToolRecoveries(context.Background(), saved.ID)
	if err != nil || len(requests) != 2 {
		t.Fatalf("PendingToolRecoveries = %#v, err=%v", requests, err)
	}
	seen := map[string]bool{}
	for _, request := range requests {
		seen[request.InvocationID] = true
	}
	if !seen[invocationID] || !seen[childInvocationID] {
		t.Fatalf("pending recovery requests = %#v", requests)
	}
	events := make(chan Event, 2)
	client.SetEventHandler(func(event Event) { events <- event })
	recoveryErr := client.checkToolRecovery(context.Background(), saved.ID, TaskState{TaskID: "resume-task", ExecutionID: executionID})
	var required *ToolRecoveryRequiredError
	if !errors.As(recoveryErr, &required) || required.InvocationID != invocationID {
		t.Fatalf("checkToolRecovery error = %v", recoveryErr)
	}
	for i := 0; i < 2; i++ {
		event := <-events
		if event.Kind != EventToolRecoveryRequired || event.ToolRecovery == nil || !seen[event.ToolRecovery.InvocationID] {
			t.Fatalf("recovery event = %#v", event)
		}
	}
	if err := client.ResolveToolRecovery(context.Background(), saved.ID, invocationID, ToolRecoverySkip); err != nil {
		t.Fatalf("ResolveToolRecovery: %v", err)
	}
	if err := client.ResolveToolRecovery(context.Background(), saved.ID, childInvocationID, ToolRecoverySkip); err != nil {
		t.Fatalf("ResolveToolRecovery child: %v", err)
	}
	identity := publictool.CallIdentity{SessionID: saved.ID, TaskID: "resume-task", RunID: runID, ExecutionID: executionID, AgentID: "lead", InvocationID: invocationID}
	executed := false
	output, err := client.executeToolCall(publictool.WithCallIdentity(context.Background(), identity), call, func() model.InputFunctionCallOutput {
		executed = true
		return model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("unexpected execution")}
	})
	if err != nil || executed || output.CallID != call.CallID {
		t.Fatalf("replayed skipped tool = %#v, executed=%v, err=%v", output, executed, err)
	}
	if output.Output.Text == nil || !strings.Contains(*output.Output.Text, "chose to skip") {
		t.Fatalf("skipped tool output = %#v", output.Output)
	}

	completedID := "tool-completed-result"
	completedCall := model.OutputFunctionCall{CallID: "call-completed", Name: "read_file", Arguments: `{}`}
	completedContext := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{
		SessionID: saved.ID, TaskID: "resume-task", RunID: runID, ExecutionID: executionID,
		AgentID: "lead", InvocationID: completedID,
	})
	executions := 0
	execute := func() model.InputFunctionCallOutput {
		executions++
		return model.InputFunctionCallOutput{
			CallID: completedCall.CallID, Output: model.NewToolOutputText("saved result"),
			TranscriptOnly: []model.ContentPart{model.InputText{Text: "host-only transcript"}},
		}
	}
	firstOutput, err := client.executeToolCall(completedContext, completedCall, execute)
	if err != nil {
		t.Fatalf("first tool execution: %v", err)
	}
	replayedOutput, err := client.executeToolCall(completedContext, completedCall, execute)
	if err != nil || executions != 1 || replayedOutput.CallID != firstOutput.CallID || len(replayedOutput.TranscriptOnly) != 1 {
		t.Fatalf("completed tool replay = %#v, calls=%d, err=%v", replayedOutput, executions, err)
	}
}
