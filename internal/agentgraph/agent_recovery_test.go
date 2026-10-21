package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
	"github.com/afterlune/luneGraph/checkpoint/memory"
	"github.com/afterlune/luneGraph/checkpoint/sqlite"
)

func TestAgentGraphRecoversSelectedCategoryAfterSQLiteStoreReopen(t *testing.T) {
	readFile := &recordingTool{}
	registry, err := tool.NewRegistry(toolRegistration(
		readFile,
		model.ToolCategory{Name: "files", Description: "Workspace files."},
		model.ToolPermissionWorkspaceRead,
		true,
	))
	if err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_1","name":"select_tool_category","arguments":"{\"category\":\"files\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "category-checkpoints.db")
	store, err := sqlite.Open[State](context.Background(), dbPath, checkpoint.JSON[State]{})
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	input, err := model.NewResponseInput("read a file")
	if err != nil {
		t.Fatal(err)
	}
	execution, paused, err := agent.Start(context.Background(), "category-recovery", nil, input, TaskOptions{}, store, graph.Options{MaxSteps: 2})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if paused.Status != graph.StatusBudget {
		t.Fatalf("paused status = %q, want budget checkpoint", paused.Status)
	}
	cp, checkpointErr := execution.Checkpoint()
	if checkpointErr != nil {
		t.Fatal(checkpointErr)
	}
	selectedCategoryPersisted := false
	for _, invocation := range cp.Invocations {
		if contains(invocation.State.SelectedCategories, "files") {
			selectedCategoryPersisted = true
		}
	}
	if !selectedCategoryPersisted {
		t.Fatalf("checkpoint did not retain selected category: %#v", cp.Invocations)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := sqlite.Open[State](context.Background(), dbPath, checkpoint.JSON[State]{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	recoveredAgent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New after reopen: %v", err)
	}
	_, completed, err := recoveredAgent.Recover(context.Background(), "category-recovery", nil, reopened, graph.Options{MaxSteps: 100})
	if err != nil {
		t.Fatalf("Recover after reopen: %v", err)
	}
	if completed.Status != graph.StatusCompleted || completed.Final == nil {
		t.Fatalf("recovered result = %#v, want completed task", completed)
	}
	if readFile.calls != 1 || len(requester.requests) != 3 {
		t.Fatalf("read_file calls=%d, model requests=%d; want one tool call and three requests", readFile.calls, len(requester.requests))
	}
	for _, index := range []int{1, 2} {
		got := strings.Join(toolNames(requester.requests[index].Tools), ",")
		want := model.CategorySelectorName + ",read_file"
		if got != want {
			t.Errorf("request %d visible tools = %q, want %q", index+1, got, want)
		}
	}
}

func TestAgentGraphRecoversAfterCancellation(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	requester := &cancelOnceRequester{started: make(chan struct{})}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input, err := model.NewResponseInput("cancel then resume")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	options := graph.Options{}
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result graph.Result[State]
		err    error
	}, 1)
	go func() {
		_, result, err := agent.Start(ctx, "cancel-poc", nil, input, TaskOptions{}, store, options)
		resultCh <- struct {
			result graph.Result[State]
			err    error
		}{result: result, err: err}
	}()
	<-requester.started
	cancel()
	interrupted := <-resultCh
	if !errors.Is(interrupted.err, context.Canceled) || interrupted.result.Status != graph.StatusCancelled {
		t.Fatalf("interrupted result = %#v, error = %v", interrupted.result, interrupted.err)
	}

	_, recovered, err := agent.Recover(context.Background(), "cancel-poc", nil, store, options)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if recovered.Status != graph.StatusCompleted || requester.calls != 2 {
		t.Fatalf("recovered status=%q requests=%d, want completed after replay", recovered.Status, requester.calls)
	}
}

func TestToolInvocationUsesStableGraphCallIDAcrossReplay(t *testing.T) {
	registered := &namedRecordingTool{name: "replay_tool"}
	registry, err := tool.NewRegistry(toolRegistration(registered, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"model-tool-call","name":"replay_tool","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var mu sync.Mutex
	var toolCallIDs, graphCallIDs []string
	controllerCalls := 0
	registry.SetToolCallController(func(ctx context.Context, _ model.OutputFunctionCall, execute tool.ToolCallExecution) (model.InputFunctionCallOutput, error) {
		mu.Lock()
		toolCallIDs = append(toolCallIDs, publictool.InvocationID(ctx))
		controllerCalls++
		call := controllerCalls
		mu.Unlock()
		if call == 1 {
			close(started)
			<-ctx.Done()
			return model.InputFunctionCallOutput{}, ctx.Err()
		}
		return execute(), nil
	})
	observer := graph.ObserverFunc(func(_ context.Context, event graph.Event) {
		if event.Operation == graph.OperationNode && event.Node == toolNode && event.Phase == graph.PhaseStarted {
			mu.Lock()
			graphCallIDs = append(graphCallIDs, event.CallID)
			mu.Unlock()
		}
	})
	input := model.NewResponseInputText("run and replay a tool")
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result graph.Result[State]
		err    error
	}, 1)
	go func() {
		_, result, runErr := agent.Start(ctx, "stable-tool-call-id", nil, input, TaskOptions{}, store, graph.Options{Observer: observer})
		resultCh <- struct {
			result graph.Result[State]
			err    error
		}{result: result, err: runErr}
	}()
	<-started
	cancel()
	interrupted := <-resultCh
	if !errors.Is(interrupted.err, context.Canceled) || interrupted.result.Status != graph.StatusCancelled {
		t.Fatalf("interrupted tool run = %#v, error=%v", interrupted.result, interrupted.err)
	}
	_, completed, err := agent.Recover(context.Background(), "stable-tool-call-id", nil, store, graph.Options{Observer: observer})
	if err != nil || completed.Status != graph.StatusCompleted {
		t.Fatalf("recovered tool run = %#v, error=%v", completed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(toolCallIDs) != 2 || toolCallIDs[0] == "" || toolCallIDs[0] != toolCallIDs[1] {
		t.Fatalf("tool invocation identities across replay = %v", toolCallIDs)
	}
	if len(graphCallIDs) != 2 || graphCallIDs[0] != graphCallIDs[1] || toolCallIDs[0] != graphCallIDs[0] {
		t.Fatalf("graph callback identities=%v, tool identities=%v", graphCallIDs, toolCallIDs)
	}
	if registered.calls != 1 {
		t.Fatalf("tool side effects = %d, want one after callback replay", registered.calls)
	}
}

func TestAgentGraphRecoversAfterSQLiteStoreReopen(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	requester := &cancelOnceRequester{started: make(chan struct{})}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := sqlite.Open[State](context.Background(), dbPath, checkpoint.JSON[State]{})
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	input, err := model.NewResponseInput([]map[string]any{{
		"type": "message", "role": "user", "content": []map[string]any{
			{"type": "input_text", "text": "persist this task"},
			{"type": "input_audio", "data": "audio-payload"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result graph.Result[State]
		err    error
	}, 1)
	go func() {
		_, result, runErr := agent.Start(ctx, "sqlite-cancel", nil, input, TaskOptions{}, store, graph.Options{})
		resultCh <- struct {
			result graph.Result[State]
			err    error
		}{result: result, err: runErr}
	}()
	<-requester.started
	cancel()
	interrupted := <-resultCh
	if !errors.Is(interrupted.err, context.Canceled) || interrupted.result.Status != graph.StatusCancelled {
		t.Fatalf("interrupted result = %#v, error = %v", interrupted.result, interrupted.err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := sqlite.Open[State](context.Background(), dbPath, checkpoint.JSON[State]{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	recoveredAgent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New after reopen: %v", err)
	}
	_, recovered, err := recoveredAgent.Recover(context.Background(), "sqlite-cancel", nil, reopened, graph.Options{})
	if err != nil {
		t.Fatalf("Recover after reopen: %v", err)
	}
	if recovered.Status != graph.StatusCompleted || recovered.Final == nil || recovered.Final.FinalResponse == nil {
		t.Fatalf("recovered result = %#v, want completed final state", recovered)
	}
	if requester.calls != 2 {
		t.Fatalf("model requests = %d, want original call plus replay", requester.calls)
	}
	encoded, err := json.Marshal(recovered.Final.History)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"persist this task", "input_audio", "audio-payload"} {
		if !strings.Contains(string(encoded), preserved) {
			t.Fatalf("recovered history lost %q: %s", preserved, encoded)
		}
	}
}

type cancelOnceRequester struct {
	started chan struct{}
	calls   int
}

func (r *cancelOnceRequester) CreateResponse(ctx context.Context, _ model.ResponseRequest) (*model.Response, error) {
	r.calls++
	if r.calls == 1 {
		close(r.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return decodeResponse(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}]}`)
}
