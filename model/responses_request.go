package model

import (
	"bytes"
	"encoding/json"
	"errors"
)

type ModelRequest struct {
	Model             string                  `json:"model"`
	Input             *ResponseInput          `json:"input,omitempty"`
	Instructions      string                  `json:"instructions,omitempty"`
	ContextManagement []ContextManagementItem `json:"context_management,omitempty"`
	Reasoning         *ReasoningConfig        `json:"reasoning,omitempty"`
	MaxOutputTokens   *int64                  `json:"max_output_tokens,omitempty"`
	Stream            bool                    `json:"stream,omitempty"`
	Temperature       *float64                `json:"temperature,omitempty"`
	TopP              *float64                `json:"top_p,omitempty"`
	TopLogprobs       *int                    `json:"top_logprobs,omitempty"`
	Text              *ResponseText           `json:"text,omitempty"`
	Tools             []ResponseTool          `json:"tools,omitempty"`
	ToolChoice        *ToolChoice             `json:"tool_choice,omitempty"`
	User              string                  `json:"user,omitempty"`
	// PromptCache is an advisory hint for provider adapters. It is not part of
	// the wire request because cache controls differ between model protocols.
	PromptCache *PromptCacheHint `json:"-"`
}

// PromptCacheHint describes a stable input prefix that sibling requests may
// reuse. Key is a provider-specific routing hint; PrefixItems identifies the
// final shared item for protocols that require an explicit cache breakpoint.
type PromptCacheHint struct {
	Key         string
	PrefixItems int
}

// ContextManagementItem configures server-side Responses API compaction.
type ContextManagementItem struct {
	Type             string `json:"type"`
	CompactThreshold int64  `json:"compact_threshold"`
}

// UnmarshalJSON decodes tools as a tagged union while preserving unknown tool
// definitions for callers that need to inspect or forward them.
func (r *ModelRequest) UnmarshalJSON(data []byte) error {
	type requestAlias ModelRequest
	var wire struct {
		*requestAlias
		Tools []json.RawMessage `json:"tools"`
	}
	wire.requestAlias = (*requestAlias)(r)
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Tools != nil {
		r.Tools = make([]ResponseTool, 0, len(wire.Tools))
		for _, raw := range wire.Tools {
			tool, err := decodeResponseTool(raw)
			if err != nil {
				return err
			}
			r.Tools = append(r.Tools, tool)
		}
	}
	return nil
}

// ResponseRequest remains an internal source alias for existing callers. Wire
// providers use their own request type.
type ResponseRequest = ModelRequest

// ResponseInput represents either a plain text input or a list of input items.
// Set exactly one of Text and Items. A nil pointer on ResponseRequest omits the
// input property; a zero value ResponseInput encodes as JSON null.
type ReasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}

const (
	ReasoningEffortNone    = "none"
	ReasoningEffortMinimal = "minimal"
	ReasoningEffortLow     = "low"
	ReasoningEffortMedium  = "medium"
	ReasoningEffortHigh    = "high"
	ReasoningEffortXHigh   = "xhigh"
	ReasoningEffortMax     = "max"
)

type ResponseText struct {
	Format ResponseTextFormat `json:"format"`
}

type ResponseTextFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
}

const (
	TextFormatText       = "text"
	TextFormatJSONObject = "json_object"
	TextFormatJSONSchema = "json_schema"
)

// ToolChoice represents either a mode such as "auto" or a named function.
type ToolChoice struct {
	Mode  *string
	Named *NamedToolChoice
	Raw   json.RawMessage
}

type NamedToolChoice struct {
	Name string `json:"name"`
}

func (v NamedToolChoice) MarshalJSON() ([]byte, error) {
	type plain NamedToolChoice
	return marshalTagged("function", plain(v))
}

func (v ToolChoice) MarshalJSON() ([]byte, error) {
	branches := 0
	if v.Mode != nil {
		branches++
	}
	if v.Named != nil {
		branches++
	}
	if len(v.Raw) != 0 {
		branches++
	}
	if branches > 1 {
		return nil, errors.New("tool choice must select exactly one representation")
	}
	switch {
	case v.Mode != nil:
		return json.Marshal(*v.Mode)
	case v.Named != nil:
		return json.Marshal(v.Named)
	case len(v.Raw) != 0:
		return rawJSON(v.Raw), nil
	default:
		return []byte("null"), nil
	}
}

func (v *ToolChoice) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*v = ToolChoice{}
		return nil
	}
	if data[0] == '"' {
		var mode string
		if err := json.Unmarshal(data, &mode); err != nil {
			return err
		}
		*v = ToolChoice{Mode: &mode}
		return nil
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	if header.Type == "function" {
		var named NamedToolChoice
		if err := json.Unmarshal(data, &named); err != nil {
			return err
		}
		*v = ToolChoice{Named: &named}
	} else {
		*v = ToolChoice{Raw: cloneJSON(data)}
	}
	return nil
}
