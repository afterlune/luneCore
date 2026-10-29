package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
)

func TestFailedOutcomeCommitStopsDrainUntilSessionReopens(t *testing.T) {
	p := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		return hostTextResponse("committed after storage recovers"), nil
	}}
	client, rt := newGoalTestCore(t, p)
	client.queueDrainWG.Wait()
	db, err := sql.Open("sqlite", filepath.Join(client.stateDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_completion BEFORE INSERT ON turn_results BEGIN SELECT RAISE(ABORT,'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	input, _ := NewTaskInput("execute only once")
	sub, err := rt.SubmitTurn(t.Context(), TurnRequest{Input: input}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitContext(ctx, &client.queueDrainWG); err != nil {
		t.Fatalf("drain kept rescheduling during persistent failure: %v", err)
	}
	queue, err := client.sessions.QueuedTurns(t.Context(), rt.sessionID)
	if err != nil || len(queue) != 1 || queue[0].Status != session.QueuedTurnActive {
		t.Fatalf("recovery evidence lost: %+v,%v", queue, err)
	}
	if _, found, err := client.sessions.TurnResult(t.Context(), rt.sessionID, sub.ExecutionID); err != nil || found {
		t.Fatalf("uncommitted result exposed: found=%v err=%v", found, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_completion`); err != nil {
		t.Fatal(err)
	}
	reopened, err := client.OpenSessionRuntime(t.Context(), rt.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	result, err := reopened.WaitTurn(ctx, sub.ExecutionID)
	if err != nil || result.Status != TaskCompleted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	client.queueDrainWG.Wait()
	if p.calls.Load() != 1 {
		t.Fatalf("model repeated: %d", p.calls.Load())
	}
}

func TestRejectedQueuePersistenceFailureStopsReconciliation(t *testing.T) {
	p := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		return hostTextResponse("must not run"), nil
	}}
	client, rt := newGoalTestCore(t, p)
	client.queueDrainWG.Wait()
	q := session.QueuedTurn{SessionID: rt.sessionID, QueueID: "broken", ExecutionID: "broken", Request: json.RawMessage(`{"input":null}`)}
	if _, _, err := client.sessions.EnqueueTurn(t.Context(), q, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.sessions.ClaimNextQueuedTurn(t.Context(), rt.sessionID); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(client.stateDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_completion BEFORE INSERT ON turn_results BEGIN SELECT RAISE(ABORT,'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	client.scheduleQueueDrain(rt.sessionID)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitContext(ctx, &client.queueDrainWG); err != nil {
		t.Fatalf("rejected task loop: %v", err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_completion`); err != nil {
		t.Fatal(err)
	}
	client.scheduleQueueDrain(rt.sessionID)
	if err := waitContext(ctx, &client.queueDrainWG); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("unsafe missing-checkpoint replay")
	}
}

func TestSynchronousCommitFailureReturnsStorageError(t *testing.T) {
	p := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		return hostTextResponse("execute once"), nil
	}}
	client, rt := newGoalTestCore(t, p)
	client.queueDrainWG.Wait()
	db, err := sql.Open("sqlite", filepath.Join(client.stateDir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_completion BEFORE INSERT ON turn_results BEGIN SELECT RAISE(ABORT,'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	input, _ := NewTaskInput("synchronous execution")
	done := make(chan error, 1)
	go func() {
		_, err := client.RunTask(t.Context(), TaskRequest{SessionID: rt.sessionID, Input: input})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "storage failure") {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("synchronous waiter hung after commit failure")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitContext(ctx, &client.queueDrainWG); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 {
		t.Fatalf("model repeated: %d", p.calls.Load())
	}
}
