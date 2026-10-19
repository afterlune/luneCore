package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

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
			return providerPayloadError(0, []byte(data))
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
			return providerPayloadError(0, []byte(data))
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
			inputTokens := start.Usage.InputTokens + start.Usage.CacheReadInputTokens + start.Usage.CacheCreationInputTokens
			response.Usage.InputTokens = inputTokens
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
		return response, err
	}
	if response.Status == "in_progress" {
		return response, transportError(io.ErrUnexpectedEOF)
	}
	return response, nil
}

func intPointer(value int) *int { return &value }
