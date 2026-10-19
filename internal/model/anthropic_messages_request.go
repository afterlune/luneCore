package model

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"strings"
)

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int64              `json:"max_tokens"`
	System      any                `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []map[string]any   `json:"tools,omitempty"`
	ToolChoice  map[string]any     `json:"tool_choice,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Output      map[string]any     `json:"output_config,omitempty"`
	Metadata    map[string]string  `json:"metadata,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

func toAnthropicRequest(request ModelRequest, profile ModelProfile, stream bool) (anthropicRequest, error) {
	modelID := resolveModelID(request.Model, profile)
	maxTokens := profile.MaxOutputTokens
	if request.MaxOutputTokens != nil {
		maxTokens = *request.MaxOutputTokens
	}
	if maxTokens <= 0 {
		return anthropicRequest{}, errors.New("Anthropic Messages requires a positive max_output_tokens value")
	}
	wire := anthropicRequest{
		Model: modelID, MaxTokens: maxTokens, Temperature: request.Temperature,
		TopP: request.TopP, Stream: stream,
	}
	var system []map[string]any
	if strings.TrimSpace(request.Instructions) != "" {
		system = append(system, map[string]any{"type": "text", "text": request.Instructions})
	}
	if request.Input != nil {
		if request.Input.Text != nil {
			wire.Messages = append(wire.Messages, anthropicMessage{Role: "user", Content: []map[string]any{{"type": "text", "text": *request.Input.Text}}})
			if request.PromptCache != nil && request.PromptCache.PrefixItems > 0 {
				markAnthropicCacheBreakpoint(&wire, &system)
			}
		} else {
			for index, item := range request.Input.Items {
				beforeSystem, beforeMessages, beforeBlocks := len(system), len(wire.Messages), 0
				if beforeMessages > 0 {
					beforeBlocks = len(wire.Messages[beforeMessages-1].Content)
				}
				if err := appendAnthropicInput(&wire, &system, item); err != nil {
					return anthropicRequest{}, err
				}
				if request.PromptCache != nil && index+1 == request.PromptCache.PrefixItems {
					markAnthropicCacheBreakpointAfterInput(&wire, &system, beforeSystem, beforeMessages, beforeBlocks)
				}
			}
		}
	}
	if len(system) > 0 {
		wire.System = system
	}
	for _, definition := range request.Tools {
		function, ok := definition.(FunctionTool)
		if !ok {
			return anthropicRequest{}, fmt.Errorf("Anthropic Messages does not support tool definition %T", definition)
		}
		schema := json.RawMessage(function.Parameters)
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tool := map[string]any{"name": function.Name, "input_schema": schema}
		if function.Description != "" {
			tool["description"] = function.Description
		}
		if function.Strict != nil {
			tool["strict"] = *function.Strict
		}
		wire.Tools = append(wire.Tools, tool)
	}
	if request.ToolChoice != nil {
		choice, err := anthropicToolChoice(*request.ToolChoice)
		if err != nil {
			return anthropicRequest{}, err
		}
		wire.ToolChoice = choice
	}
	if request.Text != nil {
		format := request.Text.Format
		schema := json.RawMessage(format.Schema)
		switch format.Type {
		case TextFormatText:
		case TextFormatJSONObject:
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","additionalProperties":true}`)
			}
			wire.Output = map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema}}
		case TextFormatJSONSchema:
			if len(schema) == 0 {
				return anthropicRequest{}, errors.New("Anthropic JSON schema output requires a schema")
			}
			wire.Output = map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema}}
		default:
			return anthropicRequest{}, fmt.Errorf("unsupported Anthropic text format %q", format.Type)
		}
	}
	if request.Reasoning != nil && request.Reasoning.Effort != "" {
		if wire.Output == nil {
			wire.Output = make(map[string]any)
		}
		wire.Output["effort"] = request.Reasoning.Effort
	}
	if request.TopLogprobs != nil {
		return anthropicRequest{}, errors.New("Anthropic Messages does not support top_logprobs")
	}
	if request.User != "" {
		wire.Metadata = map[string]string{"user_id": request.User}
	}
	if len(wire.Messages) == 0 && wire.System == nil {
		return anthropicRequest{}, errors.New("Anthropic Messages requires system instructions or at least one message")
	}
	return wire, nil
}

func markAnthropicCacheBreakpointAfterInput(wire *anthropicRequest, system *[]map[string]any, beforeSystem, beforeMessages, beforeBlocks int) {
	if len(*system) > beforeSystem {
		markAnthropicCacheBlock((*system)[len(*system)-1])
		return
	}
	if len(wire.Messages) > beforeMessages {
		content := wire.Messages[len(wire.Messages)-1].Content
		if len(content) > 0 {
			markAnthropicCacheBlock(content[len(content)-1])
		}
		return
	}
	if len(wire.Messages) > 0 {
		content := wire.Messages[len(wire.Messages)-1].Content
		if len(content) > beforeBlocks {
			markAnthropicCacheBlock(content[len(content)-1])
		}
	}
}

func markAnthropicCacheBreakpoint(wire *anthropicRequest, system *[]map[string]any) {
	if len(*system) > 0 {
		markAnthropicCacheBlock((*system)[len(*system)-1])
		return
	}
	if len(wire.Messages) > 0 && len(wire.Messages[len(wire.Messages)-1].Content) > 0 {
		content := wire.Messages[len(wire.Messages)-1].Content
		markAnthropicCacheBlock(content[len(content)-1])
	}
}

func markAnthropicCacheBlock(block map[string]any) {
	block["cache_control"] = map[string]string{"type": "ephemeral"}
}

func anthropicToolChoice(choice ToolChoice) (map[string]any, error) {
	if choice.Mode != nil {
		switch *choice.Mode {
		case "auto":
			return map[string]any{"type": "auto"}, nil
		case "none":
			return map[string]any{"type": "none"}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		default:
			return nil, fmt.Errorf("unsupported Anthropic tool choice %q", *choice.Mode)
		}
	}
	if choice.Named != nil {
		return map[string]any{"type": "tool", "name": choice.Named.Name}, nil
	}
	if len(choice.Raw) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(choice.Raw, &raw); err != nil {
			return nil, fmt.Errorf("decode Anthropic tool choice: %w", err)
		}
		return raw, nil
	}
	return nil, nil
}

func appendAnthropicInput(wire *anthropicRequest, system *[]map[string]any, item InputItem) error {
	switch value := item.(type) {
	case InputMessage:
		role := string(value.Role)
		content, err := anthropicContent(value.Content.Parts, value.Content.Text)
		if err != nil {
			return err
		}
		if role == string(RoleSystem) || role == string(RoleDeveloper) {
			*system = append(*system, content...)
			return nil
		}
		if role != string(RoleUser) && role != string(RoleAssistant) {
			return fmt.Errorf("unsupported Anthropic message role %q", role)
		}
		appendAnthropicMessage(wire, role, content)
	case InputFunctionCall:
		var input any
		if err := json.Unmarshal([]byte(value.Arguments), &input); err != nil {
			return fmt.Errorf("encode Anthropic tool input %q: %w", value.Name, err)
		}
		appendAnthropicMessage(wire, "assistant", []map[string]any{{"type": "tool_use", "id": value.CallID, "name": value.Name, "input": input}})
	case InputFunctionCallOutput:
		content, err := anthropicToolResultContent(value.Output)
		if err != nil {
			return err
		}
		appendAnthropicMessage(wire, "user", []map[string]any{{"type": "tool_result", "tool_use_id": value.CallID, "content": content}})
	case InputReasoning:
		block := map[string]any{"type": "thinking"}
		var text strings.Builder
		for _, part := range value.Content {
			text.WriteString(part.Text)
		}
		block["thinking"] = text.String()
		if value.Signature != "" {
			block["signature"] = value.Signature
		}
		appendAnthropicMessage(wire, "assistant", []map[string]any{block})
	case UnknownInputItem:
		item, err := decodeInputItem(value.Raw)
		if err != nil {
			return fmt.Errorf("decode Responses history item for Anthropic Messages: %w", err)
		}
		if _, unknown := item.(UnknownInputItem); unknown {
			return unsupportedAnthropicItem(value.Raw)
		}
		return appendAnthropicInput(wire, system, item)
	case InputCustomToolCall, InputCustomToolCallOutput:
		return fmt.Errorf("Anthropic Messages does not support custom tool history item %T", item)
	default:
		return fmt.Errorf("Anthropic Messages does not support input item %T", item)
	}
	return nil
}

func appendAnthropicMessage(wire *anthropicRequest, role string, content []map[string]any) {
	if len(wire.Messages) > 0 && wire.Messages[len(wire.Messages)-1].Role == role {
		wire.Messages[len(wire.Messages)-1].Content = append(wire.Messages[len(wire.Messages)-1].Content, content...)
		return
	}
	wire.Messages = append(wire.Messages, anthropicMessage{Role: role, Content: content})
}

func anthropicContent(parts []ContentPart, text *string) ([]map[string]any, error) {
	if text != nil {
		return []map[string]any{{"type": "text", "text": *text}}, nil
	}
	var content []map[string]any
	for _, part := range parts {
		switch value := part.(type) {
		case InputText:
			content = append(content, map[string]any{"type": "text", "text": value.Text})
		case OutputText:
			content = append(content, map[string]any{"type": "text", "text": value.Text})
		case InputImage:
			image, err := anthropicImage(value)
			if err != nil {
				return nil, err
			}
			content = append(content, image)
		case ReasoningText:
			content = append(content, map[string]any{"type": "text", "text": value.Text})
		case UnknownContentPart:
			var raw map[string]any
			if err := json.Unmarshal(value.Raw, &raw); err != nil {
				return nil, fmt.Errorf("decode unknown content part: %w", err)
			}
			typeName, _ := raw["type"].(string)
			if typeName == "image" {
				content = append(content, raw)
			} else {
				return nil, fmt.Errorf("Anthropic Messages cannot map content part %q", typeName)
			}
		default:
			return nil, fmt.Errorf("Anthropic Messages does not support content part %T", part)
		}
	}
	return content, nil
}

func anthropicImage(image InputImage) (map[string]any, error) {
	if image.FileID != "" {
		return nil, errors.New("Anthropic image file IDs require the Files API, which is not configured")
	}
	if image.ImageURL == "" {
		return nil, errors.New("Anthropic image input requires image_url")
	}
	const dataScheme, base64Suffix = "data:", ";base64"
	if len(image.ImageURL) >= len(dataScheme) && strings.EqualFold(image.ImageURL[:len(dataScheme)], dataScheme) {
		metadata, payload, ok := strings.Cut(image.ImageURL, ",")
		if !ok || len(metadata) < len(dataScheme) || !strings.EqualFold(metadata[:len(dataScheme)], dataScheme) {
			return nil, errors.New("invalid image data URL")
		}
		if len(metadata) < len(base64Suffix) || !strings.EqualFold(metadata[len(metadata)-len(base64Suffix):], base64Suffix) {
			return nil, errors.New("Anthropic image data URLs must use base64 encoding")
		}
		if len(metadata) <= len(dataScheme)+len(base64Suffix) {
			return nil, errors.New("invalid base64 image data URL")
		}
		mediaHeader := metadata[len(dataScheme) : len(metadata)-len(base64Suffix)]
		mediaType, _, err := mime.ParseMediaType(mediaHeader)
		if err != nil || len(mediaType) < len("image/") || !strings.EqualFold(mediaType[:len("image/")], "image/") {
			return nil, errors.New("invalid base64 image data URL")
		}
		decoded, err := url.PathUnescape(payload)
		if err != nil {
			return nil, errors.New("invalid image data URL payload")
		}
		if _, err := base64.StdEncoding.DecodeString(decoded); err != nil {
			return nil, errors.New("image data URL must contain base64 data")
		}
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": mediaType, "data": decoded,
		}}, nil
	}
	parsed, err := url.Parse(image.ImageURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, errors.New("Anthropic image URL must be an http or https URL")
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": image.ImageURL}}, nil
}

func anthropicToolResultContent(output ToolOutput) (any, error) {
	if output.Text != nil {
		return *output.Text, nil
	}
	content, err := anthropicContent(output.Parts, nil)
	if err != nil {
		return nil, err
	}
	return content, nil
}

func unsupportedAnthropicItem(raw json.RawMessage) error {
	var header struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &header)
	return fmt.Errorf("Anthropic Messages cannot map input item %q", header.Type)
}
