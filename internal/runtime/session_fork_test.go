package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
)

func TestForkPausedTaskClonesCheckpointAndDurableReceipts(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("ask", "ask_user", `{"questions":[{"id":"q1","question":"Continue?","options":[{"label":"yes","description":"Continue"},{"label":"no","description":"Stop"}]}]}`),
		hostTextResponse("fork completed"),
		hostTextResponse("source completed"),
	}}
	client := newProgrammaticCore(t, provider)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("ask me before continuing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunTask(ctx, TaskRequest{SessionID: source.ID, Input: input}); !errors.Is(err, ErrTaskPaused) {
		t.Fatalf("RunTask error = %v, want a user-question pause", err)
	}
	sourceRuntime, err := client.OpenSessionRuntime(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sourceQuestion UserQuestionRequest
	for _, request := range mustPendingRequests(t, sourceRuntime, ctx) {
		if request.UserQuestion != nil {
			sourceQuestion = *request.UserQuestion
		}
	}
	if sourceQuestion.InvocationID == "" || sourceQuestion.CallID == "" {
		t.Fatalf("source question = %#v", sourceQuestion)
	}
	answer, _ := json.Marshal([]UserQuestionResponse{{QuestionID: "q1", Answer: "yes"}})
	inserted, err := client.sessions.PutGraphResumeInput(ctx, source.ID, session.GraphResumeInput{
		RunID: sourceQuestion.RunID, InvocationID: sourceQuestion.InvocationID, CallID: sourceQuestion.CallID, Payload: answer,
	})
	if err != nil || !inserted {
		t.Fatalf("save source answer receipt: inserted=%t error=%v", inserted, err)
	}
	invocation := session.ToolInvocation{
		RunID: sourceQuestion.RunID, InvocationID: "fork-copy-receipt", SessionID: source.ID,
		ExecutionID: sourceQuestion.ExecutionID, AgentID: "lead", CallID: "completed-call", Name: "host_echo",
		Arguments: json.RawMessage(`{}`),
	}
	if _, _, err := client.sessions.StartToolInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if err := client.sessions.SaveToolInvocationOutput(ctx, invocation.RunID, invocation.InvocationID, session.ToolInvocationCompleted, json.RawMessage(`{"output":"saved"}`)); err != nil {
		t.Fatal(err)
	}

	fork, err := client.ForkPausedTask(ctx, source.ID)
	if err != nil {
		t.Fatalf("ForkPausedTask: %v", err)
	}
	forkRunID := client.runtime.CheckpointID(fork.ID)
	forkCheckpoint, err := client.checkpoints.Load(ctx, forkRunID)
	if err != nil {
		t.Fatal(err)
	}
	if forkCheckpoint.RunID != forkRunID || forkCheckpoint.Revision != 1 {
		t.Fatalf("fork checkpoint identity = run %q revision %d", forkCheckpoint.RunID, forkCheckpoint.Revision)
	}
	var forkExecutionID string
	for _, item := range forkCheckpoint.Invocations {
		if item.State.ExecutionID != "" {
			forkExecutionID = item.State.ExecutionID
			if item.State.RunID != forkRunID {
				t.Fatalf("fork invocation run id = %q, want %q", item.State.RunID, forkRunID)
			}
			break
		}
	}
	if forkExecutionID == "" || forkExecutionID == sourceQuestion.ExecutionID {
		t.Fatalf("fork execution id = %q, source = %q", forkExecutionID, sourceQuestion.ExecutionID)
	}
	clonedTool, err := client.sessions.GetToolInvocation(ctx, forkRunID, invocation.InvocationID)
	if err != nil || clonedTool.SessionID != fork.ID || clonedTool.ExecutionID != forkExecutionID || clonedTool.Status != session.ToolInvocationCompleted {
		t.Fatalf("forked tool receipt = %#v, error=%v", clonedTool, err)
	}
	forkInputs, err := client.sessions.GraphResumeInputs(ctx, fork.ID, forkRunID)
	if err != nil || len(forkInputs) != 1 || string(forkInputs[0].Payload) != string(answer) {
		t.Fatalf("forked continuation receipts = %#v, error=%v", forkInputs, err)
	}
	forkRuntime, err := client.OpenSessionRuntime(ctx, fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	var forkQuestion UserQuestionRequest
	for _, request := range mustPendingRequests(t, forkRuntime, ctx) {
		if request.UserQuestion != nil {
			forkQuestion = *request.UserQuestion
		}
	}
	if forkQuestion.RunID != forkRunID || forkQuestion.ExecutionID != forkExecutionID || forkQuestion.InvocationID != sourceQuestion.InvocationID || forkQuestion.CallID != sourceQuestion.CallID {
		t.Fatalf("fork question identity = %#v, source = %#v", forkQuestion, sourceQuestion)
	}
	if _, err := forkRuntime.Recover(ctx); err != nil {
		t.Fatalf("recover fork: %v", err)
	}
	forkResult, err := forkRuntime.WaitTurn(ctx, forkExecutionID)
	if err != nil || forkResult.Status != TaskCompleted {
		t.Fatalf("fork result = %#v, error=%v", forkResult, err)
	}
	if err := sourceRuntime.ResolveUserQuestion(ctx, UserQuestionAnswer{ExecutionID: sourceQuestion.ExecutionID, RunID: sourceQuestion.RunID, InvocationID: sourceQuestion.InvocationID, CallID: sourceQuestion.CallID, Answers: []UserQuestionResponse{{QuestionID: "q1", Answer: "yes"}}}); err != nil {
		t.Fatalf("resolve source question: %v", err)
	}
	sourceResult, err := sourceRuntime.WaitTurn(ctx, sourceQuestion.ExecutionID)
	if err != nil || sourceResult.Status != TaskCompleted {
		t.Fatalf("source result = %#v, error=%v", sourceResult, err)
	}
}

func mustPendingRequests(t *testing.T, runtime *SessionRuntime, ctx context.Context) []PendingRequest {
	t.Helper()
	requests, err := runtime.PendingRequests(ctx)
	if err != nil {
		t.Fatalf("PendingRequests: %v", err)
	}
	return requests
}
