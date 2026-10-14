package model

import (
	"encoding/json"
	"fmt"
)

type ModelResponse struct {
	ID                 string               `json:"id"`
	Object             string               `json:"object"`
	CreatedAt          int64                `json:"created_at"`
	Status             string               `json:"status"`
	Error              *ResponseError       `json:"error"`
	IncompleteDetails  *IncompleteDetails   `json:"incomplete_details"`
	Model              string               `json:"model"`
	Output             []ResponseOutputItem `json:"output"`
	Usage              ResponseUsage        `json:"usage"`
	Store              bool                 `json:"store"`
	ParallelToolCalls  bool                 `json:"parallel_tool_calls"`
	PreviousResponseID *string              `json:"previous_response_id"`
}

func (r *ModelResponse) UnmarshalJSON(data []byte) error {
	type responseAlias ModelResponse
	var wire struct {
		*responseAlias
		Output []json.RawMessage `json:"output"`
	}
	wire.responseAlias = (*responseAlias)(r)
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Output != nil {
		r.Output = make([]ResponseOutputItem, 0, len(wire.Output))
		for _, raw := range wire.Output {
			item, err := decodeResponseOutputItem(raw)
			if err != nil {
				return err
			}
			r.Output = append(r.Output, item)
		}
	}
	return nil
}

// Response is an internal alias used by the existing graph and history code.
type Response = ModelResponse

type ResponseError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

type ResponseUsage struct {
	InputTokens         int64              `json:"input_tokens"`
	InputTokensDetails  TokenInputDetails  `json:"input_tokens_details"`
	OutputTokens        int64              `json:"output_tokens"`
	OutputTokensDetails TokenOutputDetails `json:"output_tokens_details"`
	TotalTokens         int64              `json:"total_tokens"`
}

type TokenInputDetails struct {
	CachedTokens        int64 `json:"cached_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
}

type TokenOutputDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// ResponseOutputItem is implemented by the documented Responses output item
// types below.
type ResponseOutputItem interface{ isResponseOutputItem() }

type OutputMessage struct {
	ID      string              `json:"id,omitempty"`
	Status  string              `json:"status,omitempty"`
	Role    Role                `json:"role,omitempty"`
	Content []OutputContentPart `json:"content,omitempty"`
}

func (OutputMessage) isResponseOutputItem() {}

func (v OutputMessage) MarshalJSON() ([]byte, error) {
	type plain OutputMessage
	return marshalTagged("message", plain(v))
}

func (v *OutputMessage) UnmarshalJSON(data []byte) error {
	type outputMessageAlias OutputMessage
	var wire struct {
		*outputMessageAlias
		Content []json.RawMessage `json:"content"`
	}
	wire.outputMessageAlias = (*outputMessageAlias)(v)
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Content != nil {
		v.Content = make([]OutputContentPart, 0, len(wire.Content))
		for _, raw := range wire.Content {
			part, err := decodeOutputContentPart(raw)
			if err != nil {
				return err
			}
			v.Content = append(v.Content, part)
		}
	}
	return nil
}

type OutputReasoning struct {
	ID               string                 `json:"id,omitempty"`
	Status           string                 `json:"status,omitempty"`
	Content          []ReasoningText        `json:"content,omitempty"`
	Summary          []ReasoningSummaryText `json:"summary,omitempty"`
	EncryptedContent string                 `json:"encrypted_content,omitempty"`
	Signature        string                 `json:"signature,omitempty"`
}

func (OutputReasoning) isResponseOutputItem() {}

func (v OutputReasoning) MarshalJSON() ([]byte, error) {
	type plain OutputReasoning
	return marshalTagged("reasoning", plain(v))
}

type ReasoningSummaryText struct {
	Text string `json:"text"`
}

func (v ReasoningSummaryText) MarshalJSON() ([]byte, error) {
	type plain ReasoningSummaryText
	return marshalTagged("summary_text", plain(v))
}

type OutputFunctionCall struct {
	ID        string `json:"id,omitempty"`
	Status    string `json:"status,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (OutputFunctionCall) isResponseOutputItem() {}

func (v OutputFunctionCall) MarshalJSON() ([]byte, error) {
	type plain OutputFunctionCall
	return marshalTagged("function_call", plain(v))
}

type OutputCustomToolCall struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
}

func (OutputCustomToolCall) isResponseOutputItem() {}

func (v OutputCustomToolCall) MarshalJSON() ([]byte, error) {
	type plain OutputCustomToolCall
	return marshalTagged("custom_tool_call", plain(v))
}

// OutputCompaction is opaque provider state returned by server-side
// compaction. It is carried forward unchanged and is not part of the visible
// conversation transcript.
type OutputCompaction struct {
	ID               string `json:"id,omitempty"`
	EncryptedContent string `json:"encrypted_content"`
	CreatedBy        string `json:"created_by,omitempty"`
}

func (OutputCompaction) isResponseOutputItem() {}

func (v OutputCompaction) MarshalJSON() ([]byte, error) {
	type plain OutputCompaction
	return marshalTagged("compaction", plain(v))
}

type UnknownOutputItem struct{ Raw json.RawMessage }

func (UnknownOutputItem) isResponseOutputItem() {}

func (v UnknownOutputItem) MarshalJSON() ([]byte, error) { return rawJSON(v.Raw), nil }

func decodeResponseOutputItem(data []byte) (ResponseOutputItem, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("decode output item header: %w", err)
	}
	switch header.Type {
	case "message":
		var item OutputMessage
		return item, json.Unmarshal(data, &item)
	case "reasoning":
		var item OutputReasoning
		return item, json.Unmarshal(data, &item)
	case "function_call":
		var item OutputFunctionCall
		return item, json.Unmarshal(data, &item)
	case "custom_tool_call":
		var item OutputCustomToolCall
		return item, json.Unmarshal(data, &item)
	case "compaction":
		var item OutputCompaction
		return item, json.Unmarshal(data, &item)
	default:
		return UnknownOutputItem{Raw: cloneJSON(data)}, nil
	}
}

func decodeOutputContentPart(data []byte) (OutputContentPart, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("decode output content part header: %w", err)
	}
	if header.Type == "output_text" {
		var part OutputText
		return part, json.Unmarshal(data, &part)
	}
	return UnknownOutputContentPart{Raw: cloneJSON(data)}, nil
}
