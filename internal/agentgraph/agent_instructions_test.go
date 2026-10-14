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
	instructions := requester.requests[0].Instructions
	for _, expected := range []string{
		"Map the public API and summarize its dependencies.",
		"does not override higher-priority instructions",
	} {
		if !strings.Contains(instructions, expected) {
			t.Errorf("instructions %q are missing %q", instructions, expected)
		}
	}
	if strings.Contains(instructions, "You are in plan mode.") {
		t.Errorf("task instructions force planning: %q", instructions)
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
	if len(requester.requests) != 2 || !strings.Contains(requester.requests[1].Instructions, "Do not change generated files.") {
		t.Fatalf("follow-up request did not receive project instruction: %#v", requester.requests)
	}
}

func TestProjectInstructionsStayOutOfAcceptancePrompts(t *testing.T) {
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"criteria","name":"prepare_task_criteria","arguments":"{\"kind\":\"answer\",\"criteria\":[{\"id\":\"answer\",\"description\":\"Answer the request.\",\"basis\":\"hello\"}]}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello answered"}]}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"acceptance","name":"submit_acceptance","arguments":"{\"candidate_id\":\"candidate-1\",\"results\":[{\"criterion_id\":\"answer\",\"outcome\":\"passed\",\"evidence\":[\"candidate:candidate-1\"],\"reason\":\"The answer addresses the request.\"}]}"}]}`),
	}}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		AcceptanceEnabled: true,
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
	_, result, err := agent.Start(context.Background(), "project-instruction-acceptance", nil, input, TaskOptions{
		PersonalInstructions: personalInstructions,
	}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted {
		t.Fatalf("Start result=%#v error=%v", result, err)
	}
	if len(requester.requests) != 3 {
		t.Fatalf("model requests = %d, want criteria, execution, and acceptance", len(requester.requests))
	}
	if strings.Contains(requester.requests[0].Instructions, "Project-only execution guidance") ||
		strings.Contains(requester.requests[0].Instructions, personalInstructions) ||
		strings.Contains(requester.requests[2].Instructions, "Project-only execution guidance") ||
		strings.Contains(requester.requests[2].Instructions, personalInstructions) {
		t.Fatal("guidance changed acceptance criteria or acceptance review")
	}
	executionInstructions := requester.requests[1].Instructions
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
