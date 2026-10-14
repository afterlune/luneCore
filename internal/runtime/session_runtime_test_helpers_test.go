package runtime

import (
	"context"
	"testing"
	"time"
)

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

func waitStoredTurnStatus(t *testing.T, client *Core, sessionID, executionID string, want TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result, found, err := client.sessions.TurnResult(context.Background(), sessionID, executionID)
		if err == nil && found && TaskStatus(result.Status) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	result, found, err := client.sessions.TurnResult(context.Background(), sessionID, executionID)
	t.Fatalf("stored turn result = %#v found=%t error=%v; want status %q", result, found, err, want)
}
