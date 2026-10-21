package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

type memoryFailureProvider struct {
	response *publicmodel.ModelResponse
	err      error
}

func (*memoryFailureProvider) Name() string { return "host" }
func (p *memoryFailureProvider) CreateResponse(_ context.Context, req publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	for _, tool := range req.Tools {
		if fn, ok := tool.(publicmodel.FunctionTool); ok && fn.Name == "propose_workspace_memories" {
			return p.response, p.err
		}
	}
	return hostTextResponse("done"), nil
}
func (p *memoryFailureProvider) StreamResponse(ctx context.Context, req publicmodel.ModelRequest, profile publicmodel.ModelProfile, _ func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	return p.CreateResponse(ctx, req, profile)
}

func TestMemoryFailuresPersistSafeDiagnosticsAndKeepTaskComplete(t *testing.T) {
	for _, test := range []struct {
		name, stage string
		response    *publicmodel.ModelResponse
		err         error
	}{
		{"provider", "request", nil, &publicmodel.RequestError{Category: publicmodel.ErrorInvalidRequest, StatusCode: 400, Code: "invalid_request_error", RequestID: "req_memory", Cause: errors.New("private provider payload")}},
		{"submission", "submission", hostTextResponse("private model output"), nil},
		{"validation", "validation", memorySubmissionResponse(`[{"category":"preference","content":"private memory content","importance":3,"basis":"unsupported private basis"}]`), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newMemoryTestCore(t, &memoryFailureProvider{response: test.response, err: test.err}, MemoryConfig{})
			var logs bytes.Buffer
			client.logger = slog.New(slog.NewTextHandler(&logs, nil))
			ctx := context.Background()
			saved, err := client.CreateSession(ctx, json.RawMessage("null"))
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "private user input")})
			if err != nil || result.Status != TaskCompleted {
				t.Fatalf("task=%s error=%v", result.Status, err)
			}
			events, err := client.sessions.RuntimeEventsAfter(ctx, saved.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var failed Event
			for _, stored := range events {
				var event Event
				if err := json.Unmarshal(stored.Payload, &event); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(stored.Payload), "private") {
					t.Fatal("private contents persisted in events")
				}
				if event.Kind == EventMemoryFailed {
					failed = event
				}
			}
			if failed.Err != "memory extraction failed: "+test.stage || !strings.Contains(logs.String(), "workspace memory extraction failed") || strings.Contains(logs.String(), "private") {
				t.Fatalf("missing or unsafe diagnostic: %+v %s", failed, logs.String())
			}
			if test.name == "provider" && (failed.ModelFailure == nil || failed.ModelFailure.StatusCode != 400 || failed.ModelFailure.Code != "invalid_request_error" || failed.ModelFailure.RequestID != "req_memory") {
				t.Fatalf("model failure=%+v", failed.ModelFailure)
			}
			items, err := client.ListMemories(ctx, 10)
			if err != nil || len(items) != 0 {
				t.Fatal("failed extraction saved memory")
			}
		})
	}
}

func TestMemoryDiagnosticProjectionRejectsArbitraryErrors(t *testing.T) {
	if got := safeEventProjection(Event{Kind: EventMemoryFailed, Err: "private raw error"}); got.Err != "" {
		t.Fatal("unsafe error persisted")
	}
	if got := memoryFailureIdentifier("private token=secret"); got != "" {
		t.Fatal("unsafe identifier retained")
	}
}
