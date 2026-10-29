package runtime

import (
	"context"
	"encoding/json"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestUserQuestionDraftPayloadPersistence(t *testing.T) {
	ctx := context.Background()
	questions := []UserQuestionItem{
		{ID: "choice", Question: "Pick an option", Options: []UserQuestionOption{{Label: "A"}, {Label: "B"}}},
	}
	encoded, _ := json.Marshal(map[string]any{"questions": questions})
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("call-1", "ask_user", string(encoded)),
		hostTextResponse("Done"),
	}}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	c, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())

	saved, err := c.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := c.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}

	submitted, err := runtime.SubmitTurn(ctx, TurnRequest{Input: mustSettingsTaskInput(t, "start")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}

	wait := waitPendingUserQuestion(t, runtime, submitted.ExecutionID)
	identity := UserQuestionAnswer{
		ExecutionID:  wait.ExecutionID,
		RunID:        wait.RunID,
		InvocationID: wait.InvocationID,
		CallID:       wait.CallID,
	}

	// 1. Test arbitrary JSON draft payload
	customPayload := []byte(`{"custom_form":{"wizard_step":3,"notes":"in-progress","answers":{"choice":"A"}}}`)
	if err := runtime.SaveUserQuestionDraftPayload(ctx, identity, customPayload); err != nil {
		t.Fatalf("SaveUserQuestionDraftPayload: %v", err)
	}

	// Read pending requests
	reqs, err := runtime.PendingRequests(ctx)
	if err != nil {
		t.Fatalf("PendingRequests: %v", err)
	}
	var foundQuestion *UserQuestionRequest
	for _, req := range reqs {
		if req.Kind == PendingUserQuestion {
			foundQuestion = req.UserQuestion
			break
		}
	}
	if foundQuestion == nil {
		t.Fatal("expected pending user question request")
	}
	if string(foundQuestion.DraftPayload) != string(customPayload) {
		t.Fatalf("DraftPayload = %s, want %s", string(foundQuestion.DraftPayload), string(customPayload))
	}

	// 2. Test saving partial structured draft without radio constraints
	partialDraft := UserQuestionDraft{
		Responses: []UserQuestionDraftResponse{
			{QuestionID: "choice", SelectedOption: -1, Notes: "still thinking"},
		},
	}
	if err := runtime.SaveUserQuestionDraft(ctx, identity, partialDraft); err != nil {
		t.Fatalf("SaveUserQuestionDraft partial: %v", err)
	}

	reqs, err = runtime.PendingRequests(ctx)
	if err != nil {
		t.Fatalf("PendingRequests: %v", err)
	}
	for _, req := range reqs {
		if req.Kind == PendingUserQuestion {
			foundQuestion = req.UserQuestion
			break
		}
	}
	if foundQuestion.Draft == nil || len(foundQuestion.Draft.Responses) != 1 || foundQuestion.Draft.Responses[0].SelectedOption != -1 {
		t.Fatalf("unexpected restored draft: %+v", foundQuestion.Draft)
	}
}
