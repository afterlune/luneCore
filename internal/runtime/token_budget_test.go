package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
)

type executionBudgetProvider struct {
	mu                           sync.Mutex
	calls                        map[string]int
	seen                         []publicmodel.ModelRequest
	incompleteFirstModelResponse bool
}

func (p *executionBudgetProvider) Name() string { return "host" }

func (p *executionBudgetProvider) CreateResponse(_ context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	stage := executionBudgetStage(request)
	p.mu.Lock()
	if p.calls == nil {
		p.calls = make(map[string]int)
	}
	p.calls[stage]++
	p.seen = append(p.seen, request)
	callNumber := p.calls[stage]
	p.mu.Unlock()

	var response *publicmodel.ModelResponse
	switch stage {
	case "model":
		if p.incompleteFirstModelResponse && callNumber == 1 && request.MaxOutputTokens != nil && *request.MaxOutputTokens < profile.MaxOutputTokens {
			input := model.EstimateInputTokens(request)
			return &publicmodel.ModelResponse{
				Status: "incomplete", IncompleteDetails: &publicmodel.IncompleteDetails{Reason: "max_output_tokens"},
				Usage: publicmodel.ResponseUsage{InputTokens: input, OutputTokens: 1, TotalTokens: input + 1},
			}, nil
		}
		if callNumber == 1 {
			response = hostToolCallResponse("budget-select", "select_tool_category", `{"category":"files"}`)
			input := model.EstimateInputTokens(request)
			response.Usage = publicmodel.ResponseUsage{InputTokens: input, OutputTokens: 100_000 - input, TotalTokens: 100_000}
		} else {
			response = hostTextResponse("The requested task is complete.")
		}
	case "memory":
		response = &publicmodel.ModelResponse{Status: "completed", Output: []publicmodel.ResponseOutputItem{
			publicmodel.OutputFunctionCall{CallID: "memory-empty", Name: "propose_workspace_memories", Arguments: `{"items":[]}`},
		}}
	default:
		response = hostTextResponse("The requested task is complete.")
	}
	if response.Usage.TotalTokens == 0 {
		input := model.EstimateInputTokens(request)
		response.Usage = publicmodel.ResponseUsage{InputTokens: input, OutputTokens: 10, TotalTokens: input + 10}
	}
	return response, nil
}

func (p *executionBudgetProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, _ func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	return p.CreateResponse(ctx, request, profile)
}

func executionBudgetStage(request publicmodel.ModelRequest) string {
	for _, definition := range request.Tools {
		function, ok := definition.(publicmodel.FunctionTool)
		if !ok {
			continue
		}
		switch function.Name {
		case "propose_workspace_memories":
			return "memory"
		}
	}
	return "model"
}

func TestExecutionTokenBudgetPersistsAcrossPauseAndRecovery(t *testing.T) {
	provider := &executionBudgetProvider{}
	stateDir := t.TempDir()
	config := runtimeTestConfig(t.TempDir(), stateDir, provider)
	config.Memory = MemoryConfig{}
	config.ModelConfig.Profiles["host"] = publicmodel.ModelProfile{Provider: "host", Model: "test-model", MaxOutputTokens: 2048}
	client, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("implement the current request")
	if err != nil {
		t.Fatal(err)
	}
	paused, err := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: input, Execution: ExecutionOptions{TokenBudget: 50_000}})
	if !errors.Is(err, ErrTaskPaused) || paused.Status != TaskPaused || paused.BudgetReason != BudgetMaxTokens {
		t.Fatalf("initial result = %#v, error=%v; want token budget pause", paused, err)
	}
	if paused.ExecutionID == "" {
		t.Fatal("paused task has no execution id")
	}
	provider.mu.Lock()
	initialCalls := provider.calls["model"] + provider.calls["memory"]
	provider.mu.Unlock()
	if initialCalls != 1 {
		t.Fatalf("model calls before first pause = %d, want one completed task-tool call", initialCalls)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("close before recovery: %v", err)
	}
	client, err = New(config)
	if err != nil {
		t.Fatalf("reopen Core before recovery: %v", err)
	}

	stillPaused, err := client.ResumeTask(ctx, saved.ID, ExecutionOptions{TokenBudget: 100_010})
	if !errors.Is(err, ErrTaskPaused) || stillPaused.Status != TaskPaused || stillPaused.BudgetReason != BudgetMaxTokens || stillPaused.ExecutionID != paused.ExecutionID {
		t.Fatalf("resume below spent usage = %#v, error=%v; want same execution paused", stillPaused, err)
	}
	provider.mu.Lock()
	if provider.calls["model"] != 1 {
		provider.mu.Unlock()
		t.Fatalf("recovery repeated completed work or called with exhausted budget: calls=%v", provider.calls)
	}
	provider.mu.Unlock()

	completed, err := client.ResumeTask(ctx, saved.ID, ExecutionOptions{TokenBudget: 200_000})
	if err != nil || completed.Status != TaskCompleted || completed.ExecutionID != paused.ExecutionID {
		t.Fatalf("raised-budget result = %#v, error=%v", completed, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.calls["model"] != 2 {
		t.Fatalf("execution model calls = %v; want two lead requests", provider.calls)
	}
	for _, request := range provider.seen {
		if request.MaxOutputTokens == nil || *request.MaxOutputTokens != 2048 {
			t.Fatalf("model request output cap = %v, want configured profile cap", request.MaxOutputTokens)
		}
	}
	var budgetRows int
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM execution_token_budgets WHERE execution_id = ?`, paused.ExecutionID).Scan(&budgetRows); err != nil {
		t.Fatal(err)
	}
	if budgetRows != 0 {
		t.Fatalf("terminal execution retained %d budget rows", budgetRows)
	}
}

func TestForkPausedTaskStartsWithFreshTokenBudget(t *testing.T) {
	provider := &executionBudgetProvider{}
	stateDir := t.TempDir()
	config := runtimeTestConfig(t.TempDir(), stateDir, provider)
	config.ModelConfig.Profiles["host"] = publicmodel.ModelProfile{Provider: "host", Model: "test-model", MaxOutputTokens: 2048}
	client, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("implement the current request")
	if err != nil {
		t.Fatal(err)
	}
	paused, err := client.RunTask(ctx, TaskRequest{SessionID: source.ID, Input: input, Execution: ExecutionOptions{TokenBudget: 50_000}})
	if !errors.Is(err, ErrTaskPaused) || paused.Status != TaskPaused || paused.BudgetReason != BudgetMaxTokens {
		t.Fatalf("source result = %#v, error=%v; want token budget pause", paused, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.mu.RLock()
		draining := client.queueDraining[source.ID]
		client.mu.RUnlock()
		if !draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue drain did not finish after the task paused")
		}
		time.Sleep(time.Millisecond)
	}
	forked, err := client.ForkPausedTask(ctx, source.ID)
	if err != nil {
		t.Fatalf("ForkPausedTask: %v", err)
	}
	completed, err := client.ResumeTask(ctx, forked.ID)
	if err != nil || completed.Status != TaskCompleted || completed.ExecutionID == paused.ExecutionID {
		t.Fatalf("fork result = %#v, error=%v; want completed fresh execution", completed, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.calls["model"] != 2 {
		t.Fatalf("fork repeated completed work or exhausted inherited usage: calls=%v", provider.calls)
	}
}

func TestDiscardPausedTaskClearsTokenBudget(t *testing.T) {
	provider := &scriptedHostProvider{}
	stateDir := t.TempDir()
	client, err := New(runtimeTestConfig(t.TempDir(), stateDir, provider))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("answer the current request")
	if err != nil {
		t.Fatal(err)
	}
	paused, err := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: input, Execution: ExecutionOptions{TokenBudget: 1}})
	if !errors.Is(err, ErrTaskPaused) || paused.Status != TaskPaused || paused.BudgetReason != BudgetMaxTokens {
		t.Fatalf("paused result = %#v, error=%v", paused, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.mu.RLock()
		draining := client.queueDraining[saved.ID]
		client.mu.RUnlock()
		if !draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue drain did not finish after the task paused")
		}
		time.Sleep(time.Millisecond)
	}
	runtime, err := client.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if discarded, err := runtime.DiscardPaused(ctx); err != nil || !discarded {
		t.Fatalf("DiscardPaused = %v, error=%v", discarded, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var budgets int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM execution_token_budgets WHERE execution_id = ?`, paused.ExecutionID).Scan(&budgets); err != nil {
		t.Fatal(err)
	}
	if budgets != 0 {
		t.Fatalf("discarded task retained %d token budget rows", budgets)
	}
}

func TestBudgetCappedIncompleteResponsePausesTask(t *testing.T) {
	provider := &executionBudgetProvider{incompleteFirstModelResponse: true}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	config.ModelConfig.Profiles["host"] = publicmodel.ModelProfile{Provider: "host", Model: "test-model", MaxOutputTokens: 50_000}
	client, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("plan the current request")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: input, Execution: ExecutionOptions{TokenBudget: 10_000}})
	if !errors.Is(err, ErrTaskPaused) || result.Status != TaskPaused || result.BudgetReason != BudgetMaxTokens {
		t.Fatalf("incomplete response result = %#v, error=%v; want token budget pause", result, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.calls["model"] != 1 || provider.calls["memory"] != 0 {
		t.Fatalf("incomplete model call advanced the task: %v", provider.calls)
	}
	request := provider.seen[0]
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens >= 50_000 {
		t.Fatalf("first request was not output-capped by the remaining budget: %v", request.MaxOutputTokens)
	}
}

type concurrentBudgetRequester struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (r *concurrentBudgetRequester) CreateResponse(ctx context.Context, _ model.ModelRequest) (*model.ModelResponse, error) {
	r.calls.Add(1)
	r.entered <- struct{}{}
	select {
	case <-r.release:
		return &model.ModelResponse{Status: "completed", Usage: publicmodel.ResponseUsage{TotalTokens: 5}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConcurrentAgentsShareExecutionTokenReservations(t *testing.T) {
	store, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	request := model.ModelRequest{Input: model.NewResponseInputText("independent branch request")}
	limit := model.EstimateInputTokens(request) + 50
	if err := store.SetExecutionTokenBudget(context.Background(), "session", "execution", limit); err != nil {
		t.Fatal(err)
	}
	inner := &concurrentBudgetRequester{entered: make(chan struct{}, 2), release: make(chan struct{})}
	requester := agentruntime.NewUsageTrackingRequester(inner, "worker", "profile", "provider", "model")
	requester.SetMaxOutputTokens(50)
	ctx := agentruntime.WithTokenBudget(context.Background(), &executionTokenBudget{store: store, executionID: "execution"})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := requester.CreateResponse(ctx, request)
			results <- err
		}()
	}
	select {
	case <-inner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first branch did not enter the provider")
	}
	var secondErr error
	select {
	case secondErr = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("second branch did not reach the shared reservation")
	}
	if !errors.Is(secondErr, model.ErrTokenBudgetExceeded) {
		t.Fatalf("second branch error = %v, want shared token budget exhaustion", secondErr)
	}
	close(inner.release)
	if err := <-results; err != nil {
		t.Fatalf("reserved branch error = %v", err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want one shared-budget reservation", got)
	}
}

func TestOlderCheckpointKeepsItsVersionAndUnlimitedTokenBudget(t *testing.T) {
	var state agentgraph.State
	if err := json.Unmarshal([]byte(`{"task_settings_version":9,"execution":{"max_steps":3}}`), &state); err != nil {
		t.Fatal(err)
	}
	if state.TaskSettingsVersion != 9 || state.Execution.MaxSteps != 3 || state.Execution.TokenBudget != 0 {
		t.Fatalf("legacy task settings decoded as %#v", state)
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
		if err := client.Close(context.Background()); err != nil {
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
