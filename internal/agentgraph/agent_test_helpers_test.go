package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

type scriptedRequester struct {
	responses []*model.Response
	requests  []model.ResponseRequest
	next      int
}

func (r *scriptedRequester) CreateResponse(_ context.Context, request model.ResponseRequest) (*model.Response, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var saved model.ResponseRequest
	if err := json.Unmarshal(encoded, &saved); err != nil {
		return nil, err
	}
	if request.PromptCache != nil {
		hint := *request.PromptCache
		saved.PromptCache = &hint
	}
	r.requests = append(r.requests, saved)
	if r.next >= len(r.responses) {
		return nil, errors.New("no scripted response left")
	}
	response := r.responses[r.next]
	r.next++
	return response, nil
}

type recordingTool struct{ calls int }

func (recordingTool) Definition() model.FunctionTool {
	return model.FunctionTool{
		Name:        "read_file",
		Description: "Read a workspace file.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}
}

func (t *recordingTool) Run(context.Context, json.RawMessage) tool.Result {
	t.calls++
	return tool.Result{Output: "file contents"}
}

type namedRecordingTool struct {
	name  string
	calls int
}

func (t *namedRecordingTool) Definition() model.FunctionTool {
	return model.FunctionTool{
		Name:        t.name,
		Description: "A test tool.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}
}

func (t *namedRecordingTool) Run(context.Context, json.RawMessage) tool.Result {
	t.calls++
	return tool.Result{Output: t.name + " completed"}
}

func toolRegistration(registeredTool tool.Tool, category model.ToolCategory, permissionClass model.ToolPermissionClass, readOnly bool) tool.ToolRegistration {
	return tool.ToolRegistration{
		Tool: registeredTool,
		Metadata: model.ToolMetadata{
			Category: category, PermissionClass: permissionClass, ReadOnly: readOnly,
		},
	}
}

func decodeResponse(raw string) (*model.Response, error) {
	var response model.Response
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func mustDecodeResponse(t *testing.T, raw string) *model.Response {
	t.Helper()
	response, err := decodeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
