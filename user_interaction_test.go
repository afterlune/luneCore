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

func TestApprovalWaitsWithoutSubscriberAndCanBeResolvedAfterReconnect(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	callCtx := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: saved.ID, TaskID: "task", ExecutionID: "execution"})
	approvalDone := make(chan bool, 1)
	go func() {
		approvalDone <- client.requestApproval(callCtx, toolPermissionRequestForTest())
	}()
	deadline := time.Now().Add(5 * time.Second)
	var pending []PendingRequest
	for time.Now().Before(deadline) {
		pending, err = runtime.PendingRequests(context.Background())
		if err == nil && len(pending) == 1 && pending[0].Approval != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(pending) != 1 || pending[0].Approval == nil {
		t.Fatalf("pending approval without subscriber = %#v, error=%v", pending, err)
	}
	select {
	case approved := <-approvalDone:
		t.Fatalf("approval completed before host resolution: %t", approved)
	default:
	}
	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream, err := runtime.SubscribeEvents(streamCtx, 0)
	if err != nil {
		cancelStream()
		t.Fatal(err)
	}
	defer func() {
		cancelStream()
		for range stream {
		}
	}()
	select {
	case event := <-stream:
		if event.Kind != EventApprovalRequired || event.Approval == nil || event.Approval.ID != pending[0].Approval.ID || string(event.Approval.Arguments) != "null" {
			t.Fatalf("replayed safe approval event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("approval event was not replayed after reconnect")
	}
	if err := runtime.ResolveApproval(pending[0].Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case approved := <-approvalDone:
		if !approved {
			t.Fatal("approval returned false after explicit resolution")
		}
	case <-time.After(time.Second):
		t.Fatal("approval wait did not return after explicit resolution")
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

func TestUserInteractionWaitsWithoutSubscriberWhenAvailable(t *testing.T) {
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
	result := make(chan userInteractionResult, 1)
	go func() {
		ctx := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: saved.ID, TaskID: "task", ExecutionID: "execution", AgentID: "lead"})
		response, err := client.requestUserInteraction(ctx, "lead", "docs", json.RawMessage(`{"method":"elicitation/create"}`))
		result <- userInteractionResult{Response: response, Err: err}
	}()
	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream, err := runtime.SubscribeEvents(streamCtx, 0)
	if err != nil {
		cancelStream()
		t.Fatal(err)
	}
	defer func() {
		cancelStream()
		for range stream {
		}
	}()
	select {
	case event := <-stream:
		if event.Kind != EventUserInteractionRequired || event.Interaction == nil || string(event.Interaction.Request) != "null" {
			t.Fatalf("replayed safe interaction event = %#v", event)
		}
		pending, err := runtime.PendingRequests(context.Background())
		if err != nil || len(pending) != 1 || pending[0].Interaction == nil || string(pending[0].Interaction.Request) != `{"method":"elicitation/create"}` {
			t.Fatalf("pending interaction after reconnect = %#v, error=%v", pending, err)
		}
		if err := runtime.ResolveUserInteraction(event.Interaction.ID, json.RawMessage(`{"action":"accept"}`)); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("interaction event was not replayed after reconnect")
	}
	select {
	case resolved := <-result:
		if resolved.Err != nil || string(resolved.Response) != `{"action":"accept"}` {
			t.Fatalf("resolved interaction = %#v", resolved)
		}
	case <-time.After(time.Second):
		t.Fatal("interaction wait did not return after explicit resolution")
	}
}

func TestSubagentUserInteractionFailsExplicitly(t *testing.T) {
	client := &Core{}
	_, err := client.requestUserInteraction(context.Background(), "team-worker-1", "docs", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "subagents") {
		t.Fatalf("subagent interaction error = %v", err)
	}
}
