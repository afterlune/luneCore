package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestSessionRuntimeResumesDurableUserQuestion(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		askUserToolCallResponse("QUESTION-SECRET-7381", []string{"OPTION-LOCAL-7381", "OPTION-STAGING-7381"}),
		hostTextResponse("I will use the target you chose."),
	}}
	client, sessionID := newQuestionTestCore(t, provider)
	if err := client.SetPermissionMode(PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "prepare deployment")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	question := waitPendingUserQuestion(t, runtime, submission.ExecutionID)
	if question.AgentID != "lead" || question.RunID == "" || question.InvocationID == "" || question.CallID == "" ||
		question.Questions[0].Question != "QUESTION-SECRET-7381" || len(question.Questions[0].Options) != 2 || question.Questions[0].Options[0].Label != "OPTION-LOCAL-7381" || question.Questions[0].Options[1].Label != "OPTION-STAGING-7381" {
		t.Fatalf("pending question = %#v", question)
	}
	streamCtx, cancelStream := context.WithCancel(context.Background())
	events, err := runtime.SubscribeEvents(streamCtx, 0)
	if err != nil {
		cancelStream()
		t.Fatal(err)
	}
	defer func() {
		cancelStream()
		for range events {
		}
	}()
	var requestEvent Event
	deadline := time.After(5 * time.Second)
	for requestEvent.Kind != EventUserQuestionRequired {
		select {
		case event := <-events:
			if event.Kind == EventUserQuestionRequired {
				requestEvent = event
			}
		case <-deadline:
			t.Fatal("user question event was not replayed")
		}
	}
	if requestEvent.UserQuestion == nil || requestEvent.UserQuestion.ID != question.ID || len(requestEvent.UserQuestion.Questions) != 0 || requestEvent.UserQuestion.Draft != nil {
		t.Fatalf("durable user question event leaked its payload or lost its ID: %#v", requestEvent)
	}

	answer := "ANSWER-PRIVATE-7381"
	if err := runtime.ResolveUserQuestion(context.Background(), UserQuestionAnswer{ExecutionID: question.ExecutionID, RunID: question.RunID, InvocationID: question.InvocationID, CallID: question.CallID, Answers: []UserQuestionResponse{{QuestionID: "q1", Answer: answer}}}); err != nil {
		t.Fatal(err)
	}
	completed := waitTurnResult(t, waitTurnAsync(runtime, submission.ExecutionID))
	if completed.err == nil && completed.result.Status == TaskPaused {
		completed = waitTurnResult(t, waitTurnAsync(runtime, submission.ExecutionID))
	}
	if completed.err != nil || completed.result.Status != TaskCompleted || len(completed.result.Response) == 0 {
		t.Fatalf("WaitTurn after answer = %#v, error=%v", completed.result, completed.err)
	}
	answerInput := UserQuestionAnswer{ExecutionID: question.ExecutionID, RunID: question.RunID, InvocationID: question.InvocationID, CallID: question.CallID, Answers: []UserQuestionResponse{{QuestionID: "q1", Answer: answer}}}
	if err := runtime.ResolveUserQuestion(context.Background(), answerInput); err != nil {
		t.Fatalf("identical answer retry: %v", err)
	}
	answerInput.Answers[0].Answer = "different answer"
	if err := runtime.ResolveUserQuestion(context.Background(), answerInput); err == nil {
		t.Fatal("conflicting answer retry was accepted")
	}
	answerInput = UserQuestionAnswer{ExecutionID: question.ExecutionID, RunID: question.RunID, InvocationID: "stale-invocation", CallID: question.CallID, Answers: []UserQuestionResponse{{QuestionID: "q1", Answer: answer}}}
	if err := runtime.ResolveUserQuestion(context.Background(), answerInput); err == nil {
		t.Fatal("stale wait identity was accepted")
	}
	provider.mu.Lock()
	seenRequests := append([]publicmodel.ModelRequest(nil), provider.seenRequests...)
	provider.mu.Unlock()
	if len(seenRequests) != 2 {
		t.Fatalf("provider received %d requests, want ask and synthesis", len(seenRequests))
	}
	encodedInput, err := json.Marshal(seenRequests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encodedInput), answer) {
		t.Fatalf("model did not receive the user's free-text answer: %s", encodedInput)
	}
	storedEvents, err := client.sessions.RuntimeEventsAfter(context.Background(), sessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range storedEvents {
		for _, private := range []string{question.Questions[0].Question, question.Questions[0].Options[0].Label, question.Questions[0].Options[1].Label, answer} {
			if strings.Contains(string(event.Payload), private) {
				t.Fatalf("user question content was persisted in event log: %s", event.Payload)
			}
		}
	}
}

func TestUserQuestionAnswerResumesAfterCoreRestart(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		askUserToolCallResponse("Which environment?", []string{"local", "staging"}),
		hostTextResponse("Deployment plan ready."),
	}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	config := runtimeTestConfig(workspace, stateDir, provider)
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "prepare deployment")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	pausedWait := waitTurnAsync(runtime, submission.ExecutionID)
	firstQuestion := waitPendingUserQuestion(t, runtime, submission.ExecutionID)
	paused := waitTurnResult(t, pausedWait)
	if paused.err != nil || paused.result.Status != TaskPaused {
		t.Fatalf("WaitTurn while waiting for clarification = %#v, error=%v", paused.result, paused.err)
	}
	client.mu.RLock()
	liveExecution := client.liveExecutions[client.runtime.CheckpointID(saved.ID)]
	client.mu.RUnlock()
	if liveExecution == nil {
		t.Fatal("paused task did not retain its live Graph execution")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close paused Core: %v", err)
	}

	reopened, err := New(config)
	if err != nil {
		t.Fatalf("reopen Core: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened Core: %v", err)
		}
	})
	runtime, err = reopened.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.liveExecutions) != 0 {
		t.Fatal("new Core unexpectedly retained a live Graph execution")
	}
	secondQuestion := waitPendingUserQuestion(t, runtime, submission.ExecutionID)
	if secondQuestion.ID != firstQuestion.ID || secondQuestion.RunID != firstQuestion.RunID ||
		secondQuestion.InvocationID != firstQuestion.InvocationID || secondQuestion.CallID != firstQuestion.CallID ||
		secondQuestion.Questions[0].Question != firstQuestion.Questions[0].Question || !slices.Equal(secondQuestion.Questions[0].Options, firstQuestion.Questions[0].Options) {
		t.Fatalf("question after restart = %#v, before restart = %#v", secondQuestion, firstQuestion)
	}
	if err := runtime.ResolveUserQuestion(context.Background(), UserQuestionAnswer{ExecutionID: secondQuestion.ExecutionID, RunID: secondQuestion.RunID, InvocationID: secondQuestion.InvocationID, CallID: secondQuestion.CallID, Answers: []UserQuestionResponse{{QuestionID: "q1", Answer: "staging"}}}); err != nil {
		t.Fatal(err)
	}
	completed := waitTurnResult(t, waitTurnAsync(runtime, submission.ExecutionID))
	if completed.err != nil || completed.result.Status != TaskCompleted || completed.result.ExecutionID != submission.ExecutionID {
		t.Fatalf("WaitTurn after recovery = %#v, error=%v", completed.result, completed.err)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 2 {
		t.Fatalf("model requests after recovery = %d, want original request and final synthesis", requests)
	}
}

func newQuestionTestCore(t *testing.T, provider *scriptedHostProvider) (*Core, string) {
	t.Helper()
	workspace, stateDir := t.TempDir(), t.TempDir()
	client, err := New(runtimeTestConfig(workspace, stateDir, provider))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Core: %v", err)
		}
	})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	return client, saved.ID
}

func waitPendingUserQuestion(t *testing.T, runtime *SessionRuntime, executionID string) UserQuestionRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := runtime.PendingRequests(context.Background())
		if err == nil {
			for _, request := range pending {
				if request.Kind == PendingUserQuestion && request.UserQuestion != nil && request.UserQuestion.ExecutionID == executionID {
					return *request.UserQuestion
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	pending, err := runtime.PendingRequests(context.Background())
	t.Fatalf("pending user question for execution %q not found: %#v, error=%v", executionID, pending, err)
	return UserQuestionRequest{}
}

type turnWaitResult struct {
	result TurnResult
	err    error
}

func waitTurnAsync(runtime *SessionRuntime, executionID string) <-chan turnWaitResult {
	result := make(chan turnWaitResult, 1)
	go func() {
		value, err := runtime.WaitTurn(context.Background(), executionID)
		result <- turnWaitResult{result: value, err: err}
	}()
	return result
}

func waitTurnResult(t *testing.T, result <-chan turnWaitResult) turnWaitResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("WaitTurn did not return")
		return turnWaitResult{}
	}
}

func askUserToolCallResponse(question string, options []string) *publicmodel.ModelResponse {
	choices := make([]publicmodel.UserQuestionOption, len(options))
	for i, label := range options {
		choices[i] = publicmodel.UserQuestionOption{Label: label}
	}
	arguments, _ := json.Marshal(map[string]any{"questions": []publicmodel.UserQuestionItem{{ID: "q1", Question: question, Options: choices}}})
	return hostToolCallResponse("ask-call", "ask_user", string(arguments))
}

func TestSuccessiveQuestionsInOneSessionDoNotReuseAnswerReceipts(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		askUserToolCallResponse("First target?", []string{"one", "two"}), hostTextResponse("first complete"),
		askUserToolCallResponse("Second target?", []string{"three", "four"}), hostTextResponse("second complete"),
	}}
	client, sessionID := newQuestionTestCore(t, provider)
	runtime, err := client.OpenSessionRuntime(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var previous UserQuestionAnswer
	for i, answer := range []string{"one", "four"} {
		submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "choose a target")}, StartIfIdle)
		if err != nil {
			t.Fatal(err)
		}
		question := waitPendingUserQuestion(t, runtime, submission.ExecutionID)
		if i > 0 {
			if err := runtime.ResolveUserQuestion(context.Background(), previous); err == nil {
				t.Fatal("old execution answered a new question")
			}
		}
		input := UserQuestionAnswer{ExecutionID: question.ExecutionID, RunID: question.RunID, InvocationID: question.InvocationID, CallID: question.CallID, Answers: []UserQuestionResponse{{QuestionID: "q1", Answer: answer}}}
		if err := runtime.ResolveUserQuestion(context.Background(), input); err != nil {
			t.Fatalf("question %d: %v", i+1, err)
		}
		result := waitTurnResult(t, waitTurnAsync(runtime, submission.ExecutionID))
		if result.err != nil || result.result.Status != TaskCompleted {
			t.Fatalf("question %d result=%+v err=%v", i+1, result.result, result.err)
		}
		previous = input
	}
}
