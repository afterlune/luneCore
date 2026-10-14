package runtime

import (
	"context"
	"testing"
)

func subscribeTestEvents(t *testing.T, client *Core, sessionID string) <-chan Event {
	t.Helper()
	runtime, err := client.OpenSessionRuntime(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	cursor, err := client.sessions.LatestRuntimeEventCursor(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LatestRuntimeEventCursor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, err := runtime.SubscribeEvents(ctx, cursor)
	if err != nil {
		cancel()
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		for range events {
		}
	})
	return events
}

func interruptTestTask(t *testing.T, client *Core, sessionID string) bool {
	t.Helper()
	state, ok := client.StateForSession(sessionID)
	if !ok || state.ExecutionID == "" {
		return false
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	return runtime.Interrupt(state.ExecutionID)
}
