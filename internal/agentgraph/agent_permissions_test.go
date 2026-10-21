package agentgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	for _, name := range []string{"custom_read", "network_lookup"} {
		if !visible[name] {
			t.Errorf("read-only mode did not expose usable tool %q", name)
		}
	}
	for _, name := range []string{"custom_write", "unclassified_read"} {
		if visible[name] {
			t.Errorf("read-only mode exposed denied tool %q", name)
		}
	}
	if err := registry.SetPermissionMode(tool.PermissionModeAsk); err != nil {
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
