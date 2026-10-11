package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestSessionRuntimeAskUserReturnsAnswerToModel(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		askUserToolCallResponse("QUESTION-SECRET-7381", []string{"OPTION-LOCAL-7381", "OPTION-STAGING-7381"}),
		hostTextResponse("I will use the target you chose."),
	}}
	client, sessionID := newQuestionTestCore(t, provider)
	runtime, err := client.OpenSessionRuntime(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "prepare deployment")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	resultCh := waitTurnAsync(runtime, submission.ExecutionID)
	question := waitPendingUserQuestion(t, runtime, submission.ExecutionID)
	if question.AgentID != "lead" || question.Question != "QUESTION-SECRET-7381" || len(question.Options) != 2 || question.Options[0] != "OPTION-LOCAL-7381" || question.Options[1] != "OPTION-STAGING-7381" {
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
	if requestEvent.UserQuestion == nil || requestEvent.UserQuestion.ID != question.ID || requestEvent.UserQuestion.Question != "" || len(requestEvent.UserQuestion.Options) != 0 {
		t.Fatalf("durable user question event leaked its payload or lost its ID: %#v", requestEvent)
	}
	answer := "ANSWER-PRIVATE-7381"
	if err := runtime.ResolveUserQuestion(question.ID, answer); err != nil {
		t.Fatal(err)
	}
	result := waitTurnResult(t, resultCh)
	if result.err != nil || result.result.Status != TaskCompleted || len(result.result.Response) == 0 {
		t.Fatalf("WaitTurn = %#v, error=%v", result.result, result.err)
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
		for _, private := range []string{question.Question, question.Options[0], question.Options[1], answer} {
			if strings.Contains(string(event.Payload), private) {
				t.Fatalf("user question content was persisted in event log: %s", event.Payload)
			}
		}
	}
}

func TestUserQuestionIsReaskedAfterCoreRestartRecovery(t *testing.T) {
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
	firstWait := waitTurnAsync(runtime, submission.ExecutionID)
	firstQuestion := waitPendingUserQuestion(t, runtime, submission.ExecutionID)
	if !runtime.Interrupt(submission.ExecutionID) {
		t.Fatal("Interrupt returned false while a user question was pending")
	}
	paused := waitTurnResult(t, firstWait)
	if paused.err != nil || paused.result.Status != TaskPaused {
		t.Fatalf("WaitTurn after interrupt = %#v, error=%v", paused.result, paused.err)
	}
	events, err := client.sessions.RuntimeEventsAfter(context.Background(), saved.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var sawQuestion, sawCancellation bool
	for _, stored := range events {
		var event Event
		if err := json.Unmarshal(stored.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == EventUserQuestionRequired && event.UserQuestion != nil && event.UserQuestion.ID == firstQuestion.ID {
			sawQuestion = true
		}
		if event.Kind == EventUserQuestionCancelled && event.UserQuestionID == firstQuestion.ID {
			sawCancellation = true
		}
	}
	if !sawQuestion || !sawCancellation {
		t.Fatalf("question lifecycle events missing: requested=%t cancelled=%t", sawQuestion, sawCancellation)
	}
	if pending, err := runtime.PendingRequests(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("pending questions after interrupt = %#v, error=%v", pending, err)
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
	if pending, err := runtime.PendingRequests(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("question survived Core process restart: %#v, error=%v", pending, err)
	}
	if _, err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	resumedWait := waitTurnAsync(runtime, submission.ExecutionID)
	secondQuestion := waitPendingUserQuestion(t, runtime, submission.ExecutionID)
	if secondQuestion.ID == firstQuestion.ID || secondQuestion.Question != firstQuestion.Question || strings.Join(secondQuestion.Options, "\x00") != strings.Join(firstQuestion.Options, "\x00") {
		t.Fatalf("recovered user question = %#v, original = %#v", secondQuestion, firstQuestion)
	}
	if err := runtime.ResolveUserQuestion(secondQuestion.ID, "staging"); err != nil {
		t.Fatal(err)
	}
	completed := waitTurnResult(t, resumedWait)
	if completed.err != nil || completed.result.Status != TaskCompleted || completed.result.ExecutionID != submission.ExecutionID {
		t.Fatalf("WaitTurn after Recover = %#v, error=%v", completed.result, completed.err)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 2 {
		t.Fatalf("model requests after recovery = %d, want original request and final synthesis", requests)
	}
}

func TestCoreRejectsUserQuestionFromSubagent(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: saved.ID, AgentID: "worker"})
	if _, err := client.requestUserQuestion(ctx, "Need clarification?", nil); err == nil || !strings.Contains(err.Error(), "lead agent") {
		t.Fatalf("subagent question error = %v, want lead-only error", err)
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
	arguments, _ := json.Marshal(struct {
		Question string   `json:"question"`
		Options  []string `json:"options,omitempty"`
	}{Question: question, Options: options})
	return hostToolCallResponse("ask-call", "ask_user", string(arguments))
}
