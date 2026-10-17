package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func TestSessionRuntimeQueueInputIsFIFOAndSubmissionContextOnlyControlsAdmission(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 3), release: make(chan struct{}, 3)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = subscribeTestEvents(t, client, saved.ID)

	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "first")}, StartIfIdle)
	if err != nil || first.Status != SubmissionAccepted {
		t.Fatalf("first submission = %#v, error=%v", first, err)
	}
	_ = receiveProviderInput(t, provider.entered)

	queueCtx, cancelQueue := context.WithCancel(context.Background())
	second, err := runtime.SubmitTurn(queueCtx, TurnRequest{Input: mustSettingsTaskInput(t, "second")}, QueueInput)
	if err != nil || second.Status != SubmissionQueued {
		t.Fatalf("second submission = %#v, error=%v", second, err)
	}
	cancelQueue()
	third, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "third")}, QueueInput)
	if err != nil || third.Status != SubmissionQueued {
		t.Fatalf("third submission = %#v, error=%v", third, err)
	}
	if second.Position >= third.Position {
		t.Fatalf("queue positions second=%d third=%d", second.Position, third.Position)
	}
	if _, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "blocked")}, StartIfIdle); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("StartIfIdle while busy = %v, want ErrSessionBusy", err)
	}

	provider.release <- struct{}{}
	_ = receiveProviderInput(t, provider.entered)
	provider.release <- struct{}{}
	_ = receiveProviderInput(t, provider.entered)
	provider.release <- struct{}{}
	waitQueueLength(t, runtime, 0)

	stored, err := client.sessions.RuntimeEventsAfter(context.Background(), saved.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, record := range stored {
		var event Event
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == EventTaskStarted {
			started = append(started, event.ExecutionID)
		}
	}
	want := []string{first.ExecutionID, second.ExecutionID, third.ExecutionID}
	if len(started) != len(want) {
		t.Fatalf("started execution IDs = %v, want %v", started, want)
	}
	for i := range want {
		if started[i] != want[i] {
			t.Fatalf("started execution order = %v, want %v", started, want)
		}
	}
}

func TestSessionRuntimeReplacesQueuedTurnAndCancelsLaterTurns(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 3), release: make(chan struct{}, 3)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "first")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	second, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "second original")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	third, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "third discarded")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}

	replacement, cancelled, err := runtime.ReplaceQueuedTurnInput(context.Background(), second.ExecutionID, mustSettingsTaskInput(t, "second revised"))
	if err != nil {
		t.Fatalf("ReplaceQueuedTurnInput: %v", err)
	}
	if replacement.ExecutionID != second.ExecutionID || replacement.QueueID != second.QueueID || replacement.Position != second.Position || replacement.Status != SubmissionQueued {
		t.Fatalf("replacement = %#v, want original queue identity and position %#v", replacement, second)
	}
	if len(cancelled) != 1 || cancelled[0] != third.ExecutionID {
		t.Fatalf("cancelled execution IDs = %v, want only later turn %q", cancelled, third.ExecutionID)
	}
	stored, found, err := client.sessions.QueuedTurnByExecutionID(context.Background(), saved.ID, second.ExecutionID)
	if err != nil || !found {
		t.Fatalf("load replaced queued turn: found=%v error=%v", found, err)
	}
	var request storedTurnRequest
	if err := json.Unmarshal(stored.Request, &request); err != nil {
		t.Fatal(err)
	}
	if input, err := model.NewResponseInput(request.Input); err != nil || input.Text == nil || *input.Text != "second revised" {
		t.Fatalf("stored replacement input = %#v, error=%v", input, err)
	}
	if result, err := runtime.WaitTurn(context.Background(), third.ExecutionID); err != nil || result.Status != TaskCancelled {
		t.Fatalf("later turn result = %#v, error=%v; want cancelled", result, err)
	}

	provider.release <- struct{}{}
	if got := receiveProviderInput(t, provider.entered); got != "second revised" {
		t.Fatalf("next provider input = %q, want edited queued message", got)
	}
	provider.release <- struct{}{}
	waitQueueLength(t, runtime, 0)
	if result, err := runtime.WaitTurn(context.Background(), first.ExecutionID); err != nil || result.Status != TaskCompleted {
		t.Fatalf("first turn result = %#v, error=%v", result, err)
	}
	if result, err := runtime.WaitTurn(context.Background(), second.ExecutionID); err != nil || result.Status != TaskCompleted {
		t.Fatalf("replacement result = %#v, error=%v", result, err)
	}
}

func TestSessionRuntimeReplaceQueuedTurnDetectsClaimRace(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 3), release: make(chan struct{}, 3)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "first")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	target, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "target")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	later, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "later")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	provider.release <- struct{}{}
	if got := receiveProviderInput(t, provider.entered); got != "target" {
		t.Fatalf("next provider input = %q, want target", got)
	}
	if _, _, err := runtime.ReplaceQueuedTurnInput(context.Background(), target.ExecutionID, mustSettingsTaskInput(t, "edited")); !errors.Is(err, ErrQueuedTurnStarted) {
		t.Fatalf("ReplaceQueuedTurnInput on active target = %v, want ErrQueuedTurnStarted", err)
	}
	state, err := runtime.State(context.Background())
	if err != nil || state.Task.ExecutionID != target.ExecutionID || len(state.Queue) != 2 || state.Queue[0].ExecutionID != target.ExecutionID || state.Queue[1].ExecutionID != later.ExecutionID {
		t.Fatalf("queue changed after rejected replacement: state=%#v error=%v", state, err)
	}
	if _, err := runtime.CancelPendingTurns(context.Background()); err != nil {
		t.Fatalf("CancelPendingTurns cleanup: %v", err)
	}
	if state, err := runtime.State(context.Background()); err != nil || len(state.Queue) != 0 {
		t.Fatalf("state after cleanup = %#v, error=%v", state, err)
	}
	if result, err := runtime.WaitTurn(context.Background(), first.ExecutionID); err != nil || result.Status != TaskCompleted {
		t.Fatalf("first turn result = %#v, error=%v", result, err)
	}
}

func TestSessionRuntimeCancelPendingTurnsInterruptsActiveAndDropsQueue(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 3), release: make(chan struct{}, 3)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "active")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	second, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "queued one")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	third, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "queued two")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	affected, err := runtime.CancelPendingTurns(context.Background())
	if err != nil {
		t.Fatalf("CancelPendingTurns: %v", err)
	}
	if len(affected) != 3 {
		t.Fatalf("affected executions = %v, want active and both queued turns", affected)
	}
	if state, err := runtime.State(context.Background()); err != nil || len(state.Queue) != 0 || state.Task.Status == TaskRunning || state.Task.Status == TaskQueued {
		t.Fatalf("state after cancellation = %#v, error=%v", state, err)
	}
	for _, id := range []string{first.ExecutionID, second.ExecutionID, third.ExecutionID} {
		if _, err := runtime.WaitTurn(context.Background(), id); err != nil {
			t.Fatalf("WaitTurn(%q): %v", id, err)
		}
	}
	select {
	case next := <-provider.entered:
		t.Fatalf("a queued task started after cancellation: %q", next)
	default:
	}
}

func TestSessionRuntimeCancelsOneQueuedTurnWithoutInterruptingActiveOrReordering(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 4), release: make(chan struct{}, 4)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	active, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "active")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "keep first")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "remove this")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	last, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "keep last")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.CancelQueuedTurn(context.Background(), active.ExecutionID); !errors.Is(err, ErrQueuedTurnStarted) {
		t.Fatalf("CancelQueuedTurn(active) = %v, want ErrQueuedTurnStarted", err)
	}
	if err := runtime.CancelQueuedTurn(context.Background(), removed.ExecutionID); err != nil {
		t.Fatalf("CancelQueuedTurn(queued) = %v", err)
	}
	state, err := runtime.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Task.Status != TaskRunning || state.Task.ExecutionID != active.ExecutionID {
		t.Fatalf("active task after queue cancellation = %#v, want %q to keep running", state.Task, active.ExecutionID)
	}
	if len(state.Queue) != 3 || state.Queue[1].ExecutionID != first.ExecutionID || state.Queue[2].ExecutionID != last.ExecutionID {
		t.Fatalf("queue after removing middle item = %#v, want active, first, last", state.Queue)
	}
	result, err := runtime.WaitTurn(context.Background(), removed.ExecutionID)
	if err != nil || result.Status != TaskCancelled {
		t.Fatalf("removed turn result = %#v, error=%v; want cancelled", result, err)
	}
	select {
	case input := <-provider.entered:
		t.Fatalf("queued work started before active release: %q", input)
	default:
	}

	provider.release <- struct{}{}
	if got := receiveProviderInput(t, provider.entered); got != "keep first" {
		t.Fatalf("next input = %q, want first remaining queue item", got)
	}
	provider.release <- struct{}{}
	if got := receiveProviderInput(t, provider.entered); got != "keep last" {
		t.Fatalf("final input = %q, want last remaining queue item", got)
	}
	provider.release <- struct{}{}
	waitQueueLength(t, runtime, 0)
	for _, executionID := range []string{active.ExecutionID, first.ExecutionID, last.ExecutionID} {
		if _, err := runtime.WaitTurn(context.Background(), executionID); err != nil {
			t.Fatalf("WaitTurn(%q): %v", executionID, err)
		}
	}
}

func TestSessionRuntimeDrainsFIFOWithoutEventSubscribers(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 2), release: make(chan struct{}, 2)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "first without listeners")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	second, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "second without listeners")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	provider.release <- struct{}{}
	_ = receiveProviderInput(t, provider.entered)
	provider.release <- struct{}{}
	waitStoredTurnStatus(t, client, saved.ID, first.ExecutionID, TaskCompleted)
	waitStoredTurnStatus(t, client, saved.ID, second.ExecutionID, TaskCompleted)

	// A late observer replays lifecycle events; attaching it does not rerun either turn.
	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream, err := runtime.SubscribeEvents(streamCtx, 0)
	if err != nil {
		cancelStream()
		t.Fatal(err)
	}
	started := make(map[string]int)
	finished := make(map[string]int)
	deadline := time.After(5 * time.Second)
	for len(finished) < 2 {
		select {
		case event := <-stream:
			if event.Kind == EventTaskStarted {
				started[event.ExecutionID]++
			}
			if event.Kind == EventTaskFinished {
				finished[event.ExecutionID]++
			}
		case <-deadline:
			cancelStream()
			for range stream {
			}
			t.Fatalf("replayed events: started=%v finished=%v", started, finished)
		}
	}
	cancelStream()
	for range stream {
	}
	if started[first.ExecutionID] != 1 || started[second.ExecutionID] != 1 || finished[first.ExecutionID] != 1 || finished[second.ExecutionID] != 1 {
		t.Fatalf("replayed lifecycle counts: started=%v finished=%v", started, finished)
	}
}

func TestOpenSessionRuntimeStartsPersistedUnpausedQueue(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 1), release: make(chan struct{}, 1)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(storedTurnRequest{Input: mustSettingsTaskInput(t, "start on open").Raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.sessions.EnqueueTurn(context.Background(), session.QueuedTurn{
		SessionID: saved.ID, QueueID: "persisted-queue", ExecutionID: "persisted-execution", Request: request,
	}, mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: saved.ID, QueueID: "persisted-queue", ExecutionID: "persisted-execution", Status: TaskQueued})); err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	provider.release <- struct{}{}
	waitStoredTurnStatus(t, client, saved.ID, "persisted-execution", TaskCompleted)
	if state, err := runtime.State(context.Background()); err != nil || len(state.Queue) != 0 {
		t.Fatalf("runtime state after persisted turn = %#v, error=%v", state, err)
	}
}

func TestSessionRuntimeReopensPausedTurnAndDrainsQueuedInput(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 3), release: make(chan struct{}, 3)}
	workspace, stateDir := t.TempDir(), t.TempDir()
	config := runtimeTestConfig(workspace, stateDir, provider)
	client, err := New(config)
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
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream, err := runtime.SubscribeEvents(streamCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		for range stream {
		}
	}()

	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "paused turn")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	if !runtime.Interrupt(first.ExecutionID) {
		t.Fatal("Interrupt returned false for active turn")
	}
	waitRuntimeStatus(t, runtime, TaskPaused)
	cancelStream()
	<-streamDone
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
	state, err := runtime.State(context.Background())
	if err != nil || state.Task.Status != TaskPaused || len(state.Queue) != 1 || state.Queue[0].Status != "paused" {
		t.Fatalf("reopened runtime state = %#v, error=%v", state, err)
	}
	if _, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "blocked while paused")}, StartIfIdle); !errors.Is(err, ErrTaskPaused) {
		t.Fatalf("StartIfIdle while paused = %v, want ErrTaskPaused", err)
	}
	queued, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "after recovery")}, QueueInput)
	if err != nil || queued.Position <= state.Queue[0].Position {
		t.Fatalf("queued turn after reopen = %#v, error=%v", queued, err)
	}
	resumeCtx, cancelResume := context.WithCancel(context.Background())
	resumeEvents, err := runtime.SubscribeEvents(resumeCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancelResume()
		for range resumeEvents {
		}
	}()
	resumeDone := make(chan struct{})
	go func() {
		defer close(resumeDone)
		for range resumeEvents {
		}
	}()
	if _, err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	provider.release <- struct{}{}
	_ = receiveProviderInput(t, provider.entered)
	provider.release <- struct{}{}
	waitQueueLength(t, runtime, 0)
	state, err = runtime.State(context.Background())
	if err != nil || state.Task.Status != TaskCompleted {
		t.Fatalf("state after recovery and queued turn = %#v, error=%v", state, err)
	}
	cancelResume()
	<-resumeDone
}

func TestSessionRuntimeDiscardPausedTurnUnblocksQueue(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 2), release: make(chan struct{}, 2)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = subscribeTestEvents(t, client, saved.ID)
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "discard this")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	if _, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "run after discard")}, QueueInput); err != nil {
		t.Fatal(err)
	}
	if !runtime.Interrupt(first.ExecutionID) {
		t.Fatal("Interrupt returned false")
	}
	waitRuntimeStatus(t, runtime, TaskPaused)
	if discarded, err := runtime.DiscardPaused(context.Background()); err != nil || !discarded {
		t.Fatalf("DiscardPaused = %v, error=%v", discarded, err)
	}
	_ = receiveProviderInput(t, provider.entered)
	provider.release <- struct{}{}
	waitQueueLength(t, runtime, 0)
}
