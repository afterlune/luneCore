package agentgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

func TestAgentGraphExecutesMultipleToolCallsFromOneResponse(t *testing.T) {
	readFile := &namedRecordingTool{name: "read_file"}
	writeFile := &namedRecordingTool{name: "write_file"}
	registry, err := tool.NewRegistry(
		toolRegistration(readFile, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true),
		toolRegistration(writeFile, model.ToolCategory{}, model.ToolPermissionWorkspaceWrite, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeWorkspace); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"},{"type":"function_call","call_id":"write_1","name":"write_file","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input, err := model.NewResponseInput("read and then write")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "multiple-tools", nil, input, TaskOptions{}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v, want a completed task", result)
	}
	if readFile.calls != 1 || writeFile.calls != 1 {
		t.Fatalf("tool calls = read_file:%d write_file:%d, want one each", readFile.calls, writeFile.calls)
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(requester.requests))
	}
}

func TestToolAudienceRoutingKeepsUserOnlyTextOutOfModelHistory(t *testing.T) {
	registry, err := tool.NewRegistry(toolRegistration(
		audienceRecordingTool{}, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true,
	))
	if err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"audience-1","name":"audience","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("start")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "audience-routing", nil, input, TaskOptions{}, store, graph.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v", result)
	}
	requestHistory, err := json.Marshal(requester.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requestHistory), "visible only to the user") || !strings.Contains(string(requestHistory), "visible to the model") {
		t.Fatalf("model history audience routing = %s", requestHistory)
	}
	transcript, err := json.Marshal(result.Final.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "visible only to the user") || !strings.Contains(string(transcript), "CQgH") {
		t.Fatalf("canonical transcript lost user-only text: %s", transcript)
	}
}

func TestAgentGraphResumesBetweenModelAndToolSteps(t *testing.T) {
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

	input, err := model.NewResponseInput([]map[string]any{{
		"type": "message",
		"role": "user",
		"content": []map[string]any{
			{"type": "input_text", "text": "read this attachment"},
			{"type": "input_image", "image_url": "data:image/png;base64,AA=="},
			{"type": "input_audio", "data": "opaque-audio-payload"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	options := graph.Options{MaxSteps: 1}
	execution, paused, err := agent.Start(context.Background(), "agent-poc", nil, input, TaskOptions{Model: "override-model"}, store, options)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	checkpoint, checkpointErr := execution.Checkpoint()
	if checkpointErr != nil {
		t.Fatal(checkpointErr)
	}
	if paused.Status != graph.StatusBudget || len(checkpoint.Invocations) != 1 || checkpoint.Invocations[0].Node != toolNode {
		t.Fatalf("paused result = %#v, want a checkpoint at the tool node", paused)
	}

	options.MaxSteps = 100
	_, completed, err := agent.Recover(context.Background(), "agent-poc", nil, store, options)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if completed.Status != graph.StatusCompleted || completed.Final == nil {
		t.Fatalf("completed result = %#v, want a completed checkpoint", completed)
	}
	if completed.Final.FinalResponse == nil {
		t.Fatal("final response was not retained in graph state")
	}
	if readFile.calls != 1 {
		t.Fatalf("read_file executions = %d, want 1", readFile.calls)
	}
	if len(requester.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(requester.requests))
	}
	for index, request := range requester.requests {
		if request.Model != "override-model" {
			t.Errorf("request %d model = %q, want persisted override", index+1, request.Model)
		}
	}
	if completed.Final.ModelOverride != "override-model" {
		t.Errorf("recovered model override = %q, want persisted override", completed.Final.ModelOverride)
	}
	if len(requester.requests[0].Tools) != 1 {
		t.Fatalf("initial tools = %#v, want only category selector", requester.requests[0].Tools)
	}
	for _, index := range []int{1, 2} {
		if len(requester.requests[index].Tools) != 2 {
			t.Errorf("request %d tools = %#v, want selector and read_file", index+1, requester.requests[index].Tools)
		}
	}
	encodedHistory, err := json.Marshal(requester.requests[2].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"input_audio", "opaque-audio-payload", "file contents"} {
		if !strings.Contains(string(encodedHistory), preserved) {
			t.Errorf("recovered history lost %q: %s", preserved, encodedHistory)
		}
	}
}

func TestAgentGraphGuidesHiddenToolCategoryAndLogsMismatch(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	readFile := &namedRecordingTool{name: "read_file"}
	webSearch := &namedRecordingTool{name: "web_search"}
	registry, err := tool.NewRegistry(
		toolRegistration(readFile, model.ToolCategory{Name: "files", Description: "Workspace files."}, model.ToolPermissionWorkspaceRead, true),
		toolRegistration(webSearch, model.ToolCategory{Name: "web", Description: "Web search."}, model.ToolPermissionNetwork, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_files","name":"select_tool_category","arguments":"{\"category\":\"files\"}"},{"type":"function_call","call_id":"hidden_web","name":"web_search","arguments":"{\"query\":\"PRIVATE-ARGUMENTS\"}"},{"type":"function_call","call_id":"unknown_tool","name":"imaginary_tool","arguments":"{\"secret\":\"PRIVATE-ARGUMENTS\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_web","name":"select_tool_category","arguments":"{\"category\":\"web\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"web_call","name":"web_search","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input, err := model.NewResponseInput("search the web")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "hidden-tool-category", nil, input, TaskOptions{ReasoningEffort: model.ReasoningEffortHigh}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || len(requester.requests) != 4 {
		t.Fatalf("result status=%q requests=%d, want completed with four requests", result.Status, len(requester.requests))
	}
	if readFile.calls != 0 || webSearch.calls != 1 {
		t.Fatalf("tool calls = read_file:%d web_search:%d, want only visible web_search", readFile.calls, webSearch.calls)
	}
	for requestIndex, want := range []string{
		model.CategorySelectorName,
		model.CategorySelectorName + ",read_file",
		model.CategorySelectorName + ",web_search",
		model.CategorySelectorName + ",web_search",
	} {
		got := strings.Join(toolNames(requester.requests[requestIndex].Tools), ",")
		if got != want {
			t.Errorf("request %d visible tools = %q, want %q", requestIndex+1, got, want)
		}
	}
	encodedHistory, err := json.Marshal(requester.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"web_search", `category \"web\"`, model.CategorySelectorName, "next request", "imaginary_tool", "not registered in a discoverable category"} {
		if !strings.Contains(string(encodedHistory), expected) {
			t.Errorf("hidden tool history is missing %q: %s", expected, encodedHistory)
		}
	}
	for _, expected := range []string{
		"agent model request started",
		"request=2",
		"reasoning_effort=high",
		"selected_category=files",
		`visible_tools="[select_tool_category read_file]"`,
		"tool category selected",
		"tool call is not in the visible tool set",
		"tool=web_search",
		"visible_tools",
		"expected_category=web",
	} {
		if !strings.Contains(logBuffer.String(), expected) {
			t.Errorf("logs %q missing %q", logBuffer.String(), expected)
		}
	}
	if strings.Contains(logBuffer.String(), "PRIVATE-ARGUMENTS") {
		t.Fatalf("logs exposed tool arguments: %s", logBuffer.String())
	}
}

func TestAgentGraphPreservesHistoryAndEnforcesPerTaskToolRoundLimit(t *testing.T) {
	readFile := &recordingTool{}
	registry, err := tool.NewRegistry(toolRegistration(readFile, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true))
	if err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_2","name":"read_file","arguments":"{}"}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	options := graph.Options{}
	history, err := model.NewResponseInput([]map[string]any{{
		"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "previous turn"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("current turn")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "round-limit", history, input, TaskOptions{MaxToolRounds: 1}, store, options)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v, want completed final state", result)
	}
	final := result.Final
	if final.TaskError == "" || !strings.Contains(final.TaskError, model.ErrToolCallLimit.Error()) {
		t.Fatalf("task error = %q, want tool-call limit error", final.TaskError)
	}
	if readFile.calls != 1 {
		t.Fatalf("read_file executions = %d, want 1 before the next batch is skipped", readFile.calls)
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(requester.requests))
	}
	encodedRequest, err := json.Marshal(requester.requests[0].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"previous turn", "current turn"} {
		if !strings.Contains(string(encodedRequest), content) {
			t.Errorf("request history lost %q: %s", content, encodedRequest)
		}
	}
	if !strings.Contains(string(mustMarshal(t, final.History)), "Skipped because the tool-call round limit was reached.") {
		t.Fatal("final history does not record the skipped tool call")
	}
}

type audienceRecordingTool struct{}

func (audienceRecordingTool) Definition() model.FunctionTool {
	return model.FunctionTool{Name: "audience", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (audienceRecordingTool) Run(context.Context, json.RawMessage) tool.Result {
	output := model.NewToolOutputParts(model.InputText{Text: "visible to the model"})
	return tool.Result{ModelOutput: &output, TranscriptOnly: []model.ContentPart{
		model.InputText{Text: "visible only to the user"},
		model.InputImage{ImageURL: "data:image/png;base64,CQgH"},
	}}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
