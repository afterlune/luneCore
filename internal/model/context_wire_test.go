package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRuntimeContextUpdatesStayInHistoryOnBothProtocols(t *testing.T) {
	first := ModelRequest{Instructions: "fixed rules", Input: NewResponseInputItems(
		InputMessage{Role: RoleUser, Content: NewMessageText("[Lune runtime context]\nPlan mode")},
		InputMessage{Role: RoleUser, Content: NewMessageText("plan this")},
	)}
	next := first
	next.Input = NewResponseInputItems(append(append([]InputItem(nil), first.Input.Items...),
		InputMessage{Role: RoleAssistant, Content: NewMessageText("plan ready")},
		InputMessage{Role: RoleUser, Content: NewMessageText("[Lune runtime context]\nImplementation mode")},
		InputMessage{Role: RoleUser, Content: NewMessageText("implement")},
	)...)
	before, err := toAnthropicRequest(first, ModelProfile{Model: "test", MaxOutputTokens: 100}, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := toAnthropicRequest(next, ModelProfile{Model: "test", MaxOutputTokens: 100}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.System, after.System) {
		t.Fatal("context update rewrote Anthropic system")
	}
	if !reflect.DeepEqual(before.Messages, after.Messages[:len(before.Messages)]) {
		t.Fatal("context update rewrote Anthropic message prefix")
	}
	var wire struct {
		Instructions string
		Input        ResponseInput
	}
	encoded, err := json.Marshal(ResponsesRequest(next))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Instructions != first.Instructions || len(wire.Input.Items) != 5 {
		t.Fatal("Responses lost fixed instructions or history updates")
	}
}
