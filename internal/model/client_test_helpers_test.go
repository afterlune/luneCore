package model

import (
	"context"
	"encoding/json"
)

type testToolExecutor struct {
	definitions   []ResponseTool
	calls         []OutputFunctionCall
	alwaysVisible map[string]bool
}

func (e *testToolExecutor) Definitions() []ResponseTool {
	return e.definitions
}

func (e *testToolExecutor) ExecuteCall(_ context.Context, call OutputFunctionCall) InputFunctionCallOutput {
	e.calls = append(e.calls, call)
	return InputFunctionCallOutput{CallID: call.CallID, Output: NewToolOutputText("tool result")}
}

func (e *testToolExecutor) ToolMetadata(name string) (ToolMetadata, bool) {
	switch name {
	case "read_file":
		return ToolMetadata{PermissionClass: ToolPermissionWorkspaceRead, ReadOnly: true, AlwaysVisible: e.alwaysVisible[name]}, true
	case "command":
		return ToolMetadata{PermissionClass: ToolPermissionCommand, AlwaysVisible: e.alwaysVisible[name]}, true
	case "web_search":
		return ToolMetadata{PermissionClass: ToolPermissionNetwork, ReadOnly: true, AlwaysVisible: e.alwaysVisible[name]}, true
	default:
		return ToolMetadata{}, false
	}
}

func newTestExecutor() *testToolExecutor {
	strict := true
	return &testToolExecutor{definitions: []ResponseTool{
		FunctionTool{
			Name:       "read_file",
			Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
			Strict:     &strict,
		},
	}}
}
