package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
)

func TestInterruptGoalRejectsAnotherSessionsExecution(t *testing.T) {
	started := make(chan struct{}, 2)
	provider := &goalProvider{respond: func(ctx context.Context, _ int, _ publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	client, first := newGoalTestCore(t, provider)
	saved, err := client.CreateSession(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.OpenSessionRuntime(t.Context(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = second.PauseGoal(context.Background()); _ = second.Close() })
	for _, runtime := range []*SessionRuntime{first, second} {
		if _, err := runtime.CreateGoal(t.Context(), GoalCreateOptions{Objective: "Wait for cancellation"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("goals did not start")
		}
	}
	state, ok := client.StateForSession(first.sessionID)
	if !ok || state.Status != TaskRunning {
		t.Fatalf("first state=%+v", state)
	}
	if second.Interrupt(state.ExecutionID) {
		t.Error("accepted another session's execution ID")
	}
	goal, err := second.GetGoal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if goal.Status != GoalActive {
		t.Errorf("foreign interrupt changed second goal: %+v", goal)
	}
	if !first.Interrupt(state.ExecutionID) {
		t.Error("rejected own active goal execution")
	}
	goal, err = first.GetGoal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if goal.Status != GoalPaused {
		t.Errorf("own interrupt did not pause goal: %+v", goal)
	}
}

func TestRuntimeStateIncludesFullQueueWhenNotRunning(t *testing.T) {
	for _, headStatus := range []string{session.QueuedTurnPaused, session.QueuedTurnQueued, session.QueuedTurnActive} {
		t.Run(headStatus, func(t *testing.T) {
			provider := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
				return hostTextResponse("unused"), nil
			}}
			client, runtime := newGoalTestCore(t, provider)
			client.queueDrainWG.Wait()
			for i := range 3 {
				_, _, err := client.sessions.EnqueueTurn(t.Context(), session.QueuedTurn{
					SessionID: runtime.sessionID, QueueID: fmt.Sprintf("q%d", i), ExecutionID: fmt.Sprintf("e%d", i), Request: json.RawMessage(`{"input":"queued"}`),
				}, json.RawMessage(`{}`))
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := client.sessions.SetQueuedTurnStatus(t.Context(), runtime.sessionID, "q0", headStatus); err != nil {
				t.Fatal(err)
			}
			state, err := runtime.State(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Queue) != 3 {
				t.Fatalf("queue has %d turns, want 3: %+v", len(state.Queue), state)
			}
			for i, item := range state.Queue {
				if item.ExecutionID != fmt.Sprintf("e%d", i) {
					t.Fatalf("queue order: %+v", state.Queue)
				}
			}
			want := TaskQueued
			if headStatus == session.QueuedTurnPaused {
				want = TaskPaused
			}
			if state.Task.ExecutionID != "e0" || state.Task.Status != want {
				t.Fatalf("incorrect head task: %+v", state.Task)
			}
		})
	}
}

func TestInterruptGoalRejectsStaleGoalIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		revision int64
	}{
		{name: "previous goal", id: "old-goal", revision: 2},
		{name: "previous revision", id: "current-goal", revision: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
				return hostTextResponse("unused"), nil
			}}
			client, runtime := newGoalTestCore(t, provider)
			client.queueDrainWG.Wait()
			_, err := client.changeGoal(t.Context(), runtime.sessionID, "", func(*Goal) (*Goal, error) {
				return &Goal{ID: "current-goal", Revision: 2, Status: GoalActive, Objective: "Current objective"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			client.goalMu.Lock()
			client.goalRuns = map[string]goalExecution{"stale": {ID: tc.id, Revision: tc.revision}}
			client.goalArmed = map[string]bool{runtime.sessionID: true}
			client.goalMu.Unlock()
			if runtime.Interrupt("stale") {
				t.Error("accepted stale execution identity")
			}
			goal, err := runtime.GetGoal(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if goal.Status != GoalActive || goal.ID != "current-goal" || goal.Revision != 2 {
				t.Fatalf("stale interrupt changed goal: %+v", goal)
			}
			client.goalMu.Lock()
			armed := client.goalArmed[runtime.sessionID]
			client.goalMu.Unlock()
			if !armed {
				t.Fatal("stale interrupt disabled automatic continuation")
			}
		})
	}
}
