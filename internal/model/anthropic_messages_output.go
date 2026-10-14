package model

import "encoding/json"

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
