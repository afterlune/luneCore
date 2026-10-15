package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
	graph "github.com/afterlune/luneGraph"
)

func TestOpenSessionRuntimeDiscardsLegacyPausedCheckpoint(t *testing.T) {
	ctx := context.Background()
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	transcript := model.NewResponseInputItems(model.InputMessage{
		Role: model.RoleUser, Content: model.NewMessageText("keep this conversation"),
	})
	if err := client.setSessionConversation(ctx, saved.ID, transcript, transcript, ""); err != nil {
		t.Fatal(err)
	}
	before, err := client.GetSession(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}

	const queueID, executionID = "legacy-queue", "legacy-execution"
	request, err := json.Marshal(storedTurnRequest{Input: json.RawMessage(`"unfinished work"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.sessions.EnqueueTurn(ctx, session.QueuedTurn{
		SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Request: request,
	}, mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Status: TaskQueued})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.sessions.SetQueuedTurnStatusWithEvent(ctx, saved.ID, queueID, session.QueuedTurnPaused,
		mustMarshalEvent(Event{Kind: EventTaskFinished, SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Status: TaskPaused})); err != nil {
		t.Fatal(err)
	}
	if err := client.sessions.SetExecutionTokenBudget(ctx, saved.ID, executionID, 10_000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.sessions.StartToolInvocation(ctx, session.ToolInvocation{
		RunID: client.runtime.CheckpointID(saved.ID), InvocationID: "legacy-tool", SessionID: saved.ID,
		ExecutionID: executionID, AgentID: "lead", CallID: "call-1", Name: "read_file",
		Arguments: json.RawMessage(`{"path":"old.txt"}`),
	}); err != nil {
		t.Fatal(err)
	}
	runID := client.runtime.CheckpointID(saved.ID)
	if err := client.checkpoints.Create(ctx, graph.Checkpoint[agentgraph.State]{
		FormatVersion: graph.CheckpointFormatVersion, RunID: runID, MachineID: legacyAcceptanceMachineID, Revision: 1, NextID: 2,
		Invocations: []graph.Invocation[agentgraph.State]{{
			ID: "i1", Node: "accept_candidate", Status: graph.InvocationWaiting, Continuation: "acceptance_resolution",
			CallID: "accept-1", State: agentgraph.State{
				RunID: runID, ExecutionID: executionID, TaskSettingsVersion: legacyAcceptanceTaskSettingsVersion,
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	runtime, err := client.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	state, err := runtime.State(ctx)
	if err != nil || state.Task.Status != TaskIdle || len(state.Queue) != 0 {
		t.Fatalf("runtime state = %#v, error=%v; want idle with no stale queue", state, err)
	}
	if _, found, err := client.pendingState(ctx, saved.ID); err != nil || found {
		t.Fatalf("legacy checkpoint remains: found=%v error=%v", found, err)
	}
	loaded, err := client.GetSession(ctx, saved.ID)
	if err != nil || string(loaded.History) != string(before.History) {
		t.Fatalf("conversation history changed: before=%s after=%s error=%v", before.History, loaded.History, err)
	}
	toolInvocations, err := client.sessions.PendingToolInvocations(ctx, saved.ID, executionID)
	if err != nil || len(toolInvocations) != 0 {
		t.Fatalf("legacy tool invocations = %#v, error=%v", toolInvocations, err)
	}
	if _, _, err := client.sessions.ReserveExecutionTokens(ctx, executionID, 1, 1); err == nil {
		t.Fatal("legacy execution token budget remains")
	}
}

func TestOpenSessionRuntimeKeepsFutureCheckpoint(t *testing.T) {
	ctx := context.Background()
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runID := client.runtime.CheckpointID(saved.ID)
	if err := client.checkpoints.Create(ctx, graph.Checkpoint[agentgraph.State]{
		FormatVersion: graph.CheckpointFormatVersion, RunID: runID, MachineID: "lune-agent-v5", Revision: 1, NextID: 2,
		Invocations: []graph.Invocation[agentgraph.State]{{
			ID: "i1", Node: "model_request", Status: graph.InvocationReady,
			State: agentgraph.State{RunID: runID, ExecutionID: "future-execution", TaskSettingsVersion: agentgraph.CurrentTaskSettingsVersion + 1},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenSessionRuntime(ctx, saved.ID); err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	checkpoint, err := client.runtime.CheckpointForSession(ctx, saved.ID)
	if err != nil || checkpoint.MachineID != "lune-agent-v5" {
		t.Fatalf("future checkpoint = %#v, error=%v; want preserved", checkpoint, err)
	}
}

func TestSessionRuntimeCompletesConsecutiveShortMessages(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostTextResponse("Hi! What can I help with?"),
		hostTextResponse("You sent 1."),
	}}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"hi", "1"} {
		input, err := NewTaskInput(text)
		if err != nil {
			t.Fatal(err)
		}
		submission, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: input}, StartIfIdle)
		if err != nil {
			t.Fatalf("SubmitTurn(%q): %v", text, err)
		}
		result, err := runtime.WaitTurn(context.Background(), submission.ExecutionID)
		if err != nil || result.Status != TaskCompleted {
			t.Fatalf("WaitTurn(%q) = %#v, error=%v", text, result, err)
		}
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.requests != 2 {
		t.Fatalf("provider requests = %d, want one normal request per message", provider.requests)
	}
}
