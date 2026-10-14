package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
)

func TestActiveTurnWithoutCheckpointIsNotReplayedAfterUpgrade(t *testing.T) {
	provider := &scriptedHostProvider{}
	client := newProgrammaticCore(t, provider)
	ctx := context.Background()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	queueID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	executionID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(storedTurnRequest{Input: json.RawMessage(`"perform a side effect"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.sessions.EnqueueTurn(ctx, session.QueuedTurn{
		SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Request: request,
	}, mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Status: TaskQueued})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.sessions.SetQueuedTurnStatusWithEvent(ctx, saved.ID, queueID, session.QueuedTurnActive,
		mustMarshalEvent(Event{Kind: EventTaskStarted, SessionID: saved.ID, QueueID: queueID, ExecutionID: executionID, Status: TaskRunning})); err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.WaitTurn(ctx, executionID)
	if err != nil || result.Status != TaskFailed {
		t.Fatalf("reconciled result = %#v, error=%v; want missing-checkpoint failure", result, err)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 0 {
		t.Fatalf("provider saw %d calls while reconciling an old active turn", requests)
	}
}

func TestUnknownToolOutcomeCanBeSkippedAndReplayedWithoutExecution(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{hostTextResponse("unused")}}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runID := client.runtime.CheckpointID(saved.ID)
	executionID := "execution-test"
	invocationID := "tool-unknown-outcome"
	call := model.OutputFunctionCall{CallID: "call-unknown", Name: "read_file", Arguments: `{}`}
	if _, _, err := client.sessions.StartToolInvocation(context.Background(), session.ToolInvocation{
		RunID: runID, InvocationID: invocationID, SessionID: saved.ID, ExecutionID: executionID, AgentID: "lead",
		CallID: call.CallID, Name: call.Name, Arguments: json.RawMessage(call.Arguments),
	}); err != nil {
		t.Fatalf("StartToolInvocation: %v", err)
	}
	childInvocationID := "tool-child-unknown-outcome"
	childRunID := runID + ":team-run:worker"
	if _, _, err := client.sessions.StartToolInvocation(context.Background(), session.ToolInvocation{
		RunID: childRunID, InvocationID: childInvocationID, SessionID: saved.ID, ExecutionID: executionID, AgentID: "worker",
		CallID: "child-call", Name: "mcp__server__write", Arguments: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("StartToolInvocation for child: %v", err)
	}
	if err := client.checkpoints.Create(context.Background(), graph.Checkpoint[agentgraph.State]{
		FormatVersion: graph.CheckpointFormatVersion, RunID: runID, MachineID: "lune-agent-v2", Revision: 1, NextID: 2,
		Invocations: []graph.Invocation[agentgraph.State]{{
			ID: "active-tool", Node: "execute_tool", Status: graph.InvocationReady,
			State: agentgraph.State{RunID: runID, ExecutionID: executionID, PendingIndex: 0,
				PendingCalls: []model.OutputFunctionCall{call}},
		}},
	}); err != nil {
		t.Fatalf("create pending checkpoint: %v", err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	pendingRequests, err := runtime.PendingRequests(context.Background())
	var requests []ToolRecoveryRequest
	for _, request := range pendingRequests {
		if request.ToolRecovery != nil {
			requests = append(requests, *request.ToolRecovery)
		}
	}
	if err != nil || len(requests) != 2 {
		t.Fatalf("PendingToolRecoveries = %#v, err=%v", requests, err)
	}
	seen := map[string]bool{}
	for _, request := range requests {
		seen[request.InvocationID] = true
	}
	if !seen[invocationID] || !seen[childInvocationID] {
		t.Fatalf("pending recovery requests = %#v", requests)
	}
	events := subscribeTestEvents(t, client, saved.ID)
	recoveryErr := client.checkToolRecovery(context.Background(), saved.ID, TaskState{TaskID: "resume-task", ExecutionID: executionID})
	var required *ToolRecoveryRequiredError
	if !errors.As(recoveryErr, &required) || required.InvocationID != invocationID {
		t.Fatalf("checkToolRecovery error = %v", recoveryErr)
	}
	for i := 0; i < 2; i++ {
		event := <-events
		if event.Kind != EventToolRecoveryRequired || event.ToolRecovery == nil || !seen[event.ToolRecovery.InvocationID] {
			t.Fatalf("recovery event = %#v", event)
		}
	}
	if err := runtime.ResolveToolRecovery(context.Background(), invocationID, ToolRecoverySkip); err != nil {
		t.Fatalf("ResolveToolRecovery: %v", err)
	}
	if err := runtime.ResolveToolRecovery(context.Background(), childInvocationID, ToolRecoverySkip); err != nil {
		t.Fatalf("ResolveToolRecovery child: %v", err)
	}
	identity := publictool.CallIdentity{SessionID: saved.ID, TaskID: "resume-task", RunID: runID, ExecutionID: executionID, AgentID: "lead", InvocationID: invocationID}
	executed := false
	output, err := client.executeToolCall(publictool.WithCallIdentity(context.Background(), identity), call, func() model.InputFunctionCallOutput {
		executed = true
		return model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("unexpected execution")}
	})
	if err != nil || executed || output.CallID != call.CallID {
		t.Fatalf("replayed skipped tool = %#v, executed=%v, err=%v", output, executed, err)
	}
	if output.Output.Text == nil || !strings.Contains(*output.Output.Text, "chose to skip") {
		t.Fatalf("skipped tool output = %#v", output.Output)
	}

	completedID := "tool-completed-result"
	completedCall := model.OutputFunctionCall{CallID: "call-completed", Name: "read_file", Arguments: `{}`}
	completedContext := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{
		SessionID: saved.ID, TaskID: "resume-task", RunID: runID, ExecutionID: executionID,
		AgentID: "lead", InvocationID: completedID,
	})
	executions := 0
	execute := func() model.InputFunctionCallOutput {
		executions++
		return model.InputFunctionCallOutput{
			CallID: completedCall.CallID, Output: model.NewToolOutputText("saved result"),
			TranscriptOnly: []model.ContentPart{model.InputText{Text: "host-only transcript"}},
		}
	}
	firstOutput, err := client.executeToolCall(completedContext, completedCall, execute)
	if err != nil {
		t.Fatalf("first tool execution: %v", err)
	}
	replayedOutput, err := client.executeToolCall(completedContext, completedCall, execute)
	if err != nil || executions != 1 || replayedOutput.CallID != firstOutput.CallID || len(replayedOutput.TranscriptOnly) != 1 {
		t.Fatalf("completed tool replay = %#v, calls=%d, err=%v", replayedOutput, executions, err)
	}
}
