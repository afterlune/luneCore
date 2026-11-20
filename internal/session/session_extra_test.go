package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/command"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestGoalsDraftsAndTurnResults(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	saved := Snapshot{ID: "s-extra", Workspace: t.TempDir(), Data: json.RawMessage(`{}`), UpdatedAt: now}
	if err := store.Create(ctx, saved); err != nil {
		t.Fatal(err)
	}

	// Goals: initially nil
	if g, err := store.Goal(ctx, saved.ID); err != nil || g != nil {
		t.Fatalf("initial Goal = %#v, %v", g, err)
	}
	goal, changed, err := store.ModifyGoal(ctx, saved.ID, "", func(prev *Goal) (*Goal, error) {
		return &Goal{ID: "g1", Revision: 1, Objective: "ship tests", Status: GoalActive, CreatedAt: now, UpdatedAt: now}, nil
	}, func(g *Goal) json.RawMessage {
		return json.RawMessage(`{"type":"goal_updated"}`)
	})
	if err != nil || !changed || goal.ID != "g1" {
		t.Fatalf("ModifyGoal create = %#v, %v, %v", goal, changed, err)
	}
	// Idempotent requestKey dedupe
	_, changed2, err := store.ModifyGoal(ctx, saved.ID, "req-1", func(prev *Goal) (*Goal, error) {
		copy := *prev
		copy.TokensUsed += 10
		return &copy, nil
	}, func(*Goal) json.RawMessage { return json.RawMessage(`{"type":"goal_usage"}`) })
	if err != nil || !changed2 {
		t.Fatalf("first requestKey failed: %v, %v", changed2, err)
	}
	_, changed3, err := store.ModifyGoal(ctx, saved.ID, "req-1", func(prev *Goal) (*Goal, error) {
		return prev, nil
	}, func(*Goal) json.RawMessage { return json.RawMessage(`{"type":"goal_usage"}`) })
	if err != nil || changed3 {
		t.Fatalf("duplicate requestKey should not mutate: %v, %v", changed3, err)
	}
	if loaded, err := store.Goal(ctx, saved.ID); err != nil || loaded.TokensUsed != 10 {
		t.Fatalf("Goal after usage = %#v, %v", loaded, err)
	}
	// Delete goal
	if _, changed, err := store.ModifyGoal(ctx, saved.ID, "", func(*Goal) (*Goal, error) { return nil, nil }, func(*Goal) json.RawMessage {
		return json.RawMessage(`{"type":"goal_cleared"}`)
	}); err != nil || !changed {
		t.Fatalf("ModifyGoal delete = %v, %v", changed, err)
	}

	// UserQuestionDrafts
	if err := store.SaveUserQuestionDraft(ctx, saved.ID, "r1", "i1", "c1", []byte(`{"q1":"a"}`)); err != nil {
		t.Fatal(err)
	}
	draft, err := store.UserQuestionDraft(ctx, saved.ID, "r1", "i1", "c1")
	if err != nil || string(draft) != `{"q1":"a"}` {
		t.Fatalf("UserQuestionDraft = %q, %v", draft, err)
	}
	if err := store.DeleteUserQuestionDraft(ctx, saved.ID, "r1", "i1", "c1"); err != nil {
		t.Fatal(err)
	}
	if empty, err := store.UserQuestionDraft(ctx, saved.ID, "r1", "i1", "c1"); err != nil || len(empty) != 0 {
		t.Fatalf("expected empty draft after delete, got %q, %v", empty, err)
	}

	// TurnResult validation & not found
	if _, _, err := store.TurnResult(ctx, "", "e1"); err == nil {
		t.Fatal("expected error for empty session id")
	}
	if _, ok, err := store.TurnResult(ctx, saved.ID, "missing"); err != nil || ok {
		t.Fatalf("expected not found, got ok=%v err=%v", ok, err)
	}

	// Commands CRUD & interrupt
	rec := command.Record{
		Info: publictool.CommandInfo{
			ID: "cmd-1", SessionID: saved.ID, ExecutionID: "exec-1",
			Command: "echo hi", Status: publictool.CommandRunning, StartedAt: now,
		},
	}
	if err := store.SaveCommand(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetCommand(ctx, saved.ID, "cmd-1"); err != nil || got.Info.ID != "cmd-1" {
		t.Fatalf("GetCommand = %#v, %v", got, err)
	}
	if list, err := store.ListCommands(ctx, saved.ID, ""); err != nil || len(list) != 1 {
		t.Fatalf("ListCommands = %#v, %v", list, err)
	}
	if err := store.InterruptCommands(ctx); err != nil {
		t.Fatalf("InterruptCommands = %v", err)
	}
	if err := store.DeleteCommands(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}

	// GraphResumeInputs
	in := GraphResumeInput{RunID: "r1", InvocationID: "i1", CallID: "c1", Payload: []byte(`"ok"`)}
	if inserted, err := store.PutGraphResumeInput(ctx, saved.ID, in); err != nil || !inserted {
		t.Fatalf("PutGraphResumeInput = %v, %v", inserted, err)
	}
	if inserted, err := store.PutGraphResumeInput(ctx, saved.ID, in); err != nil || inserted {
		t.Fatalf("idempotent PutGraphResumeInput = %v, %v", inserted, err)
	}
	if _, err := store.PutGraphResumeInput(ctx, saved.ID, GraphResumeInput{RunID: "r1", InvocationID: "i1", CallID: "c1", Payload: []byte(`"diff"`)}); err == nil {
		t.Fatal("expected conflict error for differing payload")
	}
	if has, err := store.HasGraphResumeInput(ctx, saved.ID, "r1", "i1", "c1"); err != nil || !has {
		t.Fatalf("HasGraphResumeInput = %v, %v", has, err)
	}
	if list, err := store.GraphResumeInputs(ctx, saved.ID, "r1"); err != nil || len(list) != 1 {
		t.Fatalf("GraphResumeInputs = %#v, %v", list, err)
	}
	if err := store.MarkGraphResumeInputConsumed(ctx, saved.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGraphResumeInput(ctx, saved.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGraphResumeInputs(ctx, saved.ID, "r1"); err != nil {
		t.Fatal(err)
	}

	// RuntimeEvents & TokenBudget delete
	if cursor, err := store.AppendRuntimeEvent(ctx, saved.ID, json.RawMessage(`{"type":"test"}`)); err != nil || cursor <= 0 {
		t.Fatalf("AppendRuntimeEvent = %d, %v", cursor, err)
	}
	if latest, err := store.LatestRuntimeEventCursor(ctx, saved.ID); err != nil || latest <= 0 {
		t.Fatalf("LatestRuntimeEventCursor = %d, %v", latest, err)
	}
	if err := store.DeleteExecutionTokenBudget(ctx, "exec-1"); err != nil {
		t.Fatal(err)
	}

	// FileLocks
	lockDir := t.TempDir()
	sl, err := AcquireSessionLock(lockDir, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = sl.Close()
	fl, err := AcquireFileLock(filepath.Join(lockDir, "ex.lock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = fl.Close()
	sh, err := AcquireSharedFileLock(filepath.Join(lockDir, "sh.lock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = sh.Close()
	tl, err := AcquireFileLockWithTimeout(filepath.Join(lockDir, "to.lock"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_ = tl.Close()

	// QueuedTurns lifecycle & ForkPausedTask
	q1, _, err := store.EnqueueTurn(ctx, QueuedTurn{
		SessionID: saved.ID, QueueID: "q1", ExecutionID: "ex-1",
		Request: json.RawMessage(`{"text":"hello"}`),
	}, json.RawMessage(`{"type":"queued"}`))
	if err != nil || q1.Status != QueuedTurnQueued {
		t.Fatalf("EnqueueTurn = %#v, %v", q1, err)
	}
	claimed, ok, err := store.ClaimNextQueuedTurn(ctx, saved.ID)
	if err != nil || !ok || claimed.QueueID != "q1" {
		t.Fatalf("ClaimNextQueuedTurn = %#v, %v, %v", claimed, ok, err)
	}
	if byExec, found, err := store.QueuedTurnByExecutionID(ctx, saved.ID, "ex-1"); err != nil || !found || byExec.QueueID != "q1" {
		t.Fatalf("QueuedTurnByExecutionID = %#v, %v, %v", byExec, found, err)
	}
	if _, err := store.PauseQueuedTurnWithReason(ctx, saved.ID, "q1", "ex-1", "paused", "max_steps", json.RawMessage(`{"type":"paused"}`), nil); err != nil {
		t.Fatal(err)
	}
	forkSnap := Snapshot{ID: "s-fork", Workspace: saved.Workspace, Data: json.RawMessage(`{}`), UpdatedAt: now}
	if err := store.Create(ctx, forkSnap); err != nil {
		t.Fatal(err)
	}
	if err := store.ForkPausedTask(ctx, saved.ID, "r1", "ex-1", forkSnap.ID, "r2", "ex-2", "q2", json.RawMessage(`{"text":"hello"}`), json.RawMessage(`{"type":"forked"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.DiscardQueuedTurn(ctx, saved.ID, "q1", "ex-1", "discarded", json.RawMessage(`{"type":"discarded"}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.EnqueueTurn(ctx, QueuedTurn{
		SessionID: saved.ID, QueueID: "q3", ExecutionID: "ex-3",
		Request: json.RawMessage(`{"text":"cancel me"}`),
	}, json.RawMessage(`{"type":"queued"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CancelQueuedTurn(ctx, saved.ID, "q3", "cancelled", func(QueuedTurn) json.RawMessage {
		return json.RawMessage(`{"type":"cancelled"}`)
	}); err != nil {
		t.Fatal(err)
	}
}
