package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
)

func TestPruneCompletedAcceptanceContextKeepsTranscriptInputAndCandidate(t *testing.T) {
	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("current user request")},
		model.UnknownInputItem{Raw: json.RawMessage(`{"type":"function_call","call_id":"criteria-1","name":"prepare_task_criteria","arguments":"{}"}`)},
		model.InputFunctionCallOutput{CallID: "criteria-1", Output: model.NewToolOutputText("Acceptance criteria saved for this execution.")},
		model.UnknownInputItem{Raw: json.RawMessage(`{"type":"function_call","call_id":"read-1","name":"read_file","arguments":"{}"}`)},
		model.InputFunctionCallOutput{CallID: "read-1", Output: model.NewToolOutputText("useful file result")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText("accepted candidate")},
		model.UnknownInputItem{Raw: json.RawMessage(`{"type":"function_call","call_id":"report-1","name":"submit_acceptance","arguments":"{}"}`)},
		model.InputFunctionCallOutput{CallID: "report-1", Output: model.NewToolOutputText("Acceptance report received.")},
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(agentgraph.InternalAcceptancePromptPrefix + "The latest acceptance report failed.")},
	)
	original, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	pruned := PruneCompletedAcceptanceContext(history)
	encoded, err := json.Marshal(pruned)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"current user request", "useful file result", "accepted candidate"} {
		if !strings.Contains(string(encoded), value) {
			t.Errorf("pruned context lost %q: %s", value, encoded)
		}
	}
	for _, value := range []string{"prepare_task_criteria", "submit_acceptance", "Acceptance criteria saved", "Acceptance report received", agentgraph.InternalAcceptancePromptPrefix} {
		if strings.Contains(string(encoded), value) {
			t.Errorf("pruned context retained internal acceptance content %q: %s", value, encoded)
		}
	}
	after, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("pruning mutated the full conversation transcript")
	}
}
