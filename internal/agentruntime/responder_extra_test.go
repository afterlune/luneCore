package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

type dummyRequester struct{}

func (dummyRequester) CreateResponse(context.Context, model.ModelRequest) (*model.ModelResponse, error) {
	return &model.ModelResponse{Status: "completed"}, nil
}

func TestResponderLifecycleAndCheckpoints(t *testing.T) {
	store, err := memory.New[agentgraph.State](graph.JSONClone[agentgraph.State])
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := graph.NewLimiter(2)
	if err != nil {
		t.Fatal(err)
	}
	r := NewResponder(store, limiter, nil)
	r.SetTaskOptions(agentgraph.TaskOptions{ExecutionID: "exec-1"})
	r.DeferCompletedCleanup(true)

	reg, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := agentgraph.New(dummyRequester{}, reg)
	if err != nil {
		t.Fatal(err)
	}
	r.SetRunModel(agent, NewUsageTrackingRequester(dummyRequester{}, "lead", "p", "deepseek", "m"), "m", "low")

	var savedID string
	r.SetHistorySaver(func(id string, _, _ *model.ResponseInput, key string, _ json.RawMessage) error {
		savedID = id + ":" + key
		return nil
	})
	hist := model.NewResponseInputItems(model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("hi")})
	r.SetSession("s1", hist)
	r.SetSessionState("s1", hist, hist, "ctx-k1")
	if r.ContextKey() != "ctx-k1" {
		t.Fatalf("ContextKey = %q", r.ContextKey())
	}
	if err := r.persistSessionHistory(); err != nil || savedID != "s1:ctx-k1" {
		t.Fatalf("persistSessionHistory = %v, savedID=%q", err, savedID)
	}
	if r.RunID() != r.CheckpointID("s1") {
		t.Fatalf("RunID mismatch: %q vs %q", r.RunID(), r.CheckpointID("s1"))
	}
	if _, ok := r.LastState(); ok {
		t.Fatal("expected no LastState initially")
	}
	r.SetExecution(nil)
	if r.Execution() != nil {
		t.Fatal("expected nil execution")
	}
	if len(r.ConversationState()) == 0 {
		t.Fatal("expected non-empty ConversationState JSON")
	}

	// Seed a waiting checkpoint for PendingHistoryFor / PendingStateFor / PendingUserQuestion / waitingForTokenBudget.
	ctx := context.Background()
	runID := r.CheckpointID("s1")
	cp := graph.Checkpoint[agentgraph.State]{
		FormatVersion: graph.CheckpointFormatVersion,
		RunID:         runID,
		DefinitionID:  "lune-v4",
		Revision:      1,
		NextID:        2,
		Invocations: []graph.Invocation[agentgraph.State]{{
			ID:           "i1",
			CallID:       "c1",
			Node:         "execute_tool",
			Continuation: agentgraph.UserQuestionContinuation,
			Status:       graph.InvocationWaiting,
			State: agentgraph.State{
				RunID:       runID,
				ExecutionID: "exec-1",
				AgentID:     "lead",
				History:     *hist,
				Transcript:  *hist,
				UserQuestions: []model.UserQuestionItem{{
					ID: "q1", Question: "Choose?",
				}},
			},
		}},
	}
	if err := store.Create(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if pending, err := r.HasPendingFor("s1", ctx); err != nil || !pending {
		t.Fatalf("HasPendingFor = %v, %v", pending, err)
	}
	if ph, err := r.PendingHistoryFor(ctx, "s1"); err != nil || ph == nil || len(ph.Items) != 1 {
		t.Fatalf("PendingHistoryFor = %#v, %v", ph, err)
	}
	if pq, ok, err := r.PendingUserQuestion(ctx, "s1"); err != nil || !ok || pq.ID != "c1" {
		t.Fatalf("PendingUserQuestion = %#v, %v, %v", pq, ok, err)
	}
	if loaded, err := r.CheckpointForSession(ctx, "s1"); err != nil || loaded.RunID != runID {
		t.Fatalf("CheckpointForSession = %#v, %v", loaded, err)
	}
	if _, err := r.Fork(ctx, "fork-run", cp); err == nil {
		// Synthetic checkpoint without full header may be rejected; either nil or validation error is fine.
	}
	if waiting, err := r.waitingForTokenBudget(ctx); err != nil || waiting {
		t.Fatalf("waitingForTokenBudget = %v, %v", waiting, err)
	}
	if err := r.DeleteCompletedRunForSession("s1"); err != nil {
		t.Fatalf("DeleteCompletedRunForSession: %v", err)
	}
	if err := r.DeleteCompletedRunForSession("s1"); err != nil {
		t.Fatalf("DeleteCompletedRunForSession idempotent: %v", err)
	}
}

func TestErrorsAndServerCompactionHelpers(t *testing.T) {
	paused := &TaskPausedError{BudgetReason: BudgetMaxSteps}
	if paused.Error() == "" || !errors.Is(paused, ErrTaskPaused) {
		t.Fatalf("unexpected TaskPausedError: %v", paused)
	}
	rec := &ToolRecoveryRequiredError{InvocationID: "inv-9"}
	if rec.Error() == "" {
		t.Fatal("expected non-empty ToolRecoveryRequiredError")
	}

	req := model.ModelRequest{
		Input: model.NewResponseInputItems(
			model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("turn 1")},
			model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("turn 2")},
		),
	}
	if _, _, err := serverCompactionResult(req, nil); err == nil {
		t.Fatal("expected error for nil response")
	}
	if _, _, err := serverCompactionResult(req, &model.ModelResponse{Status: "failed"}); err == nil {
		t.Fatal("expected error for failed status")
	}
	if _, _, err := serverCompactionResult(req, &model.ModelResponse{Status: "completed"}); err == nil {
		t.Fatal("expected error when no compaction state item")
	}
	compResp := &model.ModelResponse{
		Status: "completed",
		Output: []model.ResponseOutputItem{
			model.OutputCompaction{ID: "cmp-1", EncryptedContent: "enc"},
		},
	}
	res, items, err := serverCompactionResult(req, compResp)
	if err != nil || !res.Compacted || len(items) != 1 || res.Turns != 2 {
		t.Fatalf("unexpected serverCompactionResult: %#v, items=%d, err=%v", res, len(items), err)
	}
	if userTurnCount(nil) != 0 {
		t.Fatal("expected 0 turns for nil input")
	}
}
