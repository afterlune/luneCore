package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/memory"
	publicmodel "github.com/afterlune/luneCore/model"
	graph "github.com/afterlune/luneGraph"
)

// Models may support reasoning and strict tools while rejecting forced choices.
type reasoningMemoryRequester struct{ scriptedRequester }

func (r *reasoningMemoryRequester) CreateResponse(ctx context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	if request.Reasoning != nil && request.Reasoning.Effort != "none" && request.ToolChoice != nil {
		return nil, &publicmodel.RequestError{Category: publicmodel.ErrorInvalidRequest, StatusCode: 400}
	}
	return r.scriptedRequester.CreateResponse(ctx, request)
}

func TestMemoryExtractionWithReasoningModel(t *testing.T) {
	a, _ := retryTestAgent()
	a.options.MemoryRequester = &reasoningMemoryRequester{scriptedRequester{responses: []*model.ModelResponse{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"memory","name":"propose_workspace_memories","arguments":"{\"items\":[]}"}]}`),
	}}}
	var status string
	a.options.MemoryObserver = func(_ State, value string, _ []string, _ error) { status = value }
	_, err := a.extractMemory(context.Background(), graph.CallInfo{}, State{ReasoningEffort: "high"})
	if err != nil || status != "skipped" {
		t.Fatalf("reasoning model extraction: status=%s error=%v", status, err)
	}
}

func TestMemoryExtractionReportsFailurePhase(t *testing.T) {
	submission := func(arguments string) *model.ModelResponse {
		args, _ := json.Marshal(arguments)
		return mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"memory","name":"propose_workspace_memories","arguments":`+string(args)+`}]}`)
	}
	valid := `{"items":[{"category":"preference","content":"Use concise Chinese","importance":3,"basis":"Use concise Chinese"}]}`
	private := errors.New("private provider or storage contents")
	for _, test := range []struct {
		name, stage, status string
		requester           Requester
		writeErr            error
	}{
		{"request", "request", "failed", &retryRequester{failures: 1, err: &publicmodel.RequestError{Category: publicmodel.ErrorInvalidRequest, Cause: private}}, nil},
		{"response", "response", "failed", &scriptedRequester{responses: []*model.ModelResponse{nil}}, nil},
		{"missing call", "submission", "failed", &scriptedRequester{responses: []*model.ModelResponse{{Status: "completed"}}}, nil},
		{"invalid JSON", "submission", "failed", &scriptedRequester{responses: []*model.ModelResponse{submission(`{"private":`)}}, nil},
		{"untrusted basis", "validation", "failed", &scriptedRequester{responses: []*model.ModelResponse{submission(strings.ReplaceAll(valid, `"basis":"Use concise Chinese"`, `"basis":"unsupported private source"`))}}, nil},
		{"persistence", "persistence", "failed", &scriptedRequester{responses: []*model.ModelResponse{submission(valid)}}, private},
		{"empty", "", "skipped", &scriptedRequester{responses: []*model.ModelResponse{submission(`{"items":[]}`)}}, nil},
		{"saved", "", "saved", &scriptedRequester{responses: []*model.ModelResponse{submission(valid)}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, _ := retryTestAgent()
			a.options.Execution.ModelMaxAttempts = 1
			a.options.MemoryRequester = test.requester
			a.options.MemoryWriter = func(context.Context, State, []memory.Candidate) ([]string, error) {
				return []string{"memory-id"}, test.writeErr
			}
			var status string
			var observed error
			a.options.MemoryObserver = func(_ State, value string, _ []string, err error) { status, observed = value, err }
			_, err := a.extractMemory(context.Background(), graph.CallInfo{}, State{TaskInput: json.RawMessage(`"Use concise Chinese"`)})
			if err != nil || status != test.status {
				t.Fatalf("status=%s error=%v", status, err)
			}
			if test.stage == "" {
				if observed != nil {
					t.Fatal(observed)
				}
				return
			}
			var extraction *MemoryExtractionError
			if !errors.As(observed, &extraction) || extraction.Stage != test.stage || strings.Contains(observed.Error(), "private") {
				t.Fatalf("unsafe or missing phase: %v", observed)
			}
		})
	}
}
