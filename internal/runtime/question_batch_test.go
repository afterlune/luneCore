package runtime

import (
	"context"
	"encoding/json"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestQuestionBatchDraftSurvivesRestartAndOnlyCompleteAnswersResume(t *testing.T) {
	ctx := context.Background()
	questions := []UserQuestionItem{
		{ID: "target", Question: "Which target?", Options: []UserQuestionOption{{Label: "Local"}, {Label: "Remote"}}},
		{ID: "scope", Question: "Which scope?", Options: []UserQuestionOption{{Label: "All"}}},
	}
	encoded, _ := json.Marshal(map[string]any{"questions": questions})
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostToolCallResponse("batch", "ask_user", string(encoded)), hostTextResponse("Complete")}}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	c, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			c.Close(context.Background())
		}
	}()
	saved, err := c.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := c.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := runtime.SubmitTurn(ctx, TurnRequest{Input: mustSettingsTaskInput(t, "prepare")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	wait := waitPendingUserQuestion(t, runtime, submitted.ExecutionID)
	paused := waitTurnResult(t, waitTurnAsync(runtime, submitted.ExecutionID))
	if paused.err != nil || paused.result.Status != TaskPaused {
		t.Fatalf("pause=%#v", paused)
	}
	identity := UserQuestionAnswer{ExecutionID: wait.ExecutionID, RunID: wait.RunID, InvocationID: wait.InvocationID, CallID: wait.CallID}
	draft := UserQuestionDraft{CurrentQuestionID: "scope", Responses: []UserQuestionDraftResponse{
		{QuestionID: "target", SelectedOption: 1, Notes: "keep context", Confirmed: true},
		{QuestionID: "scope", SelectedOption: 1, CustomAnswer: "Only storage"},
	}}
	if err := runtime.SaveUserQuestionDraft(ctx, identity, draft); err != nil {
		t.Fatal(err)
	}
	identity.Answers = []UserQuestionResponse{{QuestionID: "target", Answer: "Remote"}}
	if err := runtime.ResolveUserQuestion(ctx, identity); err == nil {
		t.Fatal("partial batch resumed")
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 1 {
		t.Fatal("saving draft resumed model")
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	c = nil
	reopened, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	c = reopened
	runtime, err = c.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	wait = waitPendingUserQuestion(t, runtime, submitted.ExecutionID)
	if len(wait.Questions) != 2 || wait.Draft == nil || wait.Draft.CurrentQuestionID != "scope" || !wait.Draft.Responses[0].Confirmed || wait.Draft.Responses[1].CustomAnswer != "Only storage" {
		t.Fatalf("restored=%#v", wait)
	}
	identity.Answers = []UserQuestionResponse{{QuestionID: "target", Answer: "Remote\n\nkeep context"}, {QuestionID: "scope", Answer: "Only storage"}}
	if err := runtime.ResolveUserQuestion(ctx, identity); err != nil {
		t.Fatal(err)
	}
	done := waitTurnResult(t, waitTurnAsync(runtime, submitted.ExecutionID))
	if done.err != nil || done.result.Status != TaskCompleted {
		t.Fatalf("done=%#v", done)
	}
	if err := runtime.ResolveUserQuestion(ctx, identity); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	identity.Answers[1].Answer = "Different"
	if err := runtime.ResolveUserQuestion(ctx, identity); err == nil {
		t.Fatal("conflicting receipt accepted")
	}
	if err := runtime.SaveUserQuestionDraft(ctx, identity, draft); err == nil {
		t.Fatal("stale draft accepted")
	}
	raw, err := c.sessions.UserQuestionDraft(ctx, saved.ID, wait.RunID, wait.InvocationID, wait.CallID)
	if err != nil || len(raw) != 0 {
		t.Fatalf("draft not cleaned: %s, %v", raw, err)
	}
}
