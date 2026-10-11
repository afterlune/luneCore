package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
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

func TestSessionRuntimeEventLogStoresSafeProjectionAndReplays(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostTextResponse("unused")}})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, err := runtime.SubscribeEvents(ctx, 0)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		for range events {
		}
	})
	client.emit(Event{Kind: EventToolStarted, SessionID: saved.ID, Err: "private error",
		Tool:         ToolCall{Name: "write_file", Arguments: json.RawMessage(`{"content":"private secret"}`)},
		Approval:     &ApprovalRequest{ID: "approval", Tool: "write_file", Arguments: json.RawMessage(`{"path":"private approval"}`)},
		Interaction:  &UserInteractionRequest{ID: "interaction", ServerName: "docs", Request: json.RawMessage(`{"prompt":"private interaction"}`)},
		ToolRecovery: &ToolRecoveryRequest{InvocationID: "invocation", Arguments: json.RawMessage(`{"token":"private recovery"}`)},
		Compaction:   &ContextCompactionDetails{Stage: "failed", Err: "private compaction error"},
	})
	client.emit(Event{Kind: EventModelStream, SessionID: saved.ID, Stream: &ModelStreamEvent{Type: "text_delta", Delta: "live-only secret"}})
	var durable Event
	select {
	case durable = <-events:
	case <-time.After(time.Second):
		t.Fatal("durable event was not delivered")
	}
	if durable.Kind != EventToolStarted || durable.Cursor == 0 || string(durable.Tool.Arguments) != "null" || durable.Err != "" ||
		durable.Approval == nil || string(durable.Approval.Arguments) != "null" || durable.Interaction == nil || string(durable.Interaction.Request) != "null" ||
		durable.ToolRecovery == nil || string(durable.ToolRecovery.Arguments) != "null" || durable.Compaction == nil || durable.Compaction.Err != "" {
		t.Fatalf("durable projection = %#v", durable)
	}
	select {
	case live := <-events:
		if live.Kind != EventModelStream || live.Cursor != 0 || live.Stream == nil || live.Stream.Delta != "live-only secret" {
			t.Fatalf("live event = %#v", live)
		}
	case <-time.After(time.Second):
		t.Fatal("live model stream event was not delivered")
	}
	stored, err := client.sessions.RuntimeEventsAfter(context.Background(), saved.ID, 0, 10)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored events = %#v, error=%v", stored, err)
	}
	for _, private := range []string{"private secret", "private error", "private approval", "private interaction", "private recovery", "private compaction error", "live-only secret"} {
		if strings.Contains(string(stored[0].Payload), private) {
			t.Fatalf("unsafe event payload persisted: %s", stored[0].Payload)
		}
	}
	// A new subscriber replays the durable event by cursor, while model deltas stay transient.
	replayCtx, cancelReplay := context.WithCancel(context.Background())
	replay, err := runtime.SubscribeEvents(replayCtx, 0)
	if err != nil {
		cancelReplay()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancelReplay()
		for range replay {
		}
	})
	select {
	case event := <-replay:
		if event.Cursor != durable.Cursor || event.Kind != EventToolStarted {
			t.Fatalf("replayed event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("durable event replay was not delivered")
	}
}

func TestRuntimeEventsSurviveRewindButForksStartWithoutRuntimeState(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostTextResponse("unused")}})
	history := json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`)
	saved, err := client.CreateSession(context.Background(), history)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	client.emit(Event{Kind: EventTaskStarted, SessionID: saved.ID, ExecutionID: "event-only", Status: TaskRunning})
	if removed, err := client.RewindSession(context.Background(), saved.ID, 1); err != nil || removed != 1 {
		t.Fatalf("RewindSession = %d, error=%v", removed, err)
	}
	oldEvents, err := client.sessions.RuntimeEventsAfter(context.Background(), saved.ID, 0, 10)
	if err != nil || len(oldEvents) != 1 {
		t.Fatalf("events after rewind = %#v, error=%v", oldEvents, err)
	}
	if _, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "queued original work")}, QueueInput); err != nil {
		t.Fatal(err)
	}
	fork, err := client.ForkSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := client.sessions.QueuedTurns(context.Background(), fork.ID)
	if err != nil || len(queued) != 0 {
		t.Fatalf("fork queue = %#v, error=%v", queued, err)
	}
	cursor, err := client.sessions.LatestRuntimeEventCursor(context.Background(), fork.ID)
	if err != nil || cursor != 0 {
		t.Fatalf("fork event cursor = %d, error=%v", cursor, err)
	}
}

func runtimeTestConfig(workspace, stateDir string, provider publicmodel.Provider) Config {
	return Config{
		Workspace: workspace, StateDir: stateDir,
		ModelConfig: &publicmodel.Config{
			Providers:    map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Models:       map[string]publicmodel.ModelProfile{"host": {Provider: "host", Model: "test-model"}},
			DefaultModel: "host",
		},
		ModelProviders: map[string]publicmodel.Provider{"host": provider},
	}
}

func receiveProviderInput(t *testing.T, inputs <-chan string) string {
	t.Helper()
	select {
	case input := <-inputs:
		return input
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not receive the queued turn")
		return ""
	}
}

func waitRuntimeStatus(t *testing.T, runtime *SessionRuntime, want TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := runtime.State(context.Background())
		if err == nil && state.Task.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, err := runtime.State(context.Background())
	t.Fatalf("runtime status = %#v, error=%v; want %q", state, err, want)
}

func waitQueueLength(t *testing.T, runtime *SessionRuntime, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := runtime.State(context.Background())
		if err == nil && len(state.Queue) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, err := runtime.State(context.Background())
	t.Fatalf("runtime queue length = %d, error=%v; want %d (%#v)", len(state.Queue), err, want, state.Queue)
}
