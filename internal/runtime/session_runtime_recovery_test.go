package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestSessionRuntimeWaitTurnResultSurvivesReopen(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostTextResponse("durable response"), hostTextResponse("durable response")}}
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
	runResponse, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "blocking response format")})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "durable result")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.WaitTurn(context.Background(), submission.ExecutionID)
	if err != nil || result.Status != TaskCompleted || len(result.Response) == 0 || !json.Valid(result.Response) {
		t.Fatalf("WaitTurn result = %#v, error=%v", result, err)
	}
	if string(result.Response) != string(runResponse.Response) {
		t.Fatalf("WaitTurn response differs from RunTask: got %s, want %s", result.Response, runResponse.Response)
	}
	firstResponse := append(json.RawMessage(nil), result.Response...)
	result, err = runtime.WaitTurn(context.Background(), submission.ExecutionID)
	if err != nil || result.Status != TaskCompleted || string(result.Response) != string(firstResponse) {
		t.Fatalf("repeated WaitTurn result = %#v, error=%v", result, err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
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
	result, err = runtime.WaitTurn(context.Background(), submission.ExecutionID)
	if err != nil || result.Status != TaskCompleted || string(result.Response) != string(firstResponse) {
		t.Fatalf("reopened WaitTurn result = %#v, error=%v", result, err)
	}
	fork, err := reopened.ForkSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := reopened.sessions.TurnResult(context.Background(), fork.ID, submission.ExecutionID); err != nil || found {
		t.Fatalf("fork copied turn result found=%t, error=%v", found, err)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 2 {
		t.Fatalf("provider requests = %d, want 2", requests)
	}
}

func TestSessionRuntimeWaitTurnCancellationDoesNotInterruptExecution(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 1), release: make(chan struct{}, 1)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "wait cancellation")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() {
		_, err := runtime.WaitTurn(waitCtx, submission.ExecutionID)
		waitDone <- err
	}()
	_ = receiveProviderInput(t, provider.entered)
	cancel()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitTurn error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitTurn did not stop after its context was canceled")
	}
	if state, err := runtime.State(context.Background()); err != nil || state.Task.Status != TaskRunning {
		t.Fatalf("task after canceling waiter = %#v, error=%v", state, err)
	}
	provider.release <- struct{}{}
	result, err := runtime.WaitTurn(context.Background(), submission.ExecutionID)
	if err != nil || result.Status != TaskCompleted {
		t.Fatalf("WaitTurn after canceled wait = %#v, error=%v", result, err)
	}
}

func TestSessionRuntimeWaitTurnReturnsPausedThenResumedResult(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 2), release: make(chan struct{}, 1)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "pause and resume")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	firstWait := make(chan struct {
		result TurnResult
		err    error
	}, 1)
	go func() {
		result, err := runtime.WaitTurn(context.Background(), submission.ExecutionID)
		firstWait <- struct {
			result TurnResult
			err    error
		}{result, err}
	}()
	_ = receiveProviderInput(t, provider.entered)
	if !runtime.Interrupt(submission.ExecutionID) {
		t.Fatal("Interrupt returned false")
	}
	var paused struct {
		result TurnResult
		err    error
	}
	select {
	case paused = <-firstWait:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitTurn did not return the paused result")
	}
	if paused.err != nil || paused.result.Status != TaskPaused || paused.result.ExecutionID != submission.ExecutionID {
		t.Fatalf("paused WaitTurn result = %#v, error=%v", paused.result, paused.err)
	}
	if _, err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondWait := make(chan struct {
		result TurnResult
		err    error
	}, 1)
	go func() {
		result, err := runtime.WaitTurn(context.Background(), submission.ExecutionID)
		secondWait <- struct {
			result TurnResult
			err    error
		}{result, err}
	}()
	_ = receiveProviderInput(t, provider.entered)
	provider.release <- struct{}{}
	select {
	case resumed := <-secondWait:
		if resumed.err != nil || resumed.result.Status != TaskCompleted || resumed.result.ExecutionID != submission.ExecutionID || len(resumed.result.Response) == 0 {
			t.Fatalf("resumed WaitTurn result = %#v, error=%v", resumed.result, resumed.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitTurn did not return the resumed result")
	}
}

func TestSessionRuntimeWaitTurnReportsCancelledAndDiscardedStatuses(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 1), release: make(chan struct{}, 1)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "pause before discarding")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	firstWait := make(chan TurnResult, 1)
	firstWaitErr := make(chan error, 1)
	go func() {
		result, err := runtime.WaitTurn(context.Background(), first.ExecutionID)
		firstWait <- result
		firstWaitErr <- err
	}()
	_ = receiveProviderInput(t, provider.entered)
	second, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "cancel before start")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.Interrupt(second.ExecutionID) {
		t.Fatal("Interrupt returned false for queued turn")
	}
	cancelled, err := runtime.WaitTurn(context.Background(), second.ExecutionID)
	if err != nil || cancelled.Status != TaskCancelled || cancelled.Response != nil {
		t.Fatalf("cancelled WaitTurn result = %#v, error=%v", cancelled, err)
	}
	if !runtime.Interrupt(first.ExecutionID) {
		t.Fatal("Interrupt returned false for active turn")
	}
	select {
	case paused := <-firstWait:
		if err := <-firstWaitErr; err != nil || paused.Status != TaskPaused {
			t.Fatalf("paused WaitTurn result = %#v, error=%v", paused, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitTurn did not return paused status")
	}
	if discarded, err := runtime.DiscardPaused(context.Background()); err != nil || !discarded {
		t.Fatalf("DiscardPaused = %t, error=%v", discarded, err)
	}
	discarded, err := runtime.WaitTurn(context.Background(), first.ExecutionID)
	if err != nil || discarded.Status != TaskDiscarded || discarded.Response != nil {
		t.Fatalf("discarded WaitTurn result = %#v, error=%v", discarded, err)
	}
}

func TestSessionRuntimeWaitTurnPersistsFailureStatusWithoutErrorDetails(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "fail once")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.WaitTurn(context.Background(), submission.ExecutionID)
	if err != nil || result.Status != TaskFailed || result.Response != nil {
		t.Fatalf("failed WaitTurn result = %#v, error=%v", result, err)
	}
	stored, found, err := client.sessions.TurnResult(context.Background(), saved.ID, submission.ExecutionID)
	if err != nil || !found || stored.Status != string(TaskFailed) || stored.Response != nil {
		t.Fatalf("stored failed result = %#v found=%t error=%v", stored, found, err)
	}
	events, err := client.sessions.RuntimeEventsAfter(context.Background(), saved.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), "scripted provider ran out of responses") {
			t.Fatalf("free-form failure detail was persisted: %s", event.Payload)
		}
	}
}

func TestSessionRuntimeFinalizesCompletedCheckpointAfterRestartWithoutRerunning(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{{
		Status: "completed",
		Output: []publicmodel.ResponseOutputItem{
			publicmodel.OutputCompaction{ID: "cmp-checkpoint", EncryptedContent: "checkpoint-compaction-state"},
			publicmodel.OutputReasoning{EncryptedContent: "checkpoint-reasoning-state", Signature: "checkpoint-signature"},
			publicmodel.OutputMessage{Role: publicmodel.RoleAssistant, Content: []publicmodel.OutputContentPart{publicmodel.OutputText{Text: "finish before commit"}}},
		},
	}}}
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
	sessionDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(stateDir, "sessions.db"))+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer sessionDB.Close()
	if _, err := sessionDB.Exec(`CREATE TRIGGER reject_turn_result BEFORE INSERT ON turn_results
		BEGIN SELECT RAISE(ABORT, 'reject turn result'); END`); err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "complete checkpoint")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, waitErr := runtime.WaitTurn(waitCtx, submission.ExecutionID)
	cancelWait()
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("WaitTurn before checkpoint recovery error = %v, want context deadline exceeded", waitErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.mu.RLock()
		running := client.hasRunningTasksLocked()
		client.mu.RUnlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("checkpoint execution did not leave its running state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := sessionDB.Exec(`DROP TRIGGER reject_turn_result`); err != nil {
		t.Fatal(err)
	}
	_ = sessionDB.Close()
	if err := client.Close(); err != nil {
		t.Fatal(err)
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
	result, err := runtime.WaitTurn(context.Background(), submission.ExecutionID)
	if err != nil || result.Status != TaskCompleted || len(result.Response) == 0 {
		t.Fatalf("recovered WaitTurn result = %#v, error=%v", result, err)
	}
	recoveredSession, err := reopened.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recoveredSession.ModelContext), "checkpoint-reasoning-state") ||
		!strings.Contains(string(recoveredSession.ModelContext), "checkpoint-compaction-state") ||
		strings.Contains(string(recoveredSession.History), "checkpoint-reasoning-state") {
		t.Fatalf("recovered provider context or transcript is incomplete: context=%s history=%s", recoveredSession.ModelContext, recoveredSession.History)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 1 {
		t.Fatalf("provider requests after checkpoint recovery = %d, want 1", requests)
	}
}
