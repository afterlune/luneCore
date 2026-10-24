package agentgraph

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

func TestDiscoveredToolsAndRequestPrefixSurviveNextTask(t *testing.T) {
	read := &namedRecordingTool{name: "read_file"}
	command := &namedRecordingTool{name: "command"}
	registry, err := tool.NewRegistry(
		toolRegistration(read, model.ToolCategory{Name: "files", Description: "Files"}, model.ToolPermissionWorkspaceRead, true),
		toolRegistration(command, model.ToolCategory{Name: "command", Description: "Commands"}, model.ToolPermissionCommand, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	registry.SetPermissionMode(tool.PermissionModeFullAccess)
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"output":[{"type":"function_call","call_id":"a","name":"select_tool_category","arguments":"{\"category\":\"files\"}"}]}`),
		mustDecodeResponse(t, `{"output":[{"type":"function_call","call_id":"b","name":"select_tool_category","arguments":"{\"category\":\"command\"}"}]}`),
		teamTextResponse("done"), teamTextResponse("next"),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{Provider: "test", Model: "test", CurrentPermissionMode: func() string { return "full-access" }})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := memory.New[State](graph.JSONClone[State])
	_, result, err := agent.Start(context.Background(), "session", nil, model.NewResponseInputText("first"), TaskOptions{ExecutionID: "first"}, store, graph.Options{})
	if err != nil || result.Final == nil {
		t.Fatalf("first: %v", err)
	}
	first := *result.Final
	if err := store.Delete(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	_, result, err = agent.StartWithTranscript(context.Background(), "session", &first.History, &first.Transcript, model.NewResponseInputText("second"), TaskOptions{ExecutionID: "second", Conversation: first.ConversationState()}, store, graph.Options{})
	if err != nil || result.Final == nil {
		t.Fatalf("second: %v", err)
	}
	if len(requester.requests) != 4 {
		t.Fatalf("requests=%d", len(requester.requests))
	}
	if !reflect.DeepEqual(toolNames(requester.requests[2].Tools), toolNames(requester.requests[3].Tools)) {
		t.Fatal("next task lost discovered tools")
	}
	if !contains(toolNames(requester.requests[2].Tools), "read_file") || !contains(toolNames(requester.requests[2].Tools), "command") {
		t.Fatal("category replacement remains")
	}
	if requester.requests[0].Instructions != requester.requests[3].Instructions {
		t.Fatal("instructions changed across tasks")
	}
	if countHostMessages(result.Final.History) != 1 {
		t.Fatal("unchanged context was inserted twice")
	}
	if countHostMessages(result.Final.Transcript) != 0 {
		t.Fatal("host context leaked into transcript")
	}
	if len(result.Final.LastRequest.Changes) != 0 {
		t.Fatalf("unexpected local changes: %v", result.Final.LastRequest.Changes)
	}
}

func TestAutomaticDiscoveryPreservesPermissionsAndArguments(t *testing.T) {
	for _, tc := range []struct {
		mode             tool.PermissionMode
		plan, approve    bool
		calls, approvals int
	}{
		{tool.PermissionModeReadOnly, false, true, 0, 0},
		{tool.PermissionModeWorkspace, false, true, 1, 0},
		{tool.PermissionModeAuto, false, true, 1, 0},
		{tool.PermissionModeFullAccess, false, true, 1, 0},
		{tool.PermissionModeFullAccess, true, true, 0, 0},
	} {
		t.Run(string(tc.mode)+map[bool]string{true: "-plan", false: ""}[tc.plan]+map[bool]string{true: "-allow", false: "-deny"}[tc.approve], func(t *testing.T) {
			writer := &argumentRecordingTool{namedRecordingTool: namedRecordingTool{name: "write_file"}}
			registry, _ := tool.NewRegistry(toolRegistration(writer, model.ToolCategory{Name: "files", Description: "Files"}, model.ToolPermissionWorkspaceWrite, false))
			defer registry.Close()
			registry.SetPermissionMode(tc.mode)
			approvals := 0
			registry.SetPermissionApprovalHandler(func(context.Context, tool.PermissionRequest) bool { approvals++; return tc.approve })
			arguments := `{"content":"original generated content"}`
			encoded, _ := json.Marshal(arguments)
			requester := &scriptedRequester{responses: []*model.Response{
				mustDecodeResponse(t, `{"output":[{"type":"function_call","call_id":"original-call","name":"write_file","arguments":`+string(encoded)+`}]}`), teamTextResponse("done"),
			}}
			agent, err := NewWithOptions(requester, registry, AgentOptions{ToolVisibleForMode: registry.ToolVisibleForMode, ToolReadOnly: func(string) bool { return false }})
			if err != nil {
				t.Fatal(err)
			}
			store, _ := memory.New[State](graph.JSONClone[State])
			_, result, err := agent.Start(context.Background(), "run", nil, model.NewResponseInputText("write"), TaskOptions{PlanMode: tc.plan}, store, graph.Options{})
			if err != nil || result.Status != graph.StatusCompleted {
				t.Fatalf("run: %v %s", err, result.Status)
			}
			if writer.calls != tc.calls || approvals != tc.approvals {
				t.Fatalf("calls=%d approvals=%d", writer.calls, approvals)
			}
			if tc.calls > 0 && writer.arguments != arguments {
				t.Fatalf("arguments changed: %#v", writer.arguments)
			}
		})
	}
}

func TestHostSnapshotsAppendOnModeAndProjectChanges(t *testing.T) {
	registry, _ := tool.NewRegistry()
	defer registry.Close()
	loads := 0
	agent, _ := NewWithOptions(&scriptedRequester{}, registry, AgentOptions{MemorySummaryFor: func(context.Context, State) (string, error) { loads++; return "notes", nil }})
	state, _ := initialState(nil, nil, model.NewResponseInputText("request"))
	state.ContextAtTaskStart = true
	state = agent.prepareHostContext(context.Background(), state, "read-only", nil)
	first := state.History.Items[0]
	state.PlanMode = true
	state.ProjectInstructions = []ProjectInstruction{{Path: "AGENTS.md", Content: "new rule"}}
	state = agent.prepareHostContext(context.Background(), state, "read-only", nil)
	if loads != 1 || countHostMessages(state.History) != 2 || !reflect.DeepEqual(first, state.History.Items[0]) {
		t.Fatal("snapshot rewrote prefix or reloaded memory")
	}
	if !strings.Contains(state.HostContext, "new rule") || !strings.Contains(state.HostContext, "You are in Plan mode.") {
		t.Fatal("snapshot lost current guidance")
	}
	state.PlanMode = false
	state = agent.prepareHostContext(context.Background(), state, "read-only", nil)
	// Simulate compaction losing the newest snapshot while retaining an older
	// matching snapshot followed by a conflicting mode snapshot.
	state.History = *model.NewResponseInputItems(hostContextMessage(state.HostContext), hostContextMessage("outdated mode"))
	state = agent.prepareHostContext(context.Background(), state, "read-only", nil)
	if !hasHostContext(state.History, state.HostContext) || countHostMessages(state.History) != 3 {
		t.Fatal("older snapshot suppressed current mode restoration")
	}
}

func countHostMessages(input model.ResponseInput) int {
	count := 0
	for _, item := range input.Items {
		if msg, ok := item.(model.InputMessage); ok && IsHostContextMessage(msg) {
			count++
		}
	}
	return count
}

type argumentRecordingTool struct {
	namedRecordingTool
	arguments string
}

func (t *argumentRecordingTool) Run(ctx context.Context, arguments json.RawMessage) tool.Result {
	t.arguments = string(arguments)
	return t.namedRecordingTool.Run(ctx, arguments)
}
