package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

func TestUserInteractionFailsFastWithoutCapableFrontend(t *testing.T) {
	client := &Core{}
	for _, agentID := range []string{"lead", "lead/reader"} {
		_, err := client.requestUserInteraction(context.Background(), agentID, "docs", json.RawMessage(`{"method":"elicitation/create"}`))
		if err == nil || (!errors.Is(err, ErrUserInteractionUnsupported) && !strings.Contains(err.Error(), "cannot display MCP user interaction")) {
			t.Fatalf("headless interaction for %q error = %v", agentID, err)
		}
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
	saved, err = client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Approvals) != 0 {
		t.Fatalf("one-time approval was persisted for the session: %#v", saved.Approvals)
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
	if event.Kind != EventUserInteractionRequired || event.AgentID != "lead" || event.Interaction == nil || event.Interaction.AgentID != "lead" || event.Interaction.ServerName != "docs" {
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

func TestSubagentUserInteractionsCanBeResolvedIndependently(t *testing.T) {
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

	agentIDs := []string{"lead/reader", "lead/reviewer"}
	type interactionResult struct {
		agentID  string
		response json.RawMessage
		err      error
	}
	results := make(chan interactionResult, len(agentIDs))
	cancels := make([]context.CancelFunc, 0, len(agentIDs))
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	for _, agentID := range agentIDs {
		callCtx, cancel := context.WithCancel(publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{
			SessionID: saved.ID, TaskID: "task", ExecutionID: "execution", AgentID: agentID,
		}))
		cancels = append(cancels, cancel)
		go func(agentID string, ctx context.Context) {
			response, err := client.requestUserInteraction(ctx, agentID, "docs", json.RawMessage(`{"method":"elicitation/create"}`))
			results <- interactionResult{agentID: agentID, response: response, err: err}
		}(agentID, callCtx)
	}

	events := make(map[string]Event, len(agentIDs))
	for range agentIDs {
		select {
		case event := <-stream:
			if event.Kind != EventUserInteractionRequired || event.Interaction == nil || event.AgentID != event.Interaction.AgentID {
				t.Fatalf("subagent interaction event identity = %#v", event)
			}
			events[event.AgentID] = event
		case <-time.After(time.Second):
			t.Fatal("subagent interaction event was not emitted")
		}
	}
	if len(events) != len(agentIDs) {
		t.Fatalf("interaction events by agent = %#v", events)
	}
	pending, err := runtime.PendingRequests(context.Background())
	if err != nil || len(pending) != len(agentIDs) {
		t.Fatalf("pending subagent interactions = %#v, error=%v", pending, err)
	}
	for _, agentID := range agentIDs {
		response := json.RawMessage(`{"answer":"` + agentID + `"}`)
		if err := runtime.ResolveUserInteraction(events[agentID].Interaction.ID, response); err != nil {
			t.Fatalf("resolve interaction for %q: %v", agentID, err)
		}
	}
	responses := make(map[string]string, len(agentIDs))
	for range agentIDs {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("subagent interaction for %q: %v", result.agentID, result.err)
			}
			responses[result.agentID] = string(result.response)
		case <-time.After(time.Second):
			t.Fatal("subagent interaction did not return after host resolution")
		}
	}
	for _, agentID := range agentIDs {
		want := `{"answer":"` + agentID + `"}`
		if responses[agentID] != want {
			t.Errorf("response for %q = %s, want %s", agentID, responses[agentID], want)
		}
	}
}

func TestSubagentUserInteractionCancellationClearsPending(t *testing.T) {
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

	callCtx, cancelCall := context.WithCancel(publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{
		SessionID: saved.ID, TaskID: "task", ExecutionID: "execution", AgentID: "lead/reader",
	}))
	defer cancelCall()
	result := make(chan error, 1)
	go func() {
		_, err := client.requestUserInteraction(callCtx, "lead/reader", "docs", json.RawMessage(`{"method":"elicitation/create"}`))
		result <- err
	}()
	var required Event
	select {
	case required = <-stream:
		if required.Kind != EventUserInteractionRequired || required.Interaction == nil || required.AgentID != "lead/reader" {
			t.Fatalf("required subagent interaction = %#v", required)
		}
	case <-time.After(time.Second):
		t.Fatal("subagent interaction event was not emitted")
	}
	cancelCall()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled subagent interaction error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("subagent interaction did not stop after branch cancellation")
	}
	if pending, err := runtime.PendingRequests(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("pending requests after cancellation = %#v, error=%v", pending, err)
	}
	select {
	case event := <-stream:
		if event.Kind != EventUserInteractionCancelled || event.AgentID != "lead/reader" || event.InteractionID != required.Interaction.ID {
			t.Fatalf("interaction cancellation event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("subagent interaction cancellation event was not emitted")
	}
	if err := runtime.ResolveUserInteraction(required.Interaction.ID, json.RawMessage(`{"action":"accept"}`)); err == nil {
		t.Fatal("canceled interaction was still resolvable")
	}
}
