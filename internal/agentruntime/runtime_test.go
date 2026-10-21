package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

type graphScriptedRequester struct {
	responses []*model.Response
	requests  []model.ResponseRequest
	errors    []error
}

func TestUsageTrackingRequesterReportsTextInputEstimate(t *testing.T) {
	inner := &graphScriptedRequester{responses: []*model.Response{{
		Status: "completed",
		Output: []model.ResponseOutputItem{model.OutputMessage{
			Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "done"}},
		}},
		Usage: model.ResponseUsage{OutputTokens: 3, TotalTokens: 3},
	}}}
	tracked := &UsageTrackingRequester{
		inner: inner, agentID: "lead", profile: "primary", providerName: "provider-a", modelName: "model-a",
	}
	var observedEstimate int64
	tracked.SetAgentUsageObserver(func(_ model.ResponseUsage, agentID, profile, provider, modelID string, estimate int64) {
		if agentID != "lead" || profile != "primary" || provider != "provider-a" || modelID != "model-a" {
			t.Errorf("usage attribution = %q/%q/%q/%q", agentID, profile, provider, modelID)
		}
		observedEstimate = estimate
	})
	request := model.ModelRequest{
		Instructions: "Follow the project conventions.",
		Input: model.NewResponseInputItems(
			model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("检查项目配置并总结结果")},
			model.InputFunctionCallOutput{CallID: "call-1", Output: model.NewToolOutputText("configuration is valid")},
		),
		Tools: []model.ResponseTool{model.FunctionTool{Name: "read_file", Description: "Read a project file."}},
	}
	if _, err := tracked.CreateResponse(context.Background(), request); err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}
	want := model.EstimateInputTokens(request)
	if want == 0 || observedEstimate != want {
		t.Fatalf("observed estimate = %d, want %d", observedEstimate, want)
	}
}

func (r *graphScriptedRequester) CreateResponse(_ context.Context, request model.ResponseRequest) (*model.Response, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var saved model.ResponseRequest
	if err := json.Unmarshal(encoded, &saved); err != nil {
		return nil, err
	}
	r.requests = append(r.requests, saved)
	if len(r.errors) > 0 {
		err := r.errors[0]
		r.errors = r.errors[1:]
		return nil, err
	}
	if len(r.responses) == 0 {
		return nil, context.Canceled
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response, nil
}

func TestGraphResponderClearsFailedModelTaskAndContinues(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	requester := &graphScriptedRequester{
		errors: []error{errors.New("temporary model error")},
		responses: []*model.Response{
			graphResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"next answer"}]}]}`),
		},
	}
	agent, err := agentgraph.New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[agentgraph.State](graph.JSONClone[agentgraph.State])
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Responder{agent: agent, store: store}
	failedInput, err := model.NewResponseInput("will fail")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Respond(context.Background(), failedInput); err == nil || !strings.Contains(err.Error(), "temporary model error") {
		t.Fatalf("first Respond error = %v, want temporary model error", err)
	}
	if pending, err := runtime.HasPending(); err != nil || pending {
		t.Fatalf("pending after model failure = %v, error = %v", pending, err)
	}
	nextInput, err := model.NewResponseInput("next task")
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtime.Respond(context.Background(), nextInput)
	if err != nil {
		t.Fatalf("second Respond: %v", err)
	}
	if responseText(response) != "next answer" {
		t.Fatalf("response = %q, want next answer", responseText(response))
	}
}

func TestGraphResponderCarriesHistoryBetweenCompletedTasks(t *testing.T) {
	registry, err := tool.NewDefaultRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	requester := &graphScriptedRequester{responses: []*model.Response{
		graphResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_web","name":"select_tool_category","arguments":"{\"category\":\"web\"}"}]}`),
		graphResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}]}`),
		graphResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second answer"}]}]}`),
	}}
	agent, err := agentgraph.New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[agentgraph.State](graph.JSONClone[agentgraph.State])
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Responder{agent: agent, store: store}
	firstInput, err := model.NewResponseInput("first question")
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.Respond(context.Background(), firstInput)
	if err != nil {
		t.Fatalf("first Respond: %v", err)
	}
	if responseText(first) != "first answer" {
		t.Fatalf("first response = %q", responseText(first))
	}
	if pending, err := runtime.HasPending(); err != nil || pending {
		t.Fatalf("pending after completed first task = %v, error = %v", pending, err)
	}
	secondInput, err := model.NewResponseInput("second question")
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.Respond(context.Background(), secondInput)
	if err != nil {
		t.Fatalf("second Respond: %v", err)
	}
	if responseText(second) != "second answer" {
		t.Fatalf("second response = %q", responseText(second))
	}
	if len(requester.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(requester.requests))
	}
	toolNames := func(request model.ResponseRequest) map[string]bool {
		names := make(map[string]bool, len(request.Tools))
		for _, definition := range request.Tools {
			if function, ok := definition.(model.FunctionTool); ok {
				names[function.Name] = true
			}
		}
		return names
	}
	initialTools := toolNames(requester.requests[0])
	for _, name := range []string{"read_file", "list_dir", "glob", "grep", "write_file", "edit_file", "command", model.CategorySelectorName} {
		if !initialTools[name] {
			t.Errorf("first task initial tools = %#v, missing %q", initialTools, name)
		}
	}
	if initialTools["web_search"] || initialTools["web_fetch"] {
		t.Fatalf("first task exposed deferred web tools: %#v", initialTools)
	}
	selectedTools := toolNames(requester.requests[1])
	if !selectedTools["read_file"] || !selectedTools["web_search"] || !selectedTools["web_fetch"] {
		t.Fatalf("web category did not preserve common tools and expose web tools: %#v", selectedTools)
	}
	newTaskTools := toolNames(requester.requests[2])
	for _, name := range []string{"read_file", "list_dir", "glob", "grep", "write_file", "edit_file", "command", model.CategorySelectorName} {
		if !newTaskTools[name] {
			t.Errorf("second task initial tools = %#v, missing %q", newTaskTools, name)
		}
	}
	if newTaskTools["web_search"] || newTaskTools["web_fetch"] {
		t.Fatalf("second task inherited deferred category selection: %#v", newTaskTools)
	}
	encoded, err := json.Marshal(requester.requests[2].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"first question", "first answer", "second question"} {
		if !strings.Contains(string(encoded), content) {
			t.Errorf("second request history lost %q: %s", content, encoded)
		}
	}
}

func TestGraphResponderReportsUsageForEveryModelRound(t *testing.T) {
	requester := &graphScriptedRequester{responses: []*model.Response{
		{
			Status: "completed",
			Output: []model.ResponseOutputItem{model.OutputFunctionCall{CallID: "call-1", Name: "unknown_tool", Arguments: `{}`}},
			Usage:  model.ResponseUsage{InputTokens: 10, InputTokensDetails: model.TokenInputDetails{CachedTokens: 2}, OutputTokens: 3, TotalTokens: 13},
		},
		{
			Status: "completed",
			Output: []model.ResponseOutputItem{model.OutputMessage{
				Role:    model.RoleAssistant,
				Content: []model.OutputContentPart{model.OutputText{Text: "done"}},
			}},
			Usage: model.ResponseUsage{
				InputTokens:         20,
				InputTokensDetails:  model.TokenInputDetails{CachedTokens: 5},
				OutputTokens:        4,
				OutputTokensDetails: model.TokenOutputDetails{ReasoningTokens: 1},
				TotalTokens:         24,
			},
		},
	}}
	runtime, _ := newCompactionRuntime(t, requester)
	var observed []model.ResponseUsage
	runtime.SetUsageObserver(func(usage model.ResponseUsage) { observed = append(observed, usage) })
	input, err := model.NewResponseInput("run a tool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Respond(context.Background(), input); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if len(observed) != 2 || observed[0].TotalTokens != 13 || observed[1].TotalTokens != 24 {
		t.Fatalf("observed usage = %#v, want usage from both model rounds", observed)
	}
}

func TestGraphResponderCompactSummarizesOlderTurnsAndKeepsLatestFour(t *testing.T) {
	requester := &graphScriptedRequester{responses: []*model.Response{{
		Status: "completed",
		Output: []model.ResponseOutputItem{model.OutputMessage{
			Role:    model.RoleAssistant,
			Content: []model.OutputContentPart{model.OutputText{Text: "summary of older turns"}},
		}},
		Usage: model.ResponseUsage{
			InputTokens:         30,
			InputTokensDetails:  model.TokenInputDetails{CachedTokens: 10},
			OutputTokens:        8,
			OutputTokensDetails: model.TokenOutputDetails{ReasoningTokens: 4},
			TotalTokens:         38,
		},
	}}}
	runtime, tracked := newCompactionRuntime(t, requester)
	items := make([]model.InputItem, 0, 24)
	for turn := 1; turn <= 6; turn++ {
		content := model.NewMessageText(fmt.Sprintf("user-%d", turn))
		if turn == 6 {
			content = model.NewMessageContentParts(
				model.InputText{Text: "user-6"},
				model.InputImage{ImageURL: "data:image/png;base64,AA=="},
				model.UnknownContentPart{Raw: json.RawMessage(`{"type":"input_audio","data":"audio-tail"}`)},
			)
		}
		items = append(items,
			model.InputMessage{Role: model.RoleUser, Content: content},
			model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText(fmt.Sprintf("assistant-%d", turn))},
		)
		if turn >= 3 {
			items = append(items,
				model.InputFunctionCall{CallID: fmt.Sprintf("call-%d", turn), Name: "read_file", Arguments: `{}`},
				model.InputFunctionCallOutput{CallID: fmt.Sprintf("call-%d", turn), Output: model.NewToolOutputText(fmt.Sprintf("tool-result-%d", turn))},
			)
		}
	}
	runtime.history = model.NewResponseInputItems(items...)
	var observed []model.ResponseUsage
	runtime.SetUsageObserver(func(usage model.ResponseUsage) { observed = append(observed, usage) })

	compactedTurns, err := runtime.Compact(context.Background())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if compactedTurns != 2 {
		t.Fatalf("compacted turns = %d, want 2", compactedTurns)
	}
	if len(requester.requests) != 1 {
		t.Fatalf("summary requests = %d, want 1", len(requester.requests))
	}
	request := requester.requests[0]
	if request.Model != model.DeepSeekFlashModel || len(request.Tools) != 0 || request.Input == nil {
		t.Fatalf("summary request = %#v, want model request without tools", request)
	}
	requestJSON, err := json.Marshal(request.Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(requestJSON), "user-1") || !strings.Contains(string(requestJSON), "user-2") || strings.Contains(string(requestJSON), "user-3") {
		t.Fatalf("summary request did not contain only the older prefix: %s", requestJSON)
	}
	if len(observed) != 1 || observed[0].TotalTokens != 38 || tracked.observer == nil {
		t.Fatalf("usage observer values = %#v", observed)
	}

	compactedJSON, err := json.Marshal(runtime.history)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"summary of older turns", "user-3", "user-4", "user-5", "user-6", "tool-result-3", "tool-result-6", "audio-tail"} {
		if !strings.Contains(string(compactedJSON), preserved) {
			t.Errorf("compacted history lost %q: %s", preserved, compactedJSON)
		}
	}
	for _, removed := range []string{"user-1", "user-2", "assistant-1", "assistant-2"} {
		if strings.Contains(string(compactedJSON), removed) {
			t.Errorf("compacted history retained old detail %q: %s", removed, compactedJSON)
		}
	}
}

func TestGraphResponderCompactNoOpAndFailurePreserveHistory(t *testing.T) {
	t.Run("no older turns", func(t *testing.T) {
		requester := &graphScriptedRequester{}
		runtime, _ := newCompactionRuntime(t, requester)
		runtime.history = model.NewResponseInputItems(model.InputMessage{
			Role: model.RoleUser, Content: model.NewMessageText("recent only"),
		})
		compactedTurns, err := runtime.Compact(context.Background())
		if err != nil || compactedTurns != 0 || len(requester.requests) != 0 {
			t.Fatalf("Compact = (%d, %v), requests=%d; want no-op", compactedTurns, err, len(requester.requests))
		}
	})

	for _, test := range []struct {
		name      string
		requester *graphScriptedRequester
	}{
		{name: "request error", requester: &graphScriptedRequester{errors: []error{errors.New("temporary error")}}},
		{name: "empty summary", requester: &graphScriptedRequester{responses: []*model.Response{{Status: "completed"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := newCompactionRuntime(t, test.requester)
			items := make([]model.InputItem, 0, 5)
			for turn := 1; turn <= 5; turn++ {
				items = append(items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(fmt.Sprintf("user-%d", turn))})
			}
			runtime.history = model.NewResponseInputItems(items...)
			before, err := json.Marshal(runtime.history)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Compact(context.Background()); err == nil {
				t.Fatal("Compact succeeded, want an error")
			}
			after, err := json.Marshal(runtime.history)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("failed compaction changed history: before=%s after=%s", before, after)
			}
		})
	}
}

func newCompactionRuntime(t *testing.T, requester *graphScriptedRequester) (*Responder, *UsageTrackingRequester) {
	t.Helper()
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	tracked := &UsageTrackingRequester{inner: requester}
	agent, err := agentgraph.New(tracked, registry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[agentgraph.State](graph.JSONClone[agentgraph.State])
	if err != nil {
		t.Fatal(err)
	}
	return &Responder{agent: agent, store: store, requester: tracked}, tracked
}

func graphResponse(t *testing.T, raw string) *model.Response {
	t.Helper()
	var response model.Response
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	return &response
}

func responseText(response *model.Response) string {
	if response == nil {
		return ""
	}
	var text strings.Builder
	for _, item := range response.Output {
		message, ok := item.(model.OutputMessage)
		if !ok {
			continue
		}
		for _, part := range message.Content {
			if output, ok := part.(model.OutputText); ok {
				text.WriteString(output.Text)
			}
		}
	}
	return text.String()
}
