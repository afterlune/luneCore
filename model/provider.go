// Package model defines the provider-neutral model API used by Lune Core
// hosts that supply their own model implementations.
package model

import (
	"context"
	"encoding/json"
)

// Provider sends one model request. The runtime owns conversation history and
// tool execution; implementations translate this shared contract to a model
// service's native request and response format.
//
// Implementations must be safe for concurrent calls from separate sessions
// and should return promptly when ctx is canceled.
type Provider interface {
	Name() string
	CreateResponse(context.Context, ModelRequest, ModelProfile) (*ModelResponse, error)
	StreamResponse(context.Context, ModelRequest, ModelProfile, func(ModelStreamEvent) error) (*ModelResponse, error)
}

// ModelStreamEventType identifies a normalized provider streaming event.
type ModelStreamEventType string

const (
	ModelStreamTextDelta      ModelStreamEventType = "text_delta"
	ModelStreamReasoningDelta ModelStreamEventType = "reasoning_delta"
	ModelStreamToolStarted    ModelStreamEventType = "tool_started"
	ModelStreamToolDelta      ModelStreamEventType = "tool_arguments_delta"
	ModelStreamCompleted      ModelStreamEventType = "completed"
	ModelStreamFailed         ModelStreamEventType = "failed"
)

// ModelStreamEvent contains provider-neutral incremental output.
type ModelStreamEvent struct {
	RequestID   string               `json:"request_id,omitempty"`
	Purpose     string               `json:"purpose,omitempty"`
	Type        ModelStreamEventType `json:"type,omitempty"`
	ItemID      string               `json:"item_id,omitempty"`
	OutputIndex *int                 `json:"output_index,omitempty"`
	Delta       string               `json:"delta,omitempty"`
	ToolCall    *OutputFunctionCall  `json:"tool_call,omitempty"`
	Response    *ModelResponse       `json:"response,omitempty"`
	Error       *ResponseError       `json:"error,omitempty"`
	Raw         json.RawMessage      `json:"raw,omitempty"`
}

// UserQuestionOption is one suggested answer and its explanation.
type UserQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
