package model

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const (
	anthropicAPIVersion = "2023-06-01"
)

type anthropicMessagesProvider struct {
	providerID string
	apiKey     string
	endpoint   string
	httpClient *http.Client
}

func newAnthropicMessagesProvider(providerID, apiKey, endpoint string, httpClient *http.Client) *anthropicMessagesProvider {
	return &anthropicMessagesProvider{providerID: providerID, apiKey: apiKey, endpoint: endpoint, httpClient: httpClient}
}

func (p *anthropicMessagesProvider) Name() string { return p.providerID }

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

func (p *anthropicMessagesProvider) CreateResponse(ctx context.Context, request ModelRequest, profile ModelProfile) (*ModelResponse, error) {
	wire, err := toAnthropicRequest(request, profile, false)
	if err != nil {
		return nil, err
	}
	response, err := postJSON(ctx, p.httpClient, p.endpoint, anthropicHeaders(p.apiKey, "application/json"), wire)
	if err != nil {
		return nil, fmt.Errorf("call %s Messages API: %w", p.providerID, err)
	}
	body, err := responseBody(response)
	if err != nil {
		return nil, err
	}
	if err := httpStatusError(response, body, p.providerID+" Messages API"); err != nil {
		return nil, err
	}
	result, err := decodeAnthropicResponse(body)
	if err != nil {
		return nil, fmt.Errorf("decode %s Messages response: %w", p.providerID, err)
	}
	return result, nil
}

func (p *anthropicMessagesProvider) StreamResponse(ctx context.Context, request ModelRequest, profile ModelProfile, emit func(ModelStreamEvent) error) (*ModelResponse, error) {
	wire, err := toAnthropicRequest(request, profile, true)
	if err != nil {
		return nil, err
	}
	response, err := postJSON(ctx, p.httpClient, p.endpoint, anthropicHeaders(p.apiKey, "text/event-stream"), wire)
	if err != nil {
		return nil, fmt.Errorf("call %s Messages stream: %w", p.providerID, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := responseBody(response)
		if readErr != nil {
			return nil, readErr
		}
		return nil, httpStatusError(response, body, p.providerID+" Messages API")
	}
	defer response.Body.Close()
	return readAnthropicStream(ctx, response.Body, emit, wire.Model, p.providerID)
}

func anthropicHeaders(apiKey, accept string) map[string]string {
	return map[string]string{
		"x-api-key":         apiKey,
		"anthropic-version": anthropicAPIVersion,
		"Accept":            accept,
	}
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
		} else {
			for _, item := range request.Input.Items {
				if err := appendAnthropicInput(&wire, &system, item); err != nil {
					return anthropicRequest{}, err
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

func decodeAnthropicResponse(data []byte) (*ModelResponse, error) {
	var wire struct {
		ID         string            `json:"id"`
		Type       string            `json:"type"`
		Role       string            `json:"role"`
		Model      string            `json:"model"`
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	result := &ModelResponse{
		ID: wire.ID, Object: "response", Model: wire.Model, Status: "completed",
		Usage: ResponseUsage{
			InputTokens: wire.Usage.InputTokens,
			InputTokensDetails: TokenInputDetails{
				CachedTokens: wire.Usage.CacheReadInputTokens, CacheCreationTokens: wire.Usage.CacheCreationInputTokens,
			},
			OutputTokens: wire.Usage.OutputTokens, TotalTokens: wire.Usage.InputTokens + wire.Usage.OutputTokens,
		},
	}
	if wire.StopReason == "max_tokens" {
		result.Status = "incomplete"
		result.IncompleteDetails = &IncompleteDetails{Reason: "max_output_tokens"}
	}
	for _, raw := range wire.Content {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, err
		}
		switch header.Type {
		case "text":
			var block struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &block); err != nil {
				return nil, err
			}
			result.Output = append(result.Output, OutputMessage{Role: RoleAssistant, Content: []OutputContentPart{OutputText{Text: block.Text}}})
		case "tool_use":
			var block struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(raw, &block); err != nil {
				return nil, err
			}
			result.Output = append(result.Output, OutputFunctionCall{CallID: block.ID, Name: block.Name, Arguments: string(block.Input)})
		case "thinking":
			var block struct {
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			}
			if err := json.Unmarshal(raw, &block); err != nil {
				return nil, err
			}
			result.Output = append(result.Output, OutputReasoning{Content: []ReasoningText{{Text: block.Thinking}}, Signature: block.Signature})
		default:
			result.Output = append(result.Output, UnknownOutputItem{Raw: cloneJSON(raw)})
		}
	}
	return result, nil
}

type anthropicStreamBlock struct {
	typeName  string
	id        string
	name      string
	text      strings.Builder
	arguments strings.Builder
	signature strings.Builder
}

func readAnthropicStream(ctx context.Context, body interface{ Read([]byte) (int, error) }, emit func(ModelStreamEvent) error, fallbackModel, providerID string) (*ModelResponse, error) {
	response := &ModelResponse{Object: "response", Status: "in_progress", Model: fallbackModel}
	blocks := make(map[int]*anthropicStreamBlock)
	var stopReason string
	push := func(event ModelStreamEvent) error {
		if emit != nil {
			return emit(event)
		}
		return nil
	}
	err := scanSSE(ctx, body, func(eventName, data string) error {
		if eventName == "error" {
			return fmt.Errorf("%s Messages stream error: %s", providerID, data)
		}
		var event struct {
			Type         string          `json:"type"`
			Message      json.RawMessage `json:"message"`
			Index        int             `json:"index"`
			ContentBlock json.RawMessage `json:"content_block"`
			Delta        json.RawMessage `json:"delta"`
			Usage        json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("decode Anthropic stream event: %w", err)
		}
		if event.Type == "error" {
			return fmt.Errorf("%s Messages stream error: %s", providerID, data)
		}
		switch event.Type {
		case "message_start":
			var start struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens              int64 `json:"input_tokens"`
					CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			}
			_ = json.Unmarshal(event.Message, &start)
			response.ID = start.ID
			if start.Model != "" {
				response.Model = start.Model
			}
			response.Usage.InputTokens = start.Usage.InputTokens
			response.Usage.InputTokensDetails = TokenInputDetails{
				CachedTokens: start.Usage.CacheReadInputTokens, CacheCreationTokens: start.Usage.CacheCreationInputTokens,
			}
		case "content_block_start":
			var block struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Text  string          `json:"text"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(event.ContentBlock, &block); err != nil {
				return err
			}
			state := &anthropicStreamBlock{typeName: block.Type, id: block.ID, name: block.Name}
			state.text.WriteString(block.Text)
			if len(block.Input) > 0 && string(block.Input) != "{}" {
				state.arguments.Write(block.Input)
			}
			blocks[event.Index] = state
			if block.Type == "tool_use" {
				return push(ModelStreamEvent{Type: ModelStreamToolStarted, OutputIndex: intPointer(event.Index), ItemID: block.ID,
					ToolCall: &OutputFunctionCall{CallID: block.ID, Name: block.Name, Arguments: string(block.Input)}})
			}
			return nil
		case "content_block_delta":
			block := blocks[event.Index]
			if block == nil {
				return fmt.Errorf("Anthropic stream delta has no block at index %d", event.Index)
			}
			var delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
			}
			if err := json.Unmarshal(event.Delta, &delta); err != nil {
				return err
			}
			switch delta.Type {
			case "text_delta":
				block.text.WriteString(delta.Text)
				return push(ModelStreamEvent{Type: ModelStreamTextDelta, OutputIndex: intPointer(event.Index), ItemID: block.id, Delta: delta.Text})
			case "input_json_delta":
				block.arguments.WriteString(delta.PartialJSON)
				return push(ModelStreamEvent{Type: ModelStreamToolDelta, OutputIndex: intPointer(event.Index), ItemID: block.id, Delta: delta.PartialJSON})
			case "thinking_delta":
				block.text.WriteString(delta.Thinking)
				return push(ModelStreamEvent{Type: ModelStreamReasoningDelta, OutputIndex: intPointer(event.Index), ItemID: block.id, Delta: delta.Thinking})
			case "signature_delta":
				block.signature.WriteString(delta.Signature)
			}
		case "content_block_stop":
			block := blocks[event.Index]
			if block == nil {
				return nil
			}
			switch block.typeName {
			case "text":
				response.Output = append(response.Output, OutputMessage{Role: RoleAssistant, Content: []OutputContentPart{OutputText{Text: block.text.String()}}})
			case "tool_use":
				args := block.arguments.String()
				if args == "" {
					args = "{}"
				}
				response.Output = append(response.Output, OutputFunctionCall{CallID: block.id, Name: block.name, Arguments: args})
			case "thinking":
				response.Output = append(response.Output, OutputReasoning{Content: []ReasoningText{{Text: block.text.String()}}, Signature: block.signature.String()})
			}
		case "message_delta":
			var delta struct {
				StopReason string `json:"stop_reason"`
				Usage      struct {
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			_ = json.Unmarshal(event.Delta, &delta)
			stopReason = delta.StopReason
			response.Usage.OutputTokens = delta.Usage.OutputTokens
			response.Usage.TotalTokens = response.Usage.InputTokens + response.Usage.OutputTokens
		case "message_stop":
			if stopReason == "max_tokens" {
				response.Status = "incomplete"
				response.IncompleteDetails = &IncompleteDetails{Reason: "max_output_tokens"}
			} else {
				response.Status = "completed"
			}
			return push(ModelStreamEvent{Type: ModelStreamCompleted, Response: response})
		case "ping":
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if response.Status == "in_progress" {
		return nil, fmt.Errorf("%s stream ended before message_stop", providerID)
	}
	return response, nil
}

func intPointer(value int) *int { return &value }

var _ Provider = (*anthropicMessagesProvider)(nil)
