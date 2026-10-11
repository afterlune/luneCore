package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUserInteractionFailsFastWithoutCapableFrontend(t *testing.T) {
	client := &Core{handler: func(Event) {}, interactions: make(map[string]chan userInteractionResult)}
	_, err := client.requestUserInteraction(context.Background(), "lead", "docs", json.RawMessage(`{"method":"elicitation/create"}`))
	if err == nil || !strings.Contains(err.Error(), "use the TUI") {
		t.Fatalf("headless interaction error = %v", err)
	}
}

func TestUserInteractionEventCanBeResolved(t *testing.T) {
	events := make(chan Event, 1)
	client := &Core{
		handler:                  func(event Event) { events <- event },
		interactions:             make(map[string]chan userInteractionResult),
		userInteractionAvailable: true,
	}
	result := make(chan userInteractionResult, 1)
	go func() {
		response, err := client.requestUserInteraction(context.Background(), "lead", "docs", json.RawMessage(`{"method":"elicitation/create"}`))
		result <- userInteractionResult{Response: response, Err: err}
	}()
	var event Event
	select {
	case event = <-events:
	case <-time.After(time.Second):
		t.Fatal("user interaction event was not emitted")
	}
	if event.Kind != EventUserInteractionRequired || event.Interaction == nil || event.Interaction.ServerName != "docs" {
		t.Fatalf("interaction event = %#v", event)
	}
	response := json.RawMessage(`{"action":"decline"}`)
	if err := client.ResolveUserInteraction(event.Interaction.ID, response); err != nil {
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
	client := &Core{handler: func(Event) {}, interactions: make(map[string]chan userInteractionResult), userInteractionAvailable: true}
	_, err := client.requestUserInteraction(context.Background(), "team-worker-1", "docs", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "subagents") {
		t.Fatalf("subagent interaction error = %v", err)
	}
}
