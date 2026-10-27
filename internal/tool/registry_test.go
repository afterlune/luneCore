package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type loggingTestTool struct{}

func (loggingTestTool) Definition() model.FunctionTool {
	return model.FunctionTool{
		Name:       "private_tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}
}

func (loggingTestTool) Run(context.Context, json.RawMessage) Result {
	return Result{Output: "private-tool-output", Err: errors.New("private-tool-error")}
}

func TestExecuteCallLogsMetadataWithoutPayloads(t *testing.T) {
	previous := slog.Default()
	logs := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	registry, err := NewRegistry(ToolRegistration{Tool: loggingTestTool{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	call := model.OutputFunctionCall{
		CallID:    "call-123",
		Name:      "private_tool",
		Arguments: `{"secret":"private-tool-arguments"}`,
	}
	registry.ExecuteCall(context.Background(), call)

	output := logs.String()
	for _, expected := range []string{"tool execution started", "tool execution finished", "tool=private_tool", "call_id=call-123", "failed=true"} {
		if !strings.Contains(output, expected) {
			t.Errorf("logs %q do not contain %q", output, expected)
		}
	}
	for _, private := range []string{"private-tool-arguments", "private-tool-output", "private-tool-error"} {
		if strings.Contains(output, private) {
			t.Errorf("logs contain private payload %q: %s", private, output)
		}
	}
}

func TestExecuteCallPublishesSanitizedLifecycleEvents(t *testing.T) {
	registry, err := NewRegistry(ToolRegistration{Tool: loggingTestTool{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	var events []ToolEvent
	registry.SetToolEventObserver(func(event ToolEvent) {
		events = append(events, event)
	})
	registry.ExecuteCall(context.Background(), model.OutputFunctionCall{
		CallID:    "call-456",
		Name:      "private_tool",
		Arguments: `{"secret":"private-tool-arguments"}`,
	})
	if len(events) != 2 {
		t.Fatalf("events = %#v, want start and finish", events)
	}
	if !events[0].Started || events[0].Name != "private_tool" || events[0].CallID != "call-456" {
		t.Errorf("start event = %#v", events[0])
	}
	finished := events[1]
	if finished.Started || finished.Name != "private_tool" || finished.CallID != "call-456" || !finished.Failed || finished.Canceled {
		t.Errorf("finish event = %#v", finished)
	}
	if finished.Duration < 0 {
		t.Errorf("finish duration = %s, want nonnegative duration", finished.Duration)
	}
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "private-tool-") {
			t.Errorf("event contains tool payload: %s", encoded)
		}
	}
}

func TestAgentIdentityFlowsThroughSharedToolAndApprovalHandlers(t *testing.T) {
	registry, err := NewRegistry(ToolRegistration{
		Tool:     loggingTestTool{},
		Metadata: model.ToolMetadata{PermissionClass: model.ToolPermissionNetwork, ReadOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeAuto); err != nil {
		t.Fatal(err)
	}
	var approvedAgent string
	registry.SetPermissionApprovalHandler(func(_ context.Context, request PermissionRequest) bool {
		approvedAgent = request.AgentID
		return true
	})
	var events []ToolEvent
	registry.SetToolEventObserver(func(event ToolEvent) { events = append(events, event) })
	registry.ExecuteCall(model.WithAgentID(context.Background(), "reviewer"), model.OutputFunctionCall{
		CallID: "call-agent", Name: "private_tool", Arguments: `{}`,
	})
	if approvedAgent != "reviewer" {
		t.Fatalf("approval agent ID = %q, want reviewer", approvedAgent)
	}
	if len(events) != 2 || events[0].AgentID != "reviewer" || events[1].AgentID != "reviewer" {
		t.Fatalf("tool events = %#v, want both attributed to reviewer", events)
	}
}

func TestDefaultRegistryIncludesEveryBuiltInTool(t *testing.T) {
	registry, err := NewDefaultRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewDefaultRegistry: %v", err)
	}
	defer registry.Close()
	if mode := registry.PermissionMode(); mode != PermissionModeWorkspace {
		t.Fatalf("default registry mode = %q, want workspace", mode)
	}
	want := map[string]model.ToolMetadata{
		"command":    {Category: model.ToolCategory{Name: "command", Description: "Run workspace commands."}, PermissionClass: model.ToolPermissionCommand, AlwaysVisible: true},
		"read_file":  {Category: model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}, PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true, AlwaysVisible: true},
		"read_image": {Category: model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}, PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true, AlwaysVisible: true},
		"write_file": {Category: model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}, PermissionClass: model.ToolPermissionWorkspaceWrite, AlwaysVisible: true},
		"edit_file":  {Category: model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}, PermissionClass: model.ToolPermissionWorkspaceWrite, AlwaysVisible: true},
		"web_search": {Category: model.ToolCategory{Name: "web", Description: "Search the web and fetch pages."}, PermissionClass: model.ToolPermissionNetwork, ReadOnly: true},
		"web_fetch":  {Category: model.ToolCategory{Name: "web", Description: "Search the web and fetch pages."}, PermissionClass: model.ToolPermissionNetwork, ReadOnly: true},
		"list_dir":   {Category: model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}, PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true, AlwaysVisible: true},
		"glob":       {Category: model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}, PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true, AlwaysVisible: true},
		"grep":       {Category: model.ToolCategory{Name: "files", Description: "Read, search, and modify workspace files."}, PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true, AlwaysVisible: true},
	}
	for _, definition := range registry.Definitions() {
		function, ok := definition.(model.FunctionTool)
		if !ok {
			t.Errorf("tool definition has type %T, want model.FunctionTool", definition)
			continue
		}
		if _, exists := want[function.Name]; !exists {
			t.Errorf("unexpected tool %q", function.Name)
			continue
		}
		metadata, exists := registry.ToolMetadata(function.Name)
		if !exists {
			t.Errorf("metadata missing for %q", function.Name)
			continue
		}
		if metadata != want[function.Name] {
			t.Errorf("metadata for %q = %#v, want %#v", function.Name, metadata, want[function.Name])
		}
		delete(want, function.Name)
	}
	for name := range want {
		t.Errorf("default registry is missing %q", name)
	}
}

func TestHostToolRegistrationsPreserveAlwaysVisibleMetadata(t *testing.T) {
	registrations, err := HostToolRegistrations([]publictool.Registration{
		{
			Tool: publicRegistrationTestTool{name: "host_eager"},
			Metadata: publictool.Metadata{
				Category:        publictool.Category{Name: "host", Description: "Host tools."},
				PermissionClass: publictool.PermissionWorkspaceRead,
				ReadOnly:        true, AlwaysVisible: true,
			},
		},
		{
			Tool: publicRegistrationTestTool{name: "host_deferred"},
			Metadata: publictool.Metadata{
				Category:        publictool.Category{Name: "host", Description: "Host tools."},
				PermissionClass: publictool.PermissionWorkspaceRead,
				ReadOnly:        true,
			},
		},
	})
	if err != nil {
		t.Fatalf("HostToolRegistrations: %v", err)
	}
	if len(registrations) != 2 || !registrations[0].Metadata.AlwaysVisible || registrations[1].Metadata.AlwaysVisible {
		t.Fatalf("converted metadata = %#v, want eager then deferred", registrations)
	}
}

type publicRegistrationTestTool struct{ name string }

func (tool publicRegistrationTestTool) Definition() publictool.Definition {
	return publictool.Definition{
		Name: tool.name, Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}
}

func (publicRegistrationTestTool) Run(context.Context, json.RawMessage) publictool.Result {
	return publictool.Result{Output: "ok"}
}

func TestDefaultRegistryFailsClosedWhenSandboxHelperIsUnavailable(t *testing.T) {
	registry, err := NewDefaultRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewDefaultRegistry: %v", err)
	}
	defer registry.Close()
	approvals := 0
	registry.SetPermissionApprovalHandler(func(context.Context, PermissionRequest) bool {
		approvals++
		return true
	})

	result := registry.Run(context.Background(), "command", json.RawMessage(`{"command":"exit 0"}`))
	if approvals != 1 {
		t.Fatalf("command approvals = %d, want one approval before sandbox execution", approvals)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "kernel sandbox helper") {
		t.Fatalf("command result error = %v, want fail-closed sandbox helper error", result.Err)
	}
}

func TestCustomRegistryDefaultsToReadOnly(t *testing.T) {
	uncategorized, err := NewRegistry(ToolRegistration{Tool: privateNamedTool{name: "custom_tool"}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if mode := uncategorized.PermissionMode(); mode != PermissionModeReadOnly {
		t.Errorf("NewRegistry mode = %q, want read-only", mode)
	}
	if result := uncategorized.Run(context.Background(), "custom_tool", json.RawMessage(`{}`)); result.Err == nil {
		t.Error("NewRegistry ran an unrecognized custom tool in its default read-only mode")
	}

	categorized, err := NewRegistry(ToolRegistration{
		Tool:     privateNamedTool{name: "custom_tool"},
		Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "custom", Description: "Custom tools."}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if mode := categorized.PermissionMode(); mode != PermissionModeReadOnly {
		t.Errorf("categorized registry mode = %q, want read-only", mode)
	}
	if result := categorized.Run(context.Background(), "custom_tool", json.RawMessage(`{}`)); result.Err == nil {
		t.Error("categorized registry ran an unclassified custom tool in its default read-only mode")
	}
}

func TestCategorizedRegistryPreservesCategoryAndToolOrder(t *testing.T) {
	first := loggingTestTool{}
	second := privateNamedTool{name: "second_tool"}
	registry, err := NewRegistry(
		ToolRegistration{Tool: first, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "files", Description: "Workspace files."}}},
		ToolRegistration{Tool: second, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "files", Description: "Workspace files."}}},
		ToolRegistration{Tool: privateNamedTool{name: "web_tool"}, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "web", Description: "Web access."}}},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	categories := registry.Categories()
	if len(categories) != 2 || categories[0].Name != "files" || categories[1].Name != "web" {
		t.Fatalf("categories = %#v, want files then web", categories)
	}
	definitions := registry.DefinitionsForCategory("files")
	if len(definitions) != 2 {
		t.Fatalf("files definitions = %d, want 2", len(definitions))
	}
	firstDefinition, firstOK := definitions[0].(model.FunctionTool)
	secondDefinition, secondOK := definitions[1].(model.FunctionTool)
	if !firstOK || !secondOK || firstDefinition.Name != "private_tool" || secondDefinition.Name != "second_tool" {
		t.Fatalf("files definitions = %#v, want registration order", definitions)
	}
}

func TestRegistrySnapshotsToolDefinitionsOnRegistration(t *testing.T) {
	strict := false
	definitionTool := &snapshotDefinitionTool{definition: model.FunctionTool{
		Name:       "snapshot_tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
		Strict:     &strict,
	}}
	registry, err := NewRegistry(ToolRegistration{
		Tool: definitionTool,
		Metadata: model.ToolMetadata{Category: model.ToolCategory{
			Name: "snapshot", Description: "Snapshot tools.",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if definitionTool.calls != 1 {
		t.Fatalf("Definition calls after registration = %d, want 1", definitionTool.calls)
	}

	definitionTool.definition.Parameters[0] = '!'
	*definitionTool.definition.Strict = true
	first := registry.Definitions()[0].(model.FunctionTool)
	first.Parameters[0] = '!'
	*first.Strict = true
	second := registry.Definitions()[0].(model.FunctionTool)
	if string(second.Parameters) != `{"type":"object","properties":{},"required":[],"additionalProperties":false}` {
		t.Errorf("definition parameters changed after registration or return: %s", second.Parameters)
	}
	if second.Strict == nil || *second.Strict {
		t.Errorf("definition strict flag = %v, want false", second.Strict)
	}
	_ = registry.DefinitionsForCategory("snapshot")
	if definitionTool.calls != 1 {
		t.Errorf("Definition calls after reads = %d, want 1", definitionTool.calls)
	}
	if !registry.Unregister("snapshot_tool") {
		t.Fatal("Unregister returned false for registered tool")
	}
	if got := registry.Definitions(); len(got) != 0 {
		t.Errorf("definitions after unregister = %#v, want empty", got)
	}
	if got := registry.DefinitionsForCategory("snapshot"); len(got) != 0 {
		t.Errorf("category definitions after unregister = %#v, want empty", got)
	}
}

func TestCategorizedRegistryRejectsInconsistentCategoryMetadata(t *testing.T) {
	_, err := NewRegistry(
		ToolRegistration{Tool: loggingTestTool{}, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "files", Description: "Workspace files."}}},
		ToolRegistration{Tool: privateNamedTool{name: "second_tool"}, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "files", Description: "Different description."}}},
	)
	if err == nil {
		t.Fatal("expected inconsistent category descriptions to fail")
	}
}

func TestRegistryDoesNotMixCategorizedAndUncategorizedTools(t *testing.T) {
	registry, err := NewRegistry(ToolRegistration{Tool: loggingTestTool{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(ToolRegistration{Tool: privateNamedTool{name: "second_tool"}, Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "files", Description: "Workspace files."}}}); err == nil {
		t.Fatal("expected a categorized tool to be rejected after an uncategorized tool")
	}

	categorized, err := NewRegistry(ToolRegistration{
		Tool:     loggingTestTool{},
		Metadata: model.ToolMetadata{Category: model.ToolCategory{Name: "files", Description: "Workspace files."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := categorized.Register(ToolRegistration{Tool: privateNamedTool{name: "second_tool"}}); err == nil {
		t.Fatal("expected an uncategorized tool to be rejected from a categorized registry")
	}
}

func TestDefaultRegistryCategorizesBuiltInTools(t *testing.T) {
	registry, err := NewDefaultRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewDefaultRegistry: %v", err)
	}
	defer registry.Close()
	want := map[string][]string{
		"files":   {"list_dir", "glob", "grep", "read_file", "read_image", "write_file", "edit_file"},
		"command": {"command"},
		"web":     {"web_search", "web_fetch"},
	}
	if categories := registry.Categories(); len(categories) != 3 || categories[0].Name != "files" || categories[1].Name != "command" || categories[2].Name != "web" {
		t.Fatalf("categories = %#v, want files, command, web", categories)
	}
	for category, expectedNames := range want {
		definitions := registry.DefinitionsForCategory(category)
		if len(definitions) != len(expectedNames) {
			t.Errorf("%s definitions = %d, want %d", category, len(definitions), len(expectedNames))
			continue
		}
		for index, expectedName := range expectedNames {
			definition, ok := definitions[index].(model.FunctionTool)
			if !ok || definition.Name != expectedName {
				t.Errorf("%s definition %d = %#v, want %q", category, index, definitions[index], expectedName)
			}
		}
	}
}

func TestUserQuestionToolNameIsReserved(t *testing.T) {
	_, err := NewRegistry(ToolRegistration{Tool: privateNamedTool{name: model.UserQuestionToolName}})
	if err == nil || !strings.Contains(err.Error(), model.UserQuestionToolName) {
		t.Fatalf("register built-in user question name error = %v", err)
	}
}

type privateNamedTool struct{ name string }

func (t privateNamedTool) Definition() model.FunctionTool {
	return model.FunctionTool{
		Name:       t.name,
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}
}

func (privateNamedTool) Run(context.Context, json.RawMessage) Result {
	return Result{Output: "ok"}
}

type snapshotDefinitionTool struct {
	definition model.FunctionTool
	calls      int
}

func (tool *snapshotDefinitionTool) Definition() model.FunctionTool {
	tool.calls++
	return tool.definition
}

func (*snapshotDefinitionTool) Run(context.Context, json.RawMessage) Result {
	return Result{Output: "ok"}
}
