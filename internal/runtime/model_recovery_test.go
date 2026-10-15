package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type faultHostProvider struct {
	inner         *scriptedHostProvider
	mu            sync.Mutex
	failAt        int
	requests      int
	failed        bool
	streamPartial bool
	retryAfter    time.Duration
}

func (*faultHostProvider) Name() string { return "host" }
func (p *faultHostProvider) CreateResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	return p.respond(ctx, request, profile, nil)
}
func (p *faultHostProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	return p.respond(ctx, request, profile, emit)
}
func (p *faultHostProvider) respond(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	p.mu.Lock()
	fail := false
	p.requests++
	fail = !p.failed && p.requests == p.failAt
	if fail {
		p.failed = true
	}
	p.mu.Unlock()
	if fail {
		if p.streamPartial && emit != nil {
			if err := emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamToolStarted, ToolCall: &publicmodel.OutputFunctionCall{CallID: "incomplete-call", Name: "host_echo", Arguments: "{}"}}); err != nil {
				return nil, err
			}
		}
		return nil, &publicmodel.RequestError{Category: publicmodel.ErrorUnavailable, StatusCode: 503, Code: "overloaded_error", RequestID: "req_fault", RetryAfter: p.retryAfter, Cause: errors.New("secret request body")}
	}
	if emit != nil {
		return p.inner.StreamResponse(ctx, request, profile, emit)
	}
	return p.inner.CreateResponse(ctx, request, profile)
}

func TestModelRecoverySurvivesRestartAndDoesNotReplayTools(t *testing.T) {
	ctx := context.Background()
	hostTool := &hostEchoTool{}
	provider := &faultHostProvider{failAt: 3, retryAfter: 2 * time.Second, streamPartial: true, inner: &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("select-host", "select_tool_category", `{"category":"host"}`),
		hostToolCallResponse("finished-tool", "host_echo", `{}`),
		hostTextResponse("recovered result"), hostTextResponse("next queued result"),
	}}}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	config.Execution.ModelMaxAttempts = 1
	config.Tools = []publictool.Registration{{Tool: hostTool, Metadata: publictool.Metadata{Category: publictool.Category{Name: "host", Description: "Host tools"}, PermissionClass: publictool.PermissionWorkspaceRead, ReadOnly: true}}}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "use the host tool")})
	if !errors.Is(err, ErrTaskPaused) || result.ModelFailure == nil || result.ModelFailure.Attempts != 1 || !result.ModelFailure.StreamStarted {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	firstTask, _ := client.StateForSession(saved.ID)
	runtime, err := client.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := runtime.PendingModelRecovery(ctx)
	if err != nil || !found || pending.ExecutionID != result.ExecutionID {
		t.Fatalf("pending=%+v found=%v error=%v", pending, found, err)
	}
	if _, err := runtime.Recover(ctx); !errors.Is(err, ErrModelRetryNotReady) {
		t.Fatalf("early recovery=%v", err)
	}
	queued, err := runtime.SubmitTurn(ctx, TurnRequest{Input: mustSettingsTaskInput(t, "next question")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = client.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := runtime.WaitTurn(ctx, result.ExecutionID)
	if err != nil || paused.Status != TaskPaused || paused.ModelFailure == nil || paused.ModelFailure.RetryNotBefore != result.ModelFailure.RetryNotBefore {
		t.Fatalf("restart result=%+v error=%v", paused, err)
	}
	requests, err := runtime.PendingRequests(ctx)
	if err != nil || len(requests) != 1 || requests[0].Kind != PendingModelRecoveryRequest {
		t.Fatalf("requests=%+v error=%v", requests, err)
	}
	if delay := time.Until(paused.ModelFailure.RetryNotBefore); delay > 0 {
		time.Sleep(delay + time.Millisecond)
	}
	completed, err := client.ResumeTask(ctx, saved.ID)
	if err != nil || completed.Status != TaskCompleted || completed.ExecutionID != result.ExecutionID || completed.ModelFailure != nil {
		t.Fatalf("completed=%+v error=%v", completed, err)
	}
	secondTask, _ := client.StateForSession(saved.ID)
	if secondTask.TaskID == firstTask.TaskID {
		t.Fatal("recovery reused stream TaskID")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	next, err := runtime.WaitTurn(waitCtx, queued.ExecutionID)
	if err != nil || next.Status != TaskCompleted {
		t.Fatalf("queued=%+v error=%v", next, err)
	}
	hostTool.mu.Lock()
	calls := hostTool.calls
	hostTool.mu.Unlock()
	if calls != 1 {
		t.Fatalf("completed/incomplete tools executed %d times", calls)
	}
	events, err := client.sessions.RuntimeEventsAfter(ctx, saved.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var required, resumed bool
	for _, stored := range events {
		if strings.Contains(string(stored.Payload), "secret request body") || strings.Contains(string(stored.Payload), "incomplete-call") {
			t.Fatal("sensitive payload persisted")
		}
		var event Event
		if err := json.Unmarshal(stored.Payload, &event); err != nil {
			t.Fatal(err)
		}
		required = required || event.Kind == EventModelRecoveryRequired
		resumed = resumed || event.Kind == EventModelRecoveryResumed
	}
	if !required || !resumed {
		t.Fatalf("required=%v resumed=%v", required, resumed)
	}
}

func TestModelMaxAttemptsSettings(t *testing.T) {
	if err := validateExecutionOptions(ExecutionOptions{ModelMaxAttempts: -1}); err == nil {
		t.Fatal("negative attempts accepted")
	}
	if err := validateExecutionOptions(ExecutionOptions{TokenBudget: -1}); err == nil {
		t.Fatal("negative token budget accepted")
	}
	defaults := ExecutionOptions{ModelMaxAttempts: 5, TokenBudget: 500}
	if got := mergeExecutionOptions(defaults, ExecutionOptions{}); got.ModelMaxAttempts != 5 {
		t.Fatal(got)
	}
	if got := coreExecutionOptions(agentExecutionOptions(mergeExecutionOptions(defaults, ExecutionOptions{ModelMaxAttempts: 1, TokenBudget: 1000}))); got.ModelMaxAttempts != 1 || got.TokenBudget != 1000 {
		t.Fatal(got)
	}
	if got := coreExecutionOptions(agentExecutionOptions(mergeExecutionOptions(defaults, ExecutionOptions{}))); got.TokenBudget != 500 {
		t.Fatalf("host token budget default = %d, want 500", got.TokenBudget)
	}
}
