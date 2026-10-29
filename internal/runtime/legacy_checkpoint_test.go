package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	publicmodel "github.com/afterlune/luneCore/model"
	graph "github.com/afterlune/luneGraph"
)

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
