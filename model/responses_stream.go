package model

import (
	"bytes"
	"encoding/json"
)

type ResponseEventType string

const (
	EventResponseCreated            ResponseEventType = "response.created"
	EventResponseInProgress         ResponseEventType = "response.in_progress"
	EventOutputItemAdded            ResponseEventType = "response.output_item.added"
	EventOutputItemDone             ResponseEventType = "response.output_item.done"
	EventContentPartAdded           ResponseEventType = "response.content_part.added"
	EventContentPartDone            ResponseEventType = "response.content_part.done"
	EventReasoningTextDelta         ResponseEventType = "response.reasoning_text.delta"
	EventReasoningTextDone          ResponseEventType = "response.reasoning_text.done"
	EventOutputTextDelta            ResponseEventType = "response.output_text.delta"
	EventOutputTextDone             ResponseEventType = "response.output_text.done"
	EventFunctionCallArgumentsDelta ResponseEventType = "response.function_call_arguments.delta"
	EventFunctionCallArgumentsDone  ResponseEventType = "response.function_call_arguments.done"
	EventCustomToolCallInputDelta   ResponseEventType = "response.custom_tool_call_input.delta"
	EventCustomToolCallInputDone    ResponseEventType = "response.custom_tool_call_input.done"
	EventResponseCompleted          ResponseEventType = "response.completed"
	EventResponseIncomplete         ResponseEventType = "response.incomplete"
	EventResponseFailed             ResponseEventType = "response.failed"
)

// ResponseStreamEvent is one semantic event from the Responses stream. Its
// Type determines which optional payload fields are populated. It models the
// JSON data field of each SSE event, not the surrounding SSE frame.
type ResponseStreamEvent struct {
	Type           ResponseEventType
	SequenceNumber int64
	Response       *Response
	OutputIndex    *int
	ItemID         string
	ContentIndex   *int
	SummaryIndex   *int
	Delta          string
	Text           string
	Arguments      string
	Input          string
	Name           string
	Item           ResponseOutputItem
	Part           ContentPart
}

func (v ResponseStreamEvent) MarshalJSON() ([]byte, error) {
	type eventJSON struct {
		Type           ResponseEventType `json:"type"`
		SequenceNumber int64             `json:"sequence_number"`
		Response       *Response         `json:"response,omitempty"`
		OutputIndex    *int              `json:"output_index,omitempty"`
		ItemID         string            `json:"item_id,omitempty"`
		ContentIndex   *int              `json:"content_index,omitempty"`
		SummaryIndex   *int              `json:"summary_index,omitempty"`
		Delta          string            `json:"delta,omitempty"`
		Text           string            `json:"text,omitempty"`
		Arguments      string            `json:"arguments,omitempty"`
		Input          string            `json:"input,omitempty"`
		Name           string            `json:"name,omitempty"`
		Item           json.RawMessage   `json:"item,omitempty"`
		Part           json.RawMessage   `json:"part,omitempty"`
	}
	wire := eventJSON{
		Type: v.Type, SequenceNumber: v.SequenceNumber, Response: v.Response,
		OutputIndex: v.OutputIndex, ItemID: v.ItemID, ContentIndex: v.ContentIndex,
		SummaryIndex: v.SummaryIndex, Delta: v.Delta, Text: v.Text,
		Arguments: v.Arguments, Input: v.Input, Name: v.Name,
	}
	if v.Item != nil {
		item, err := json.Marshal(v.Item)
		if err != nil {
			return nil, err
		}
		wire.Item = item
	}
	if v.Part != nil {
		part, err := json.Marshal(v.Part)
		if err != nil {
			return nil, err
		}
		wire.Part = part
	}
	return json.Marshal(wire)
}

func (v *ResponseStreamEvent) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type           ResponseEventType `json:"type"`
		SequenceNumber int64             `json:"sequence_number"`
		Response       *Response         `json:"response"`
		OutputIndex    *int              `json:"output_index"`
		ItemID         string            `json:"item_id"`
		ContentIndex   *int              `json:"content_index"`
		SummaryIndex   *int              `json:"summary_index"`
		Delta          string            `json:"delta"`
		Text           string            `json:"text"`
		Arguments      string            `json:"arguments"`
		Input          string            `json:"input"`
		Name           string            `json:"name"`
		Item           json.RawMessage   `json:"item"`
		Part           json.RawMessage   `json:"part"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	event := ResponseStreamEvent{
		Type: wire.Type, SequenceNumber: wire.SequenceNumber, Response: wire.Response,
		OutputIndex: wire.OutputIndex, ItemID: wire.ItemID, ContentIndex: wire.ContentIndex,
		SummaryIndex: wire.SummaryIndex, Delta: wire.Delta, Text: wire.Text,
		Arguments: wire.Arguments, Input: wire.Input, Name: wire.Name,
	}
	if len(wire.Item) != 0 && !bytes.Equal(bytes.TrimSpace(wire.Item), []byte("null")) {
		item, err := decodeResponseOutputItem(wire.Item)
		if err != nil {
			return err
		}
		event.Item = item
	}
	if len(wire.Part) != 0 && !bytes.Equal(bytes.TrimSpace(wire.Part), []byte("null")) {
		part, err := decodeContentPart(wire.Part)
		if err != nil {
			return err
		}
		event.Part = part
	}
	*v = event
	return nil
}
