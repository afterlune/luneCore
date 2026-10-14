package model

import (
	"encoding/json"
	"fmt"
)

// ResponseTool is implemented by function and custom tool definitions.
type ResponseTool interface{ isResponseTool() }

type FunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

func (FunctionTool) isResponseTool() {}

func (v FunctionTool) MarshalJSON() ([]byte, error) {
	type plain FunctionTool
	return marshalTagged("function", plain(v))
}

type CustomTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (CustomTool) isResponseTool() {}

func (v CustomTool) MarshalJSON() ([]byte, error) {
	type plain CustomTool
	return marshalTagged("custom", plain(v))
}

type UnknownResponseTool struct{ Raw json.RawMessage }

func (UnknownResponseTool) isResponseTool() {}

func (v UnknownResponseTool) MarshalJSON() ([]byte, error) { return rawJSON(v.Raw), nil }

func decodeResponseTool(data []byte) (ResponseTool, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("decode tool header: %w", err)
	}
	switch header.Type {
	case "function":
		var tool FunctionTool
		return tool, json.Unmarshal(data, &tool)
	case "custom":
		var tool CustomTool
		return tool, json.Unmarshal(data, &tool)
	default:
		return UnknownResponseTool{Raw: cloneJSON(data)}, nil
	}
}
