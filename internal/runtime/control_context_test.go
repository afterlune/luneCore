package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestCoreCloseDeadlineDoesNotCloseBusyStorage(t *testing.T) {
	provider := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		return hostTextResponse("unused"), nil
	}}
	client, rt := newGoalTestCore(t, provider)
	client.queueDrainWG.Wait()
	client.sessionOps.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := client.Close(ctx)
	client.sessionOps.Done()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error=%v", err)
	}
	if _, err := client.GetSession(t.Context(), rt.sessionID); err != nil {
		t.Fatalf("storage was closed before its operation finished: %v", err)
	}
	if err := client.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestControlCancellationAndInvalidApprovalDoNotConsumeRequest(t *testing.T) {
	provider := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		return hostTextResponse("unused"), nil
	}}
	client, rt := newGoalTestCore(t, provider)
	client.queueDrainWG.Wait()
	response := make(chan publictool.ApprovalDecision, 1)
	client.mu.Lock()
	client.approvals["request"] = pendingApproval{sessionID: rt.sessionID, request: ApprovalRequest{ID: "request"}, response: response}
	client.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := rt.ResolveApprovalDecision(ctx, "request", publictool.ApprovalAllowOnce); !errors.Is(err, context.Canceled) {
		t.Fatalf("approval error=%v", err)
	}
	if err := rt.ResolveApprovalDecision(t.Context(), "request", publictool.ApprovalDecision("invalid")); err == nil {
		t.Fatal("invalid approval accepted")
	}
	client.mu.RLock()
	_, pending := client.approvals["request"]
	client.mu.RUnlock()
	if !pending {
		t.Fatal("failed resolution consumed approval")
	}
	if accepted, err := rt.Interrupt(ctx, "missing"); accepted || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt=%v,%v", accepted, err)
	}
	if err := rt.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("runtime close error=%v", err)
	}
}

func TestGoalAdmissionWaitsBeforeCheckingSessionState(t *testing.T) {
	started := make(chan struct{}, 1)
	provider := &goalProvider{respond: func(ctx context.Context, _ int, _ publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	client, rt := newGoalTestCore(t, provider)
	client.queueDrainWG.Wait()
	client.queueMutationMu.Lock()
	goalDone := make(chan error, 1)
	go func() {
		_, err := rt.CreateGoal(t.Context(), GoalCreateOptions{Objective: "new goal"})
		goalDone <- err
	}()
	input, _ := NewTaskInput("ordinary turn")
	sub, _, err := client.submitTurnAdmitted(t.Context(), rt.sessionID, TurnRequest{Input: input}, StartIfIdle, false)
	client.queueMutationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rt.Interrupt(context.Background(), sub.ExecutionID) })
	if err := <-goalDone; !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("goal accepted despite admitted turn: %v", err)
	}
}

func TestGoalAdmissionDeadlineWhileGateIsHeld(t *testing.T) {
	provider := &goalProvider{respond: func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) {
		return hostTextResponse("unused"), nil
	}}
	client, rt := newGoalTestCore(t, provider)
	client.queueDrainWG.Wait()
	client.queueMutationMu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, err := rt.CreateGoal(ctx, GoalCreateOptions{Objective: "must wait for admission"})
	cancel()
	client.queueMutationMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	goal, err := rt.GetGoal(t.Context())
	if err != nil || goal != nil {
		t.Fatalf("goal was admitted: %+v,%v", goal, err)
	}
}
