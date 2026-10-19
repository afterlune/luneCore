package agentgraph

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
)

func TestCloneStateDeepCopiesMutableData(t *testing.T) {
	failure := publicmodel.Failure{Category: publicmodel.ErrorAuthentication}
	state := State{
		ModelFailure: &failure,
		PlanAuthorization: publictool.PlanAuthorization{
			Paths:                    []publictool.PathGrant{{Path: "data", Access: publictool.PathRead}},
			AllowedTools:             []string{"read_file"},
			ApprovedHighRiskCommands: []string{"rm file"},
		},
		History: *model.NewResponseInputItems(
			model.InputMessage{Role: model.RoleUser, Content: model.NewMessageContentParts(
				model.UnknownContentPart{Raw: json.RawMessage(`{"type":"future_input","payload":{"value":1}}`)},
				model.OutputText{Text: "visible", Annotations: []json.RawMessage{json.RawMessage(`{"kind":"source"}`)}},
			)},
			model.InputFunctionCallOutput{
				CallID:         "call-1",
				Output:         model.NewToolOutputParts(model.InputImage{FileID: "image-1"}),
				TranscriptOnly: []model.ContentPart{model.InputText{Text: "transcript only"}},
			},
			model.UnknownInputItem{Raw: json.RawMessage(`{"type":"future_item","payload":2}`)},
		),
		FinalResponse: &model.ModelResponse{Output: []model.ResponseOutputItem{
			model.OutputMessage{Content: []model.OutputContentPart{
				model.OutputText{Text: "answer", Annotations: []json.RawMessage{json.RawMessage(`{"kind":"citation"}`)}},
				model.UnknownOutputContentPart{Raw: json.RawMessage(`{"type":"future_part"}`)},
			}},
			model.UnknownOutputItem{Raw: json.RawMessage(`{"type":"future_output"}`)},
		}},
		UserQuestions: []model.UserQuestionItem{{ID: "decision", Options: []model.UserQuestionOption{{Label: "yes"}}}},
		TaskInput:     json.RawMessage(`{"task":"work"}`),
	}
	state.History.Items[1].(model.InputFunctionCallOutput).TranscriptOnly[0] = model.InputText{Text: "transcript only"}

	clone, err := cloneState(state)
	if err != nil {
		t.Fatal(err)
	}
	jsonClone, err := graph.JSONClone[State](state)
	if err != nil {
		t.Fatal(err)
	}
	cloneJSON, err := json.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	jsonCloneJSON, err := json.Marshal(jsonClone)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cloneJSON, jsonCloneJSON) {
		t.Fatalf("clone JSON differs from checkpoint clone:\n%s\n%s", cloneJSON, jsonCloneJSON)
	}
	clone.PlanAuthorization.Paths[0].Path = "changed"
	clone.PlanAuthorization.AllowedTools[0] = "write_file"
	clone.PlanAuthorization.ApprovedHighRiskCommands[0] = "changed"
	clone.History.Items[0].(model.InputMessage).Content.Parts[0].(model.UnknownContentPart).Raw[30] = 'x'
	clone.History.Items[0].(model.InputMessage).Content.Parts[1].(model.OutputText).Annotations[0][9] = 'x'
	clone.History.Items[1].(model.InputFunctionCallOutput).TranscriptOnly[0] = model.InputText{Text: "changed"}
	clone.History.Items[2].(model.UnknownInputItem).Raw[20] = 'x'
	clone.FinalResponse.Output[0].(model.OutputMessage).Content[0].(model.OutputText).Annotations[0][9] = 'x'
	clone.FinalResponse.Output[0].(model.OutputMessage).Content[1].(model.UnknownOutputContentPart).Raw[15] = 'x'
	clone.FinalResponse.Output[1].(model.UnknownOutputItem).Raw[15] = 'x'
	clone.UserQuestions[0].Options[0].Label = "changed"
	clone.TaskInput[9] = 'x'
	clone.ModelFailure.Category = "changed"

	if state.PlanAuthorization.Paths[0].Path != "data" || state.PlanAuthorization.AllowedTools[0] != "read_file" || state.PlanAuthorization.ApprovedHighRiskCommands[0] != "rm file" {
		t.Error("plan authorization was shared with the clone")
	}
	if state.History.Items[0].(model.InputMessage).Content.Parts[0].(model.UnknownContentPart).Raw[30] == 'x' || state.History.Items[0].(model.InputMessage).Content.Parts[1].(model.OutputText).Annotations[0][9] == 'x' {
		t.Error("input message content was shared with the clone")
	}
	if state.History.Items[1].(model.InputFunctionCallOutput).TranscriptOnly[0].(model.InputText).Text != "transcript only" {
		t.Error("transcript-only content was shared with the clone")
	}
	if state.History.Items[2].(model.UnknownInputItem).Raw[20] == 'x' {
		t.Error("unknown input item was shared with the clone")
	}
	if state.FinalResponse.Output[0].(model.OutputMessage).Content[0].(model.OutputText).Annotations[0][9] == 'x' || state.FinalResponse.Output[0].(model.OutputMessage).Content[1].(model.UnknownOutputContentPart).Raw[15] == 'x' || state.FinalResponse.Output[1].(model.UnknownOutputItem).Raw[15] == 'x' {
		t.Error("response output was shared with the clone")
	}
	if state.UserQuestions[0].Options[0].Label != "yes" || string(state.TaskInput) != `{"task":"work"}` || state.ModelFailure.Category == "changed" {
		t.Error("state metadata was shared with the clone")
	}
}

func TestCloneStateRejectsInvalidSerializedFields(t *testing.T) {
	if _, err := cloneState(State{TaskInput: json.RawMessage("not-json")}); err == nil {
		t.Fatal("cloneState accepted invalid task input JSON")
	}
	if _, err := cloneState(State{History: model.ResponseInput{Text: new(string), Items: []model.InputItem{}}}); err == nil {
		t.Fatal("cloneState accepted invalid history shape")
	}
}

func TestCloneStateCopiesPointerModelItems(t *testing.T) {
	inputMessage := &model.InputMessage{
		Role:    model.RoleUser,
		Content: model.NewMessageContentParts(&model.InputText{Text: "input"}),
	}
	inputCall := &model.InputFunctionCall{CallID: "call-1", Name: "read_file", Arguments: `{}`}
	toolOutput := &model.InputFunctionCallOutput{
		CallID:         "call-1",
		Output:         model.NewToolOutputText("tool output"),
		TranscriptOnly: []model.ContentPart{&model.InputText{Text: "transcript"}},
	}
	responseCall := &model.OutputFunctionCall{CallID: "call-2", Name: "write_file", Arguments: `{}`}
	state := State{
		History:       *model.NewResponseInputItems(inputMessage, inputCall, toolOutput),
		FinalResponse: &model.ModelResponse{Output: []model.ResponseOutputItem{responseCall}},
	}
	clone, err := cloneState(state)
	if err != nil {
		t.Fatal(err)
	}
	clone.History.Items[0].(*model.InputMessage).Content.Parts[0].(*model.InputText).Text = "changed input"
	clone.History.Items[1].(*model.InputFunctionCall).Name = "changed tool"
	clone.History.Items[2].(*model.InputFunctionCallOutput).TranscriptOnly[0].(*model.InputText).Text = "changed transcript"
	clone.FinalResponse.Output[0].(*model.OutputFunctionCall).Name = "changed response"
	if inputMessage.Content.Parts[0].(*model.InputText).Text != "input" || inputCall.Name != "read_file" || toolOutput.TranscriptOnly[0].(*model.InputText).Text != "transcript" || responseCall.Name != "write_file" {
		t.Fatal("pointer model items were shared with the clone")
	}
}

func TestCloneStatePreservesNilUnionItemsAsUnknownItems(t *testing.T) {
	state := State{
		History:       *model.NewResponseInputItems(nil),
		FinalResponse: &model.ModelResponse{Output: []model.ResponseOutputItem{nil}},
	}
	clone, err := cloneState(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := clone.History.Items[0].(model.UnknownInputItem); !ok {
		t.Fatalf("nil input item cloned as %T, want UnknownInputItem", clone.History.Items[0])
	}
	if _, ok := clone.FinalResponse.Output[0].(model.UnknownOutputItem); !ok {
		t.Fatalf("nil response item cloned as %T, want UnknownOutputItem", clone.FinalResponse.Output[0])
	}
	jsonClone, err := graph.JSONClone[State](state)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(jsonClone)
	if err != nil {
		t.Fatal(err)
	}
	clonedEncoded, err := json.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, clonedEncoded) {
		t.Fatalf("nil item clone JSON differs from JSON clone:\n%s\n%s", clonedEncoded, encoded)
	}
}
