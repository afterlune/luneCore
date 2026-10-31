package agentgraph

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

func TestTaskGoalIsAppliedWithoutForcingPlanInstructions(t *testing.T) {
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"plan ready"}]}]}`),
	}}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("inspect the project")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "goal-instructions", nil, input, TaskOptions{
		Goal: "Map the public API and summarize its dependencies.",
	}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || len(requester.requests) != 1 {
		t.Fatalf("result status=%q requests=%d", result.Status, len(requester.requests))
	}
	instructions := requestContextText(requester.requests[0])
	for _, expected := range []string{
		"Map the public API and summarize its dependencies.",
		"does not override higher-priority instructions",
	} {
		if !strings.Contains(instructions, expected) {
			t.Errorf("instructions %q are missing %q", instructions, expected)
		}
	}
	if strings.Contains(instructions, "You are in Plan mode.") {
		t.Errorf("task instructions force planning: %q", instructions)
	}
}

func TestInstructionsStayStableAcrossToolRounds(t *testing.T) {
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read-1","name":"inspect","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	registered := &namedRecordingTool{name: "inspect"}
	registry, err := tool.NewRegistry(toolRegistration(registered, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("inspect the project")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "stable-prefix", nil, input, TaskOptions{
		Goal: "Keep the configured session goal stable.",
	}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted {
		t.Fatalf("Start status=%q error=%v", result.Status, err)
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(requester.requests))
	}
	if requester.requests[0].Instructions != requester.requests[1].Instructions {
		t.Fatalf("instructions changed across tool round:\nfirst: %s\nsecond: %s", requester.requests[0].Instructions, requester.requests[1].Instructions)
	}
	for _, expected := range []string{
		"autonomous coding agent",
		"file or directory paths",
		"inspect them directly using local file tools",
	} {
		if !strings.Contains(requester.requests[0].Instructions, expected) {
			t.Errorf("Instructions missing %q: %s", expected, requester.requests[0].Instructions)
		}
	}
}

func TestPlanModeInstructionsIncludePlanFormat(t *testing.T) {
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<proposed_plan>Done.</proposed_plan>"}]}]}`),
	}}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewWithOptions(requester, registry, AgentOptions{AskUser: true})
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("plan the change")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "plan-mode-instructions", nil, input, TaskOptions{PlanMode: true}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted || len(requester.requests) != 1 {
		t.Fatalf("Start = status %q, requests %d, error %v", result.Status, len(requester.requests), err)
	}
	instructions := requestContextText(requester.requests[0])
	for _, expected := range []string{
		"You are in Plan mode.",
		"<proposed_plan>",
		"</proposed_plan>",
	} {
		if !strings.Contains(instructions, expected) {
			t.Errorf("Plan mode instructions missing %q: %s", expected, instructions)
		}
	}
	workerInstructions := taskInstructions("", "workspace", true, true)
	if !strings.Contains(workerInstructions, "report findings to the lead") {
		t.Fatalf("shared Plan mode instructions = %q", workerInstructions)
	}
}

func TestProjectInstructionsPreflightSkipsWholeToolBatch(t *testing.T) {
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
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{\"path\":\"src/main.go\"}"},{"type":"function_call","call_id":"write_1","name":"write_file","arguments":"{\"path\":\"src/main.go\",\"content\":\"changed\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will follow the project rule."}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ToolVisible: registry.ToolVisible,
		ProjectInstructionsForTool: func(_ context.Context, _ State, call model.OutputFunctionCall) ([]ProjectInstruction, error) {
			if call.Name == "write_file" {
				return []ProjectInstruction{{Path: "src/AGENTS.md", Content: "Do not change generated files."}}, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("inspect and update the source")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "project-instruction-preflight", nil, input, TaskOptions{}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted {
		t.Fatalf("Start result=%#v error=%v", result, err)
	}
	if readFile.calls != 0 || writeFile.calls != 0 {
		t.Fatalf("tools ran before the model reconsidered the batch: read=%d write=%d", readFile.calls, writeFile.calls)
	}
	if len(requester.requests) != 2 || !strings.Contains(requestContextText(requester.requests[1]), "Do not change generated files.") {
		t.Fatalf("follow-up request did not receive project instruction: %#v", requester.requests)
	}
}

func TestProjectAndPersonalInstructionsReachTaskRequest(t *testing.T) {
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello answered"}]}]}`),
	}}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ProjectInstructionsForTask: func(context.Context, State) ([]ProjectInstruction, error) {
			return []ProjectInstruction{{Path: "AGENTS.md", Content: "Project-only execution guidance."}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("hello")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	personalInstructions := "Prefers concise answers."
	prior := model.NewResponseInputItems(model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("Earlier context remains available to the model.")})
	_, result, err := agent.StartWithTranscript(context.Background(), "project-instruction-task", prior, prior, input, TaskOptions{
		PersonalInstructions: personalInstructions,
	}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted {
		t.Fatalf("Start result=%#v error=%v", result, err)
	}
	if len(requester.requests) != 1 {
		t.Fatalf("model requests = %d, want one task request", len(requester.requests))
	}
	executionInstructions := requestContextText(requester.requests[0])
	if !strings.Contains(executionInstructions, "Project-only execution guidance") || !strings.Contains(executionInstructions, personalInstructions) {
		t.Fatal("project or personal guidance was not included for task execution")
	}
	if strings.Index(executionInstructions, personalInstructions) > strings.Index(executionInstructions, "Project instructions from") {
		t.Fatal("project guidance was not ordered after personal preferences")
	}
	if result.Final == nil {
		t.Fatal("completed task has no final checkpoint")
	}
	if result.Final.PersonalInstructions != personalInstructions {
		t.Fatalf("final checkpoint personal instructions = %q", result.Final.PersonalInstructions)
	}
	encoded, err := json.Marshal(result.Final)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint State
	if err := json.Unmarshal(encoded, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.PersonalInstructions != personalInstructions {
		t.Fatalf("checkpoint personal instructions = %q", checkpoint.PersonalInstructions)
	}
}

func requestContextText(request model.ModelRequest) string {
	var parts []string
	for _, item := range request.Input.Items {
		if msg, ok := item.(model.InputMessage); ok && msg.Content.Text != nil {
			parts = append(parts, *msg.Content.Text)
		}
	}
	return strings.Join(parts, "\n")
}
