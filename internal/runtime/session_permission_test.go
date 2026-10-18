package runtime

import (
	"context"
	"encoding/json"
	publicmodel "github.com/afterlune/luneCore/model"
	"testing"
)

func TestExplicitTurnPermissionsArePinnedWithoutChangingCore(t *testing.T) {
	for _, mode := range []PermissionMode{PermissionReadOnly, PermissionWorkspace, PermissionAsk, PermissionFullAccess} {
		t.Run(string(mode), func(t *testing.T) {
			provider := &barrierHostProvider{entered: make(chan string, 2), release: make(chan struct{}, 2)}
			client := newProgrammaticCore(t, provider)
			saved, err := client.CreateSession(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
			if err != nil {
				t.Fatal(err)
			}
			first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "first"), PermissionMode: mode}, StartIfIdle)
			if err != nil {
				t.Fatal(err)
			}
			_ = receiveProviderInput(t, provider.entered)
			second, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "queued"), PermissionMode: mode}, QueueInput)
			if err != nil {
				t.Fatal(err)
			}
			if err = client.SetPermissionMode(PermissionFullAccess); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{first.ExecutionID, second.ExecutionID} {
				row, found, err := client.sessions.QueuedTurnByExecutionID(context.Background(), saved.ID, id)
				if err != nil || !found {
					t.Fatalf("queue found=%v err=%v", found, err)
				}
				var stored storedTurnRequest
				if err = json.Unmarshal(row.Request, &stored); err != nil {
					t.Fatal(err)
				}
				if stored.PermissionOverride != string(mode) {
					t.Fatalf("permission=%q want=%q", stored.PermissionOverride, mode)
				}
			}
			provider.release <- struct{}{}
			_ = receiveProviderInput(t, provider.entered)
			provider.release <- struct{}{}
			if _, err = runtime.WaitTurn(context.Background(), second.ExecutionID); err != nil {
				t.Fatal(err)
			}
			if client.PermissionMode() != PermissionFullAccess {
				t.Fatal("turn mutated Core mode")
			}
		})
	}
}

func TestPlanPinsReadOnlyEvenWhenHostRequestsFullAccess(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 1), release: make(chan struct{}, 1)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "plan"), PlanMode: true, PermissionMode: PermissionFullAccess}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveProviderInput(t, provider.entered)
	row, found, err := client.sessions.QueuedTurnByExecutionID(context.Background(), saved.ID, submission.ExecutionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	var stored storedTurnRequest
	if err = json.Unmarshal(row.Request, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.PermissionOverride != "read-only" {
		t.Fatalf("Plan permission=%q", stored.PermissionOverride)
	}
	provider.release <- struct{}{}
	if _, err = runtime.WaitTurn(context.Background(), submission.ExecutionID); err != nil {
		t.Fatal(err)
	}
}

func TestWaitTurnWhileResultReplacesQueueEntry(t *testing.T) {
	responses := make([]*publicmodel.ModelResponse, 32)
	for i := range responses {
		responses[i] = hostTextResponse("done")
	}
	client := newProgrammaticCore(t, &scriptedHostProvider{responses: responses})
	saved, err := client.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range responses {
		submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "complete immediately"), PermissionMode: PermissionWorkspace}, QueueInput)
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.WaitTurn(context.Background(), submission.ExecutionID)
		if err != nil || result.Status != TaskCompleted {
			t.Fatalf("completion=%+v err=%v", result, err)
		}
	}
}

func TestInvalidExplicitPermissionIsRejectedBeforeAdmission(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range []bool{false, true} {
		if _, err = runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "invalid"), PermissionMode: "invalid", PlanMode: plan}, StartIfIdle); err == nil {
			t.Fatal("accepted invalid permission")
		}
	}
	state, err := runtime.State(context.Background())
	if err != nil || len(state.Queue) > 0 {
		t.Fatalf("invalid admission changed queue: %+v err=%v", state, err)
	}
}
