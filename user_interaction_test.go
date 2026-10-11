package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

func TestUserInteractionFailsFastWithoutCapableFrontend(t *testing.T) {
	client := &Core{}
	_, err := client.requestUserInteraction(context.Background(), "lead", "docs", json.RawMessage(`{"method":"elicitation/create"}`))
	if err == nil || !strings.Contains(err.Error(), "use the TUI") {
		t.Fatalf("headless interaction error = %v", err)
	}
}

func TestInternalSynchronousEventDrainIsNotAnApprovalFrontend(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events, err := runtime.subscribeEvents(ctx, 0, false, false)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range events {
		}
	}()
	defer func() { cancel(); <-done }()
	callCtx := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: saved.ID, TaskID: "task", ExecutionID: "execution"})
	if client.requestApproval(callCtx, toolPermissionRequestForTest()) {
		t.Fatal("approval unexpectedly succeeded without an attached control subscriber")
	}
}

func TestUserInteractionEventCanBeResolved(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime.SetUserInteractionAvailable(true)
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	stream, err := runtime.SubscribeEvents(streamCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancelStream()
		for range stream {
		}
	}()
	result := make(chan userInteractionResult, 1)
	go func() {
		ctx := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: saved.ID, TaskID: "task", ExecutionID: "execution", AgentID: "lead"})
		response, err := client.requestUserInteraction(ctx, "lead", "docs", json.RawMessage(`{"method":"elicitation/create"}`))
		result <- userInteractionResult{Response: response, Err: err}
	}()
	var event Event
	select {
	case event = <-stream:
	case <-time.After(time.Second):
		t.Fatal("user interaction event was not emitted")
	}
	if event.Kind != EventUserInteractionRequired || event.Interaction == nil || event.Interaction.ServerName != "docs" {
		t.Fatalf("interaction event = %#v", event)
	}
	response := json.RawMessage(`{"action":"decline"}`)
	pending, err := runtime.PendingRequests(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Interaction == nil || string(pending[0].Interaction.Request) != `{"method":"elicitation/create"}` {
		t.Fatalf("pending interaction snapshot = %#v, error=%v", pending, err)
	}
	if err := runtime.ResolveUserInteraction(event.Interaction.ID, response); err != nil {
		t.Fatal(err)
	}
	select {
	case resolved := <-result:
		if resolved.Err != nil || string(resolved.Response) != string(response) {
			t.Fatalf("resolved interaction = %#v", resolved)
		}
	case <-time.After(time.Second):
		t.Fatal("interaction request did not return")
	}
}

func TestSubagentUserInteractionFailsExplicitly(t *testing.T) {
	client := &Core{}
	_, err := client.requestUserInteraction(context.Background(), "team-worker-1", "docs", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "subagents") {
		t.Fatalf("subagent interaction error = %v", err)
	}
}
