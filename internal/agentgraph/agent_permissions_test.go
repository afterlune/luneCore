package agentgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

func TestPlanModeCheckpointRoundTrips(t *testing.T) {
	codec := checkpoint.JSON[State]{}
	state, err := codec.Unmarshal([]byte(`{"plan_mode":true,"task_settings_version":5,"goal":"create a plan"}`))
	if err != nil {
		t.Fatalf("decode plan checkpoint: %v", err)
	}
	if !state.PlanMode || state.TaskSettingsVersion != 5 || state.Goal != "create a plan" {
		t.Fatalf("decoded plan checkpoint = %#v", state)
	}
	encoded, err := codec.Append(nil, state)
	if err != nil {
		t.Fatalf("encode checkpoint: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"plan_mode":true`)) {
		t.Fatalf("checkpoint lost PlanMode: %s", encoded)
	}
}

func TestToolVisibilityFollowsPermissionMode(t *testing.T) {
	customRead := &namedRecordingTool{name: "custom_read"}
	customWrite := &namedRecordingTool{name: "custom_write"}
	networkRead := &namedRecordingTool{name: "network_lookup"}
	unclassifiedRead := &namedRecordingTool{name: "unclassified_read"}
	registry, err := tool.NewRegistry(
		toolRegistration(customRead, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true),
		toolRegistration(customWrite, model.ToolCategory{}, model.ToolPermissionWorkspaceWrite, false),
		toolRegistration(networkRead, model.ToolCategory{}, model.ToolPermissionNetwork, true),
		toolRegistration(unclassifiedRead, model.ToolCategory{}, "future-class", true),
	)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewWithOptions(&scriptedRequester{}, registry, AgentOptions{
		ToolVisibleForMode: registry.ToolVisibleForMode,
		ToolReadOnly: func(name string) bool {
			metadata, ok := registry.ToolMetadata(name)
			return ok && metadata.ReadOnly
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions, _, err := agent.visibleTools(State{AgentID: "lead"}, "")
	if err != nil {
		t.Fatal(err)
	}
	visible := map[string]bool{}
	for _, definition := range definitions {
		if function, ok := definition.(model.FunctionTool); ok {
			visible[function.Name] = true
		}
	}
	for _, name := range []string{"custom_read", "network_lookup", "unclassified_read"} {
		if !visible[name] {
			t.Errorf("read-only mode did not expose usable tool %q", name)
		}
	}
	if visible["custom_write"] {
		t.Errorf("read-only mode exposed denied tool %q", "custom_write")
	}
	if err := registry.SetPermissionMode(tool.PermissionModeAuto); err != nil {
		t.Fatal(err)
	}
	definitions, _, err = agent.visibleTools(State{AgentID: "lead"}, "")
	if err != nil {
		t.Fatal(err)
	}
	visible = map[string]bool{}
	for _, definition := range definitions {
		if function, ok := definition.(model.FunctionTool); ok {
			visible[function.Name] = true
		}
	}
	for _, name := range []string{"custom_write", "unclassified_read"} {
		if !visible[name] {
			t.Errorf("ask mode did not expose approval-gated tool %q", name)
		}
	}
	definitions, _, err = agent.visibleTools(State{AgentID: "lead", PlanMode: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	visible = map[string]bool{}
	for _, definition := range definitions {
		if function, ok := definition.(model.FunctionTool); ok {
			visible[function.Name] = true
		}
	}
	for _, name := range []string{"custom_read", "network_lookup"} {
		if !visible[name] {
			t.Errorf("Plan mode hid read-only tool %q", name)
		}
	}
	for _, name := range []string{"custom_write"} {
		if visible[name] {
			t.Errorf("Plan mode exposed non-read-only tool %q", name)
		}
	}
	if !visible["unclassified_read"] {
		t.Error("Plan mode hid a tool explicitly marked read-only and approved by Ask mode")
	}
}

func TestPlanReadsFollowPermissionModeAndBlockMutations(t *testing.T) {
	for _, tc := range []struct {
		mode               tool.PermissionMode
		approvals, reviews int
	}{
		{mode: tool.PermissionModeReadOnly, approvals: 2},
		{mode: tool.PermissionModeWorkspace, approvals: 2},
		{mode: tool.PermissionModeAuto, reviews: 2},
		{mode: tool.PermissionModeFullAccess},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			workspace, external := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "local.txt"), []byte("inside marker"), 0o600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(external, "outside.txt")
			if err := os.WriteFile(outside, []byte("outside marker"), 0o600); err != nil {
				t.Fatal(err)
			}
			registry, err := tool.NewDefaultRegistry(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			if err := registry.SetPermissionMode(tc.mode); err != nil {
				t.Fatal(err)
			}
			network := &namedRecordingTool{name: "network_read"}
			write := &namedRecordingTool{name: "mutate"}
			command := &namedRecordingTool{name: "run_command"}
			for _, registration := range []tool.ToolRegistration{
				{Tool: network, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "web", Description: "Search the web and fetch pages."}, PermissionClass: model.ToolPermissionNetwork, ReadOnly: true, AlwaysVisible: true}},
				{Tool: write, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}, PermissionClass: model.ToolPermissionWorkspaceWrite, AlwaysVisible: true}},
				{Tool: command, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "command", Description: "Run workspace commands."}, PermissionClass: model.ToolPermissionCommand, AlwaysVisible: true}},
			} {
				if err := registry.Register(registration); err != nil {
					t.Fatal(err)
				}
			}
			approvals, reviews := 0, 0
			registry.SetPermissionApprovalHandler(func(context.Context, tool.PermissionRequest) bool { approvals++; return true })
			registry.SetPermissionAutoReviewHandler(func(context.Context, tool.PermissionRequest) (tool.AutoReviewResult, error) {
				reviews++
				return tool.AutoReviewResult{Decision: tool.AutoReviewApprove}, nil
			})
			localArgs, _ := json.Marshal(map[string]string{"path": "local.txt"})
			externalArgs, _ := json.Marshal(map[string]string{"path": outside})
			response := fmt.Sprintf(`{"status":"completed","output":[{"type":"function_call","call_id":"local","name":"read_file","arguments":%q},{"type":"function_call","call_id":"outside","name":"read_file","arguments":%q},{"type":"function_call","call_id":"network","name":"network_read","arguments":"{}"},{"type":"function_call","call_id":"write","name":"mutate","arguments":"{}"},{"type":"function_call","call_id":"command","name":"run_command","arguments":"{}"}]}`, string(localArgs), string(externalArgs))
			requester := &scriptedRequester{responses: []*model.Response{mustDecodeResponse(t, response), mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)}}
			agent, err := NewWithOptions(requester, registry, AgentOptions{
				ToolVisibleForMode: registry.ToolVisibleForMode,
				ToolReadOnly:       func(name string) bool { metadata, ok := registry.ToolMetadata(name); return ok && metadata.ReadOnly },
			})
			if err != nil {
				t.Fatal(err)
			}
			store, err := memory.New[State](graph.JSONClone[State])
			if err != nil {
				t.Fatal(err)
			}
			_, result, err := agent.Start(context.Background(), "plan-permission-matrix", nil, model.NewResponseInputText("inspect"), TaskOptions{PlanMode: true, PermissionOverride: string(tc.mode)}, store, graph.Options{})
			if err != nil || result.Status != graph.StatusCompleted {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if len(requester.requests) != 2 {
				t.Fatalf("requests=%d", len(requester.requests))
			}
			followup, err := json.Marshal(requester.requests[1])
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range []string{"inside marker", "outside marker", "network_read completed"} {
				if !strings.Contains(string(followup), marker) {
					t.Errorf("missing %q in follow-up: %s", marker, followup)
				}
			}
			if approvals != tc.approvals || reviews != tc.reviews || network.calls != 1 || write.calls != 0 || command.calls != 0 {
				t.Fatalf("approvals=%d reviews=%d network=%d writes=%d commands=%d", approvals, reviews, network.calls, write.calls, command.calls)
			}
		})
	}
}

func TestPlanRejectsStaleVisibleMutatingTools(t *testing.T) {
	for _, team := range []bool{false, true} {
		t.Run(fmt.Sprintf("team=%t", team), func(t *testing.T) {
			write := &namedRecordingTool{name: "mutate"}
			registry, err := tool.NewRegistry(toolRegistration(write, model.ToolCategory{}, model.ToolPermissionWorkspaceWrite, false))
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
				t.Fatal(err)
			}
			agent, err := NewWithOptions(&scriptedRequester{}, registry, AgentOptions{ToolReadOnly: func(name string) bool { metadata, ok := registry.ToolMetadata(name); return ok && metadata.ReadOnly }})
			if err != nil {
				t.Fatal(err)
			}
			state := State{PlanMode: true, TeamBranch: team, VisibleTools: []string{"mutate"}, PendingCalls: []model.OutputFunctionCall{{Name: "mutate", CallID: "write", Arguments: `{}`}}}
			if _, err := agent.executeTool(context.Background(), graph.CallInfo{}, state); err != nil {
				t.Fatal(err)
			}
			if write.calls != 0 {
				t.Fatal("stale visible tool bypassed Plan mode")
			}
		})
	}
}

func TestPlanReadsFollowPermissionChangesDuringTask(t *testing.T) {
	network := &namedRecordingTool{name: "network_read"}
	registry, err := tool.NewRegistry(toolRegistration(network, model.ToolCategory{}, model.ToolPermissionNetwork, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	approvals := 0
	registry.SetPermissionApprovalHandler(func(context.Context, tool.PermissionRequest) bool {
		approvals++
		return true
	})
	agent, err := NewWithOptions(&scriptedRequester{}, registry, AgentOptions{
		ToolReadOnly: func(name string) bool {
			metadata, ok := registry.ToolMetadata(name)
			return ok && metadata.ReadOnly
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := State{PlanMode: true, VisibleTools: []string{"network_read"}, PendingCalls: []model.OutputFunctionCall{{Name: "network_read", CallID: "network", Arguments: `{}`}}}
	if _, err := agent.executeTool(context.Background(), graph.CallInfo{}, state); err != nil {
		t.Fatal(err)
	}
	if network.calls != 1 || approvals != 0 {
		t.Fatalf("full-access Plan read calls=%d approvals=%d", network.calls, approvals)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeWorkspace); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.executeTool(context.Background(), graph.CallInfo{}, state); err != nil {
		t.Fatal(err)
	}
	if network.calls != 2 || approvals != 1 {
		t.Fatalf("workspace Plan read calls=%d approvals=%d, want 2 and 1", network.calls, approvals)
	}
}

func TestReadOnlyPermissionExposesExplorationToolsAndBlocksWrites(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "main.go"), []byte("package main\n// target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewDefaultRegistry(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.SetPermissionMode(tool.PermissionModeReadOnly); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_1","name":"select_tool_category","arguments":"{\"category\":\"files\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"list_1","name":"list_dir","arguments":"{}"},{"type":"function_call","call_id":"glob_1","name":"glob","arguments":"{\"pattern\":\"**/*.go\"}"},{"type":"function_call","call_id":"grep_1","name":"grep","arguments":"{\"pattern\":\"target\"}"},{"type":"function_call","call_id":"write_1","name":"write_file","arguments":"{\"path\":\"blocked.txt\",\"content\":\"no\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"plan ready"}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{ToolVisible: registry.ToolVisible})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input, err := model.NewResponseInput("inspect the repository")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "readonly-exploration-tools", nil, input, TaskOptions{}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || len(requester.requests) != 3 {
		t.Fatalf("result = %#v, model requests = %d", result, len(requester.requests))
	}

	visible := map[string]bool{}
	for _, definition := range requester.requests[1].Tools {
		if function, ok := definition.(model.FunctionTool); ok {
			visible[function.Name] = true
		}
	}
	for _, name := range []string{"list_dir", "glob", "grep", "read_file", model.CategorySelectorName} {
		if !visible[name] {
			t.Errorf("read-only mode did not expose %q: %#v", name, visible)
		}
	}
	for _, name := range []string{"write_file", "edit_file", "command"} {
		if visible[name] {
			t.Errorf("read-only mode exposed denied tool %q", name)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "blocked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked write created a file: %v", err)
	}
	encoded, err := json.Marshal(requester.requests[2].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"src", "src/main.go", "src/main.go:2:// target", "which is not visible in this request"} {
		if !strings.Contains(string(encoded), output) {
			t.Errorf("tool output %q missing from history: %s", output, encoded)
		}
	}
}
