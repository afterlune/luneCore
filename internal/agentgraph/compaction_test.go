package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

func TestAutomaticCompactionClearsOldToolOutputAndKeepsTranscript(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	largeResult := strings.Repeat("old-tool-result ", 3000)
	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("old request")},
		model.UnknownInputItem{Raw: json.RawMessage(`{"type":"function_call","call_id":"call-old","name":"read_file","arguments":"{}"}`)},
		model.InputFunctionCallOutput{Status: "completed", CallID: "call-old", Output: model.NewToolOutputParts(
			model.OutputText{Text: largeResult}, model.InputImage{ImageURL: "data:image/png;base64,AA=="},
		)},
	)
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{ID: "lead", Profile: "main", Provider: "vendor", Protocol: model.ProtocolAnthropicMessages, Model: "claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("continue the task")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.StartWithTranscript(context.Background(), "auto-compact-clear-tool-output", history, history, input,
		TaskOptions{ContextWindow: 2400, ReserveOutputTokens: 100, AutoCompact: true, AutoCompactThreshold: 0.8},
		store, graph.Options{})
	if err != nil {
		t.Fatalf("StartWithTranscript: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v", result)
	}
	if len(requester.requests) != 1 {
		t.Fatalf("model requests = %d, want one after clearing the tool result", len(requester.requests))
	}
	requestJSON, err := json.Marshal(requester.requests[0].Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(requestJSON), "call-old") || !strings.Contains(string(requestJSON), "Earlier tool output cleared") {
		t.Fatalf("request lost the tool pair or clearing marker: %s", requestJSON)
	}
	var compactedInput model.ResponseInput
	if err := json.Unmarshal(requestJSON, &compactedInput); err != nil {
		t.Fatal(err)
	}
	var preservedStatus bool
	for _, item := range compactedInput.Items {
		if output, ok := item.(model.InputFunctionCallOutput); ok && output.CallID == "call-old" {
			preservedStatus = output.Status == "completed"
		}
	}
	if !preservedStatus {
		t.Fatal("tool output status was not preserved")
	}
	if strings.Contains(string(requestJSON), largeResult) {
		t.Fatal("compacted model context still contains the large tool output")
	}
	if !strings.Contains(string(requestJSON), "data:image/png;base64,AA==") {
		t.Fatal("tool-result compaction removed a non-text image part")
	}
	transcriptJSON, err := json.Marshal(result.Final.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcriptJSON), largeResult) {
		t.Fatal("canonical transcript lost the original tool output")
	}
}

func TestAutomaticCompactionSummarizesOlderHistoryWhenToolClearingIsInsufficient(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	older := strings.Repeat("project requirement ", 1000)
	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("initial request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText(older)},
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("previous follow-up")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText("previous answer")},
	)
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The user is continuing the established project task."}]}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continued"}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{ID: "lead", Profile: "main", Provider: "anthropic", Protocol: model.ProtocolAnthropicMessages, Model: "claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("continue with the next change")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.StartWithTranscript(context.Background(), "auto-compact-summary", history, history, input,
		TaskOptions{ContextWindow: 2400, ReserveOutputTokens: 100, AutoCompact: true, AutoCompactThreshold: 0.8},
		store, graph.Options{})
	if err != nil {
		t.Fatalf("StartWithTranscript: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v", result)
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want one summary and one task request", len(requester.requests))
	}
	if !strings.Contains(requester.requests[0].Instructions, "key code facts") {
		t.Fatalf("summary instructions = %q", requester.requests[0].Instructions)
	}
	contextJSON, err := json.Marshal(result.Final.History)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contextJSON), older) || !strings.Contains(string(contextJSON), "established project task") {
		t.Fatalf("model context was not summarized correctly: %s", contextJSON)
	}
	transcriptJSON, err := json.Marshal(result.Final.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcriptJSON), older) || !strings.Contains(string(transcriptJSON), "continue with the next change") {
		t.Fatal("automatic compaction changed the canonical transcript")
	}
}

func TestResponsesServerCompactionKeepsOpaqueStateOutOfTranscript(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("earlier user turn")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText("old provider context must be pruned")},
	)
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"compaction","id":"cmp-1","encrypted_content":"opaque-context"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Profile: "openai", Provider: "openai", Protocol: model.ProtocolResponses,
		ServerCompaction: true, Model: "gpt-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("long task")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.StartWithTranscript(context.Background(), "auto-compact-server", history, history, input,
		TaskOptions{ContextWindow: 2100, ReserveOutputTokens: 100, AutoCompact: true, AutoCompactThreshold: 0.8},
		store, graph.Options{})
	if err != nil {
		t.Fatalf("StartWithTranscript: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v", result)
	}
	if len(requester.requests) != 1 || len(requester.requests[0].ContextManagement) != 1 || requester.requests[0].ContextManagement[0].CompactThreshold != 1600 {
		t.Fatalf("server compaction request = %#v", requester.requests)
	}
	modelContext, err := json.Marshal(result.Final.History)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := json.Marshal(result.Final.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(modelContext), "opaque-context") || strings.Contains(string(transcript), "opaque-context") {
		t.Fatalf("opaque compaction state was not isolated: model=%s transcript=%s", modelContext, transcript)
	}
	if strings.Contains(string(modelContext), "old provider context") || !strings.Contains(string(transcript), "old provider context") {
		t.Fatalf("latest server state did not prune only the model context: model=%s transcript=%s", modelContext, transcript)
	}
}

func TestCanonicalTranscriptOmitsProviderSpecificOutputItems(t *testing.T) {
	transcript, err := appendTranscriptOutput(model.ResponseInput{}, []model.ResponseOutputItem{
		model.OutputReasoning{
			Content:          []model.ReasoningText{{Text: "private provider reasoning"}},
			EncryptedContent: "encrypted-reasoning-state",
			Signature:        "provider-signature",
		},
		model.OutputCompaction{EncryptedContent: "opaque-compaction-state"},
		model.OutputCustomToolCall{CallID: "custom-1", Name: "vendor_custom", Input: "{}"},
		model.UnknownOutputItem{Raw: json.RawMessage(`{"type":"web_search_call","id":"search-1"}`)},
		model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "portable answer"}}},
		model.OutputFunctionCall{CallID: "call-1", Name: "read_file", Arguments: `{"path":"a.go"}`},
	})
	if err != nil {
		t.Fatalf("appendTranscriptOutput: %v", err)
	}
	encoded, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	for _, providerState := range []string{
		"private provider reasoning", "encrypted-reasoning-state", "provider-signature",
		"opaque-compaction-state", "vendor_custom", "web_search_call",
	} {
		if strings.Contains(string(encoded), providerState) {
			t.Errorf("canonical transcript contains non-portable output %q: %s", providerState, encoded)
		}
	}
	for _, portable := range []string{"portable answer", "read_file", "call-1"} {
		if !strings.Contains(string(encoded), portable) {
			t.Errorf("canonical transcript dropped portable output %q: %s", portable, encoded)
		}
	}
}

func TestResponsesServerCompactionFallsBackWhenThresholdIsBelowEndpointMinimum(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	var events []ContextCompactionEvent
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Profile: "openai", Provider: "openai", Protocol: model.ProtocolResponses,
		ServerCompaction: true, Model: "gpt-test",
		CompactionObserver: func(event ContextCompactionEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.StartWithTranscript(context.Background(), "auto-compact-small-threshold", nil, nil,
		model.NewResponseInputText("short task"),
		TaskOptions{ContextWindow: 1200, ReserveOutputTokens: 100, AutoCompact: true, AutoCompactThreshold: 0.8},
		store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("StartWithTranscript result=%#v error=%v", result, err)
	}
	if len(requester.requests) != 1 || len(requester.requests[0].ContextManagement) != 0 {
		t.Fatalf("server compaction request = %#v, want local fallback", requester.requests)
	}
	if !result.Final.ServerCompactionFallback {
		t.Fatal("task did not retain local fallback for an unsupported server threshold")
	}
	if len(events) == 0 || events[0].Stage != "fallback" || events[0].Threshold != 880 {
		t.Fatalf("fallback events = %#v", events)
	}
}

type contextManagementRejectRequester struct {
	inner    *scriptedRequester
	requests []model.ModelRequest
}

func (r *contextManagementRejectRequester) CreateResponse(ctx context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	r.requests = append(r.requests, request)
	if len(request.ContextManagement) > 0 {
		return nil, errors.New("provider rejected context_management as an unsupported parameter")
	}
	return r.inner.CreateResponse(ctx, request)
}

func TestResponsesServerCompactionFallsBackOnlyWhenEndpointRejectsCapability(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	old := strings.Repeat("preserved project decision ", 800)
	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("initial request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText(old)},
	)
	requester := &contextManagementRejectRequester{inner: &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary retains the key decision"}]}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Profile: "openai", Provider: "openai", Protocol: model.ProtocolResponses,
		ServerCompaction: true, Model: "gpt-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("continue the work")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.StartWithTranscript(context.Background(), "auto-compact-fallback", history, history, input,
		TaskOptions{ContextWindow: 1700, ReserveOutputTokens: 100, AutoCompact: true, AutoCompactThreshold: 0.8},
		store, graph.Options{})
	if err != nil {
		t.Fatalf("StartWithTranscript: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v", result)
	}
	if len(requester.requests) != 3 || len(requester.requests[0].ContextManagement) != 1 || len(requester.requests[1].ContextManagement) != 0 || len(requester.requests[2].ContextManagement) != 0 {
		t.Fatalf("requests after capability fallback = %#v", requester.requests)
	}
	if !result.Final.ServerCompactionFallback {
		t.Fatal("task did not retain endpoint fallback for later model requests")
	}
}

type failFirstRequester struct {
	inner *scriptedRequester
	err   error
	calls int
}

func (r *failFirstRequester) CreateResponse(ctx context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	r.calls++
	if r.calls == 1 {
		return nil, r.err
	}
	return r.inner.CreateResponse(ctx, request)
}

func TestAutomaticCompactionFailureLeavesBothHistoriesUnchanged(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	large := strings.Repeat("old context ", 1000)
	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("old request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText(large)},
	)
	requester := &failFirstRequester{
		inner: &scriptedRequester{},
		err:   errors.New("summary provider unavailable"),
	}
	agent, err := NewWithOptions(requester, registry, AgentOptions{ID: "lead", Profile: "main", Provider: "anthropic", Protocol: model.ProtocolAnthropicMessages, Model: "claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("new request")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.StartWithTranscript(context.Background(), "auto-compact-failure", history, history, input,
		TaskOptions{ContextWindow: 1000, ReserveOutputTokens: 100, AutoCompact: true, AutoCompactThreshold: 0.8},
		store, graph.Options{})
	if err != nil {
		t.Fatalf("StartWithTranscript: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v", result)
	}
	final := result.Final
	if final.TaskError == "" || !strings.Contains(final.TaskError, "summary provider unavailable") {
		t.Fatalf("task error = %q", final.TaskError)
	}
	if !strings.Contains(string(mustMarshal(t, final.History)), large) || !strings.Contains(string(mustMarshal(t, final.Transcript)), large) {
		t.Fatal("failed compaction partially changed model context or transcript")
	}
}

func TestAutomaticCompactionWithoutContextWindowSkipsAndSignalsOnce(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	var events []ContextCompactionEvent
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Profile: "main", Provider: "vendor", Protocol: model.ProtocolResponses,
		CompactionObserver: func(event ContextCompactionEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("hello")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.StartWithTranscript(context.Background(), "auto-compact-unknown-window", nil, nil, input,
		TaskOptions{AutoCompact: true, AutoCompactThreshold: 0.8}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted {
		t.Fatalf("StartWithTranscript result=%#v error=%v", result, err)
	}
	if len(requester.requests) != 1 || len(requester.requests[0].ContextManagement) != 0 {
		t.Fatalf("request with unknown window = %#v", requester.requests)
	}
	if len(events) != 1 || events[0].Stage != "skipped" || events[0].Error == nil || !strings.Contains(events[0].Error.Error(), "contextWindow is unset") {
		t.Fatalf("compaction events = %#v", events)
	}
}

func TestAutomaticCompactionCancellationLeavesResumableUnmodifiedContext(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	old := strings.Repeat("preserve this context ", 1000)
	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("initial request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText(old)},
	)
	requester := &cancelOnceRequester{started: make(chan struct{})}
	agent, err := NewWithOptions(requester, registry, AgentOptions{ID: "lead", Profile: "main", Provider: "anthropic", Protocol: model.ProtocolAnthropicMessages, Model: "claude-test"})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("new user request")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type runResult struct {
		execution *graph.Execution[State]
		result    graph.Result[State]
		err       error
	}
	finished := make(chan runResult, 1)
	go func() {
		execution, result, err := agent.StartWithTranscript(ctx, "auto-compact-cancel", history, history, input,
			TaskOptions{ContextWindow: 1000, ReserveOutputTokens: 100, AutoCompact: true, AutoCompactThreshold: 0.8},
			store, graph.Options{})
		finished <- runResult{execution: execution, result: result, err: err}
	}()
	<-requester.started
	cancel()
	completed := <-finished
	if !errors.Is(completed.err, context.Canceled) {
		t.Fatalf("StartWithTranscript error = %v, want cancellation", completed.err)
	}
	var state State
	found := false
	checkpoint, err := completed.execution.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	for index := len(checkpoint.Invocations) - 1; index >= 0; index-- {
		invocation := checkpoint.Invocations[index]
		if invocation.Status == graph.InvocationEnded || invocation.Status == graph.InvocationFailed || invocation.Status == graph.InvocationJoined {
			continue
		}
		state, found = invocation.State, true
		break
	}
	if !found {
		t.Fatalf("canceled result has no pending invocation: %#v", checkpoint)
	}
	if !strings.Contains(string(mustMarshal(t, state.History)), old) || strings.Contains(string(mustMarshal(t, state.History)), "Earlier conversation summary") {
		t.Fatal("cancelled compaction partially replaced the model context")
	}
	if !strings.Contains(string(mustMarshal(t, state.Transcript)), "new user request") {
		t.Fatal("checkpoint lost the current user request")
	}
}
