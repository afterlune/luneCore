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
	mu               sync.Mutex
	responses        []*publicmodel.ModelResponse
	requests         int
	seenRequests     []publicmodel.ModelRequest
	manualAcceptance bool
}

func (p *scriptedHostProvider) Name() string { return "host" }

func (p *scriptedHostProvider) CreateResponse(_ context.Context, request publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stage := acceptanceRequestStage(request)
	if !p.manualAcceptance && (stage == "criteria" || stage == "review") && !scriptHasRequestedTool(p.responses, request) {
		if stage == "criteria" {
			return scriptedCriteriaResponse(request), nil
		}
		return scriptedAcceptanceResponse(request), nil
	}
	if len(p.responses) == 0 {
		return nil, errors.New("scripted provider ran out of responses")
	}
	p.seenRequests = append(p.seenRequests, request)
	response := p.responses[0]
	p.responses = p.responses[1:]
	p.requests++
	return response, nil
}

func acceptanceRequestStage(request publicmodel.ModelRequest) string {
	var names []string
	for _, definition := range request.Tools {
		function, ok := definition.(publicmodel.FunctionTool)
		if !ok {
			continue
		}
		names = append(names, function.Name)
	}
	return acceptanceRequestStageNames(names)
}

func acceptanceRequestStageNames(names []string) string {
	for _, name := range names {
		switch name {
		case "prepare_task_criteria":
			return "criteria"
		case "submit_acceptance", "request_acceptance_evidence":
			return "review"
		}
	}
	return "execution"
}

func scriptHasRequestedTool(responses []*publicmodel.ModelResponse, request publicmodel.ModelRequest) bool {
	if len(responses) == 0 {
		return false
	}
	allowed := make(map[string]bool)
	for _, definition := range request.Tools {
		if function, ok := definition.(publicmodel.FunctionTool); ok {
			allowed[function.Name] = true
		}
	}
	for _, item := range responses[0].Output {
		if call, ok := item.(publicmodel.OutputFunctionCall); ok && allowed[call.Name] {
			return true
		}
	}
	return false
}

func scriptedCriteriaResponse(request publicmodel.ModelRequest) *publicmodel.ModelResponse {
	text := scriptedCurrentTask(request.Instructions)
	kind := "answer"
	lower := strings.ToLower(text)
	if strings.Contains(lower, "plan") || strings.Contains(text, "计划") || strings.Contains(text, "规划") {
		kind = "plan"
	} else if strings.Contains(lower, "implement") || strings.Contains(lower, "build") || strings.Contains(lower, "fix") || strings.Contains(text, "实现") || strings.Contains(text, "修改") || strings.Contains(text, "修复") {
		kind = "execution"
	}
	if text == "" {
		text = "current user turn"
	}
	arguments, _ := json.Marshal(map[string]any{
		"kind":     kind,
		"criteria": []map[string]string{{"id": "current_request", "description": "Satisfy the current user request within its stated scope.", "basis": text}},
	})
	return &publicmodel.ModelResponse{Status: "completed", Output: []publicmodel.ResponseOutputItem{
		publicmodel.OutputFunctionCall{CallID: "test-criteria", Name: "prepare_task_criteria", Arguments: string(arguments)},
	}}
}

func scriptedAcceptanceResponse(request publicmodel.ModelRequest) *publicmodel.ModelResponse {
	instructions := request.Instructions
	candidateID := "candidate-1"
	if marker := "Candidate ID: "; strings.Contains(instructions, marker) {
		rest := strings.SplitN(instructions[strings.Index(instructions, marker)+len(marker):], "\n", 2)[0]
		if strings.TrimSpace(rest) != "" {
			candidateID = strings.TrimSpace(rest)
		}
	}
	var criteria []struct {
		ID string `json:"id"`
	}
	if marker := "Fixed criteria: "; strings.Contains(instructions, marker) {
		rest := instructions[strings.Index(instructions, marker)+len(marker):]
		if end := strings.Index(rest, "\n\nCandidate ID:"); end >= 0 {
			_ = json.Unmarshal([]byte(rest[:end]), &criteria)
		}
	}
	results := make([]map[string]any, 0, len(criteria))
	for _, criterion := range criteria {
		results = append(results, map[string]any{
			"criterion_id": criterion.ID, "outcome": "passed", "evidence": []string{"candidate:" + candidateID},
			"reason": "The scripted test provider accepts this candidate.",
		})
	}
	arguments, _ := json.Marshal(map[string]any{"candidate_id": candidateID, "results": results})
	return &publicmodel.ModelResponse{Status: "completed", Output: []publicmodel.ResponseOutputItem{
		publicmodel.OutputFunctionCall{CallID: "test-acceptance", Name: "submit_acceptance", Arguments: string(arguments)},
	}}
}

func scriptedCurrentTask(instructions string) string {
	marker := "Current user turn text (attachments remain in the conversation input): "
	index := strings.Index(instructions, marker)
	if index < 0 {
		return ""
	}
	var text string
	text = strings.TrimSpace(instructions[index+len(marker):])
	if end := strings.Index(text, "\n\n"); end >= 0 {
		text = text[:end]
	}
	return strings.Trim(text, "\" ")
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
	stage := acceptanceRequestStage(request)
	if stage == "criteria" {
		return scriptedCriteriaResponse(request), nil
	}
	if stage == "review" {
		return scriptedAcceptanceResponse(request), nil
	}
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
	eventStream := subscribeTestEvents(t, client, session.ID)
	input, err := NewTaskInput("use the host tool")
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.RunTask(context.Background(), TaskRequest{SessionID: session.ID, Input: input})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	var events []Event
	for {
		event := <-eventStream
		events = append(events, event)
		if event.Kind == EventTaskFinished {
			break
		}
	}
	var result publicmodel.ModelResponse
	if err := json.Unmarshal(response.Response, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Output) != 1 {
		t.Fatalf("response output = %#v", result.Output)
	}
	hostTool.mu.Lock()
	calls, invocationID := hostTool.calls, hostTool.invocationID
	hostTool.mu.Unlock()
	if calls != 1 || invocationID == "" {
		t.Fatalf("host tool calls=%d invocationID=%q", calls, invocationID)
	}
	state, ok := client.StateForSession(session.ID)
	if !ok || state.Status != TaskCompleted {
		t.Fatalf("session task state = %#v, found=%v", state, ok)
	}
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

func TestTaskBudgetReasonPersistsAndRecoveryCanRaiseLimit(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("select", "select_tool_category", `{"category":"files"}`),
		hostTextResponse("completed after recovery"),
	}}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	config.Execution.MaxSteps = 1
	client, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("continue with enough budget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: input, Execution: ExecutionOptions{MaxSteps: 2}}); !errors.Is(err, ErrTaskPaused) {
		t.Fatalf("RunTask error = %v, want a budget pause", err)
	}
	state, ok := client.StateForSession(saved.ID)
	if !ok || state.Status != TaskPaused || state.ExecutionID == "" {
		t.Fatalf("paused state = %#v, found=%t", state, ok)
	}
	checkpoint, err := client.checkpoints.Load(context.Background(), client.runtime.CheckpointID(saved.ID))
	if err != nil || checkpoint.Steps != 2 {
		t.Fatalf("checkpoint steps = %d, error=%v; want task override to replace host default", checkpoint.Steps, err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := runtime.WaitTurn(context.Background(), state.ExecutionID)
	if err != nil || paused.Status != TaskPaused || paused.BudgetReason != BudgetMaxSteps {
		t.Fatalf("paused result = %#v, error=%v", paused, err)
	}
	if _, err := runtime.Recover(context.Background(), ExecutionOptions{MaxSteps: 10}); err != nil {
		t.Fatalf("Recover with raised budget: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed, err := runtime.WaitTurn(ctx, state.ExecutionID)
	if err != nil || completed.Status != TaskCompleted || completed.BudgetReason != "" {
		t.Fatalf("completed result = %#v, error=%v", completed, err)
	}
}

func TestForkPausedTaskClonesCheckpointAndDurableReceipts(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("ask", "ask_user", `{"question":"Continue?","options":["yes","no"]}`),
		hostTextResponse("fork completed"),
		hostTextResponse("source completed"),
	}}
	client := newProgrammaticCore(t, provider)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("ask me before continuing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunTask(ctx, TaskRequest{SessionID: source.ID, Input: input}); !errors.Is(err, ErrTaskPaused) {
		t.Fatalf("RunTask error = %v, want a user-question pause", err)
	}
	sourceRuntime, err := client.OpenSessionRuntime(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sourceQuestion UserQuestionRequest
	for _, request := range mustPendingRequests(t, sourceRuntime, ctx) {
		if request.UserQuestion != nil {
			sourceQuestion = *request.UserQuestion
		}
	}
	if sourceQuestion.InvocationID == "" || sourceQuestion.CallID == "" {
		t.Fatalf("source question = %#v", sourceQuestion)
	}
	answer, _ := json.Marshal("yes")
	inserted, err := client.sessions.PutGraphResumeInput(ctx, source.ID, session.GraphResumeInput{
		RunID: sourceQuestion.RunID, InvocationID: sourceQuestion.InvocationID, CallID: sourceQuestion.CallID, Payload: answer,
	})
	if err != nil || !inserted {
		t.Fatalf("save source answer receipt: inserted=%t error=%v", inserted, err)
	}
	invocation := session.ToolInvocation{
		RunID: sourceQuestion.RunID, InvocationID: "fork-copy-receipt", SessionID: source.ID,
		ExecutionID: sourceQuestion.ExecutionID, AgentID: "lead", CallID: "completed-call", Name: "host_echo",
		Arguments: json.RawMessage(`{}`),
	}
	if _, _, err := client.sessions.StartToolInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if err := client.sessions.SaveToolInvocationOutput(ctx, invocation.RunID, invocation.InvocationID, session.ToolInvocationCompleted, json.RawMessage(`{"output":"saved"}`)); err != nil {
		t.Fatal(err)
	}

	fork, err := client.ForkPausedTask(ctx, source.ID)
	if err != nil {
		t.Fatalf("ForkPausedTask: %v", err)
	}
	forkRunID := client.runtime.CheckpointID(fork.ID)
	forkCheckpoint, err := client.checkpoints.Load(ctx, forkRunID)
	if err != nil {
		t.Fatal(err)
	}
	if forkCheckpoint.RunID != forkRunID || forkCheckpoint.Revision != 1 {
		t.Fatalf("fork checkpoint identity = run %q revision %d", forkCheckpoint.RunID, forkCheckpoint.Revision)
	}
	var forkExecutionID string
	for _, item := range forkCheckpoint.Invocations {
		if item.State.ExecutionID != "" {
			forkExecutionID = item.State.ExecutionID
			if item.State.RunID != forkRunID {
				t.Fatalf("fork invocation run id = %q, want %q", item.State.RunID, forkRunID)
			}
			break
		}
	}
	if forkExecutionID == "" || forkExecutionID == sourceQuestion.ExecutionID {
		t.Fatalf("fork execution id = %q, source = %q", forkExecutionID, sourceQuestion.ExecutionID)
	}
	clonedTool, err := client.sessions.GetToolInvocation(ctx, forkRunID, invocation.InvocationID)
	if err != nil || clonedTool.SessionID != fork.ID || clonedTool.ExecutionID != forkExecutionID || clonedTool.Status != session.ToolInvocationCompleted {
		t.Fatalf("forked tool receipt = %#v, error=%v", clonedTool, err)
	}
	forkInputs, err := client.sessions.GraphResumeInputs(ctx, fork.ID, forkRunID)
	if err != nil || len(forkInputs) != 1 || string(forkInputs[0].Payload) != string(answer) {
		t.Fatalf("forked continuation receipts = %#v, error=%v", forkInputs, err)
	}
	forkRuntime, err := client.OpenSessionRuntime(ctx, fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	var forkQuestion UserQuestionRequest
	for _, request := range mustPendingRequests(t, forkRuntime, ctx) {
		if request.UserQuestion != nil {
			forkQuestion = *request.UserQuestion
		}
	}
	if forkQuestion.RunID != forkRunID || forkQuestion.ExecutionID != forkExecutionID || forkQuestion.InvocationID != sourceQuestion.InvocationID || forkQuestion.CallID != sourceQuestion.CallID {
		t.Fatalf("fork question identity = %#v, source = %#v", forkQuestion, sourceQuestion)
	}
	if _, err := forkRuntime.Recover(ctx); err != nil {
		t.Fatalf("recover fork: %v", err)
	}
	forkResult, err := forkRuntime.WaitTurn(ctx, forkExecutionID)
	if err != nil || forkResult.Status != TaskCompleted {
		t.Fatalf("fork result = %#v, error=%v", forkResult, err)
	}
	if err := sourceRuntime.ResolveUserQuestion(ctx, UserQuestionAnswer{
		RunID: sourceQuestion.RunID, InvocationID: sourceQuestion.InvocationID, CallID: sourceQuestion.CallID, Answer: "yes",
	}); err != nil {
		t.Fatalf("resolve source question: %v", err)
	}
	sourceResult, err := sourceRuntime.WaitTurn(ctx, sourceQuestion.ExecutionID)
	if err != nil || sourceResult.Status != TaskCompleted {
		t.Fatalf("source result = %#v, error=%v", sourceResult, err)
	}
}

func mustPendingRequests(t *testing.T, runtime *SessionRuntime, ctx context.Context) []PendingRequest {
	t.Helper()
	requests, err := runtime.PendingRequests(ctx)
	if err != nil {
		t.Fatalf("PendingRequests: %v", err)
	}
	return requests
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
		Memory: MemoryConfig{Disabled: true},
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
	if received.Command != "echo safe" || received.Workspace != client.Workspace() || received.InvocationID == "" {
		t.Fatalf("command request = %#v", received)
	}
}

func TestActiveTurnWithoutCheckpointIsNotReplayedAfterUpgrade(t *testing.T) {
	provider := &scriptedHostProvider{}
	client := newProgrammaticCore(t, provider)
	ctx := context.Background()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	queueID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(storedTurnRequest{Input: json.RawMessage(`"perform a side effect"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.sessions.EnqueueTurn(ctx, session.QueuedTurn{
		SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Request: request,
	}, mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Status: TaskQueued})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.sessions.SetQueuedTurnStatusWithEvent(ctx, saved.ID, queueID, session.QueuedTurnActive,
		mustMarshalEvent(Event{Kind: EventTaskStarted, SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Status: TaskRunning})); err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.WaitTurn(ctx, executionID)
	if err != nil || result.Status != TaskFailed {
		t.Fatalf("reconciled result = %#v, error=%v; want missing-checkpoint failure", result, err)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 0 {
		t.Fatalf("provider saw %d calls while reconciling an old active turn", requests)
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
		Memory: MemoryConfig{Disabled: true},
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
	runID := client.runtime.CheckpointID(saved.ID)
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
		FormatVersion: graph.CheckpointFormatVersion, RunID: runID, MachineID: "lune-agent-v2", Revision: 1, NextID: 2,
		Invocations: []graph.Invocation[agentgraph.State]{{
			ID: "active-tool", Node: "execute_tool", Status: graph.InvocationReady,
			State: agentgraph.State{RunID: runID, ExecutionID: executionID, PendingIndex: 0,
				PendingCalls: []model.OutputFunctionCall{call}},
		}},
	}); err != nil {
		t.Fatalf("create pending checkpoint: %v", err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	pendingRequests, err := runtime.PendingRequests(context.Background())
	var requests []ToolRecoveryRequest
	for _, request := range pendingRequests {
		if request.ToolRecovery != nil {
			requests = append(requests, *request.ToolRecovery)
		}
	}
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
	events := subscribeTestEvents(t, client, saved.ID)
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
	if err := runtime.ResolveToolRecovery(context.Background(), invocationID, ToolRecoverySkip); err != nil {
		t.Fatalf("ResolveToolRecovery: %v", err)
	}
	if err := runtime.ResolveToolRecovery(context.Background(), childInvocationID, ToolRecoverySkip); err != nil {
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
