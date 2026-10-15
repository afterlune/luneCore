package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
)

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
	var live Event
	deadline := time.After(time.Second)
	for durable.Kind == "" || live.Kind == "" {
		select {
		case event := <-events:
			if event.Kind == EventToolStarted {
				durable = event
			}
			if event.Kind == EventModelStream {
				live = event
			}
		case <-deadline:
			t.Fatal("runtime events were not delivered")
		}
	}
	if durable.Cursor == 0 || string(durable.Tool.Arguments) != "null" || durable.Err != "" || durable.Approval == nil || string(durable.Approval.Arguments) != "null" || durable.Interaction == nil || string(durable.Interaction.Request) != "null" || durable.ToolRecovery == nil || string(durable.ToolRecovery.Arguments) != "null" || durable.Compaction == nil || durable.Compaction.Err != "" {
		t.Fatalf("durable projection = %#v", durable)
	}
	if live.Cursor != 0 || live.Stream == nil || live.Stream.Delta != "live-only secret" {
		t.Fatalf("live event = %#v", live)
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
