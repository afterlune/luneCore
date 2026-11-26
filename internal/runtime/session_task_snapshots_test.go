package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
)

func TestSessionTaskSnapshotsDoNotStartQueuedWork(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostTextResponse("should remain queued")}}
	client := newProgrammaticCore(t, provider)
	ctx := context.Background()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(storedTurnRequest{Input: json.RawMessage(`"queued request"`)})
	if err != nil {
		t.Fatal(err)
	}
	queued := session.QueuedTurn{
		SessionID: saved.ID, QueueID: "snapshot-queue", ExecutionID: "snapshot-execution",
		Request: request, Status: session.QueuedTurnQueued,
	}
	event := mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: saved.ID, QueueID: queued.QueueID, ExecutionID: queued.ExecutionID, Status: TaskQueued})
	if _, _, err := client.sessions.EnqueueTurn(ctx, queued, event); err != nil {
		t.Fatal(err)
	}

	snapshots, err := client.ListSessionTaskSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].SessionID != saved.ID || snapshots[0].Status != TaskQueued || snapshots[0].ExecutionID != queued.ExecutionID || snapshots[0].QueueCount != 1 {
		t.Fatalf("task snapshots = %+v", snapshots)
	}
	runtime, err := client.OpenSessionRuntimeReadOnly(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := runtime.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Task.Status != TaskQueued || len(state.Queue) != 1 {
		t.Fatalf("read-only runtime state = %+v", state)
	}
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 0 {
		t.Fatalf("read-only snapshots started %d model requests", requests)
	}
}
