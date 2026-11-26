package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAnalysisMetadataRollsBackWhenQueueInsertFails(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	if err := store.Create(ctx, Snapshot{
		ID: "analysis", Workspace: "workspace", UpdatedAt: time.Now(), Data: json.RawMessage(`{"history":null}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_analysis_queue BEFORE INSERT ON queued_turn_inputs BEGIN SELECT RAISE(ABORT, 'queue rejected'); END`); err != nil {
		t.Fatal(err)
	}
	queued := QueuedTurn{
		SessionID: "analysis", QueueID: "queue", ExecutionID: "execution",
		Request: json.RawMessage(`{"input":"review"}`), Status: QueuedTurnQueued,
	}
	_, _, _, _, err := store.EnqueueTurnIdempotentWithAnalysis(
		ctx, queued, json.RawMessage(`{"kind":"task_queued"}`), "submission", []byte("digest"),
		func(QueuedTurn) ([]byte, error) { return []byte(`{"executionId":"execution"}`), nil },
		json.RawMessage(`{"kind":"review","sourceSessionId":"source","executionId":"execution","evidenceDigest":"0123456789012345678901234567890123456789012345678901234567890123"}`),
	)
	if err == nil || !strings.Contains(err.Error(), "enqueue turn") {
		t.Fatalf("failed queue insert error = %v", err)
	}
	snapshot, err := store.Get(ctx, "analysis", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(snapshot.Data, &data); err != nil {
		t.Fatal(err)
	}
	if _, exists := data["analysis"]; exists {
		t.Fatalf("analysis metadata survived rolled-back queue transaction: %s", snapshot.Data)
	}
	var queuedCount, eventCount, submissionCount int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM queued_turn_inputs WHERE session_id = 'analysis'`:    &queuedCount,
		`SELECT count(*) FROM runtime_events WHERE session_id = 'analysis'`:        &eventCount,
		`SELECT count(*) FROM turn_submission_dedup WHERE session_id = 'analysis'`: &submissionCount,
	} {
		if err := store.db.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if queuedCount != 0 || eventCount != 0 || submissionCount != 0 {
		t.Fatalf("partial queue transaction: queued=%d events=%d submissions=%d", queuedCount, eventCount, submissionCount)
	}
}
