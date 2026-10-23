package model

import (
	"encoding/json"
	"errors"
	"fmt"
)

// CloneResponseInput copies the input while preserving typed and unknown
// multimodal items. Agent state cloning uses this instead of a JSON round trip.
func CloneResponseInput(input ResponseInput) (ResponseInput, error) {
	if input.Text != nil && input.Items != nil {
		return ResponseInput{}, errors.New("responses input cannot contain both text and items")
	}
	clone := ResponseInput{Items: cloneSlice(input.Items)}
	if input.Text != nil {
		text := *input.Text
		clone.Text = &text
	}
	for index, item := range input.Items {
		cloned, err := cloneInputItem(item)
		if err != nil {
			return ResponseInput{}, fmt.Errorf("clone input item %d: %w", index, err)
		}
		clone.Items[index] = cloned
	}
	return clone, nil
}

// CloneModelResponse deep-copies the provider response retained in agent state.
func CloneModelResponse(response *ModelResponse) (*ModelResponse, error) {
	if response == nil {
		return nil, nil
	}
	clone := *response
	if response.Error != nil {
		errorValue := *response.Error
		clone.Error = &errorValue
	}
	if response.IncompleteDetails != nil {
		details := *response.IncompleteDetails
		clone.IncompleteDetails = &details
	}
	if response.PreviousResponseID != nil {
		id := *response.PreviousResponseID
		clone.PreviousResponseID = &id
	}
	if response.Output != nil {
		clone.Output = make([]ResponseOutputItem, len(response.Output))
		for index, item := range response.Output {
			cloned, err := cloneResponseOutputItem(item)
			if err != nil {
				return nil, fmt.Errorf("clone response output %d: %w", index, err)
			}
			clone.Output[index] = cloned
		}
	}
	return &clone, nil
}

func cloneInputItem(item InputItem) (InputItem, error) {
	switch item := item.(type) {
	case InputMessage:
		content, err := cloneMessageContent(item.Content)
		item.Content = content
		return item, err
	case *InputMessage:
		if item == nil {
			return UnknownInputItem{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneInputItem(*item)
		if err != nil {
			return nil, err
		}
		value := cloned.(InputMessage)
		return &value, nil
	case *InputFunctionCall:
		if item == nil {
			return UnknownInputItem{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(item), nil
	case *InputCustomToolCall:
		if item == nil {
			return UnknownInputItem{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(item), nil
	case InputFunctionCallOutput:
		output, err := cloneToolOutput(item.Output)
		if err != nil {
			return nil, err
		}
		item.Output = output
		item.TranscriptOnly, err = cloneContentParts(item.TranscriptOnly)
		return item, err
	case *InputFunctionCallOutput:
		if item == nil {
			return UnknownInputItem{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneInputItem(*item)
		if err != nil {
			return nil, err
		}
		value := cloned.(InputFunctionCallOutput)
		return &value, nil
	case InputCustomToolCallOutput:
		output, err := cloneToolOutput(item.Output)
		item.Output = output
		return item, err
	case *InputCustomToolCallOutput:
		if item == nil {
			return UnknownInputItem{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneInputItem(*item)
		if err != nil {
			return nil, err
		}
		value := cloned.(InputCustomToolCallOutput)
		return &value, nil
	case InputReasoning:
		item.Content = cloneSlice(item.Content)
		return item, nil
	case *InputReasoning:
		if item == nil {
			return UnknownInputItem{Raw: json.RawMessage("null")}, nil
		}
		value := *item
		value.Content = cloneSlice(item.Content)
		return &value, nil
	case UnknownInputItem:
		raw, err := cloneRawMessage(item.Raw)
		item.Raw = raw
		return item, err
	case *UnknownInputItem:
		if item == nil {
			return UnknownInputItem{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneInputItem(*item)
		if err != nil {
			return nil, err
		}
		value := cloned.(UnknownInputItem)
		return &value, nil
	case InputFunctionCall, InputCustomToolCall:
		return item, nil
	default:
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		return decodeInputItem(encoded)
	}
}

func cloneMessageContent(content MessageContent) (MessageContent, error) {
	if content.Text != nil && content.Parts != nil {
		return MessageContent{}, errors.New("message content cannot contain both text and parts")
	}
	clone := MessageContent{Parts: make([]ContentPart, len(content.Parts))}
	if content.Parts == nil {
		clone.Parts = nil
	}
	if content.Text != nil {
		text := *content.Text
		clone.Text = &text
	}
	for index, part := range content.Parts {
		cloned, err := cloneContentPart(part)
		if err != nil {
			return MessageContent{}, fmt.Errorf("clone content part %d: %w", index, err)
		}
		clone.Parts[index] = cloned
	}
	return clone, nil
}

func cloneToolOutput(output ToolOutput) (ToolOutput, error) {
	if output.Text != nil && output.Parts != nil {
		return ToolOutput{}, errors.New("tool output cannot contain both text and parts")
	}
	clone := ToolOutput{Parts: make([]ContentPart, len(output.Parts))}
	if output.Parts == nil {
		clone.Parts = nil
	}
	if output.Text != nil {
		text := *output.Text
		clone.Text = &text
	}
	for index, part := range output.Parts {
		cloned, err := cloneContentPart(part)
		if err != nil {
			return ToolOutput{}, fmt.Errorf("clone tool output part %d: %w", index, err)
		}
		clone.Parts[index] = cloned
	}
	return clone, nil
}

func cloneContentParts(parts []ContentPart) ([]ContentPart, error) {
	if parts == nil {
		return nil, nil
	}
	cloned := make([]ContentPart, len(parts))
	for index, part := range parts {
		copy, err := cloneContentPart(part)
		if err != nil {
			return nil, fmt.Errorf("clone content part %d: %w", index, err)
		}
		cloned[index] = copy
	}
	return cloned, nil
}

func cloneContentPart(part ContentPart) (ContentPart, error) {
	switch part := part.(type) {
	case OutputText:
		part.Annotations = cloneSlice(part.Annotations)
		for index, annotation := range part.Annotations {
			cloned, err := cloneRawMessage(annotation)
			if err != nil {
				return nil, fmt.Errorf("clone annotation %d: %w", index, err)
			}
			part.Annotations[index] = cloned
		}
		return part, nil
	case UnknownContentPart:
		raw, err := cloneRawMessage(part.Raw)
		part.Raw = raw
		return part, err
	case *UnknownContentPart:
		if part == nil {
			return UnknownContentPart{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneContentPart(*part)
		if err != nil {
			return nil, err
		}
		value := cloned.(UnknownContentPart)
		return &value, nil
	case InputText:
		return part, nil
	case *InputText:
		if part == nil {
			return UnknownContentPart{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(part), nil
	case InputImage:
		return part, nil
	case *InputImage:
		if part == nil {
			return UnknownContentPart{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(part), nil
	case InputAudio:
		return part, nil
	case *InputAudio:
		if part == nil {
			return UnknownContentPart{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(part), nil
	case ReasoningText:
		return part, nil
	case *ReasoningText:
		if part == nil {
			return UnknownContentPart{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(part), nil
	default:
		encoded, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		return decodeMessageContentPart(encoded)
	}
}

func cloneResponseOutputItem(item ResponseOutputItem) (ResponseOutputItem, error) {
	switch item := item.(type) {
	case OutputMessage:
		if item.Content != nil {
			content := make([]OutputContentPart, len(item.Content))
			for index, part := range item.Content {
				cloned, err := cloneOutputContentPart(part)
				if err != nil {
					return nil, fmt.Errorf("clone output content %d: %w", index, err)
				}
				content[index] = cloned
			}
			item.Content = content
		}
		return item, nil
	case OutputReasoning:
		item.Content = cloneSlice(item.Content)
		item.Summary = cloneSlice(item.Summary)
		return item, nil
	case UnknownOutputItem:
		raw, err := cloneRawMessage(item.Raw)
		item.Raw = raw
		return item, err
	case *OutputMessage:
		if item == nil {
			return UnknownOutputItem{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneResponseOutputItem(*item)
		if err != nil {
			return nil, err
		}
		value := cloned.(OutputMessage)
		return &value, nil
	case *OutputReasoning:
		if item == nil {
			return UnknownOutputItem{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneResponseOutputItem(*item)
		if err != nil {
			return nil, err
		}
		value := cloned.(OutputReasoning)
		return &value, nil
	case *UnknownOutputItem:
		if item == nil {
			return UnknownOutputItem{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneResponseOutputItem(*item)
		if err != nil {
			return nil, err
		}
		value := cloned.(UnknownOutputItem)
		return &value, nil
	case OutputFunctionCall, OutputCustomToolCall, OutputCompaction:
		return item, nil
	case *OutputFunctionCall:
		if item == nil {
			return UnknownOutputItem{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(item), nil
	case *OutputCustomToolCall:
		if item == nil {
			return UnknownOutputItem{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(item), nil
	case *OutputCompaction:
		if item == nil {
			return UnknownOutputItem{Raw: json.RawMessage("null")}, nil
		}
		return clonePointer(item), nil
	default:
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		return decodeModelResponseOutputItem(encoded)
	}
}

func cloneOutputContentPart(part OutputContentPart) (OutputContentPart, error) {
	switch part := part.(type) {
	case OutputText:
		cloned, err := cloneContentPart(part)
		if err != nil {
			return nil, err
		}
		return cloned.(OutputText), nil
	case UnknownOutputContentPart:
		raw, err := cloneRawMessage(part.Raw)
		part.Raw = raw
		return part, err
	case *OutputText:
		if part == nil {
			return UnknownOutputContentPart{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneContentPart(*part)
		if err != nil {
			return nil, err
		}
		value := cloned.(OutputText)
		return &value, nil
	case *UnknownOutputContentPart:
		if part == nil {
			return UnknownOutputContentPart{Raw: json.RawMessage("null")}, nil
		}
		cloned, err := cloneOutputContentPart(*part)
		if err != nil {
			return nil, err
		}
		value := cloned.(UnknownOutputContentPart)
		return &value, nil
	default:
		encoded, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		return decodeModelOutputContentPart(encoded)
	}
}

func decodeMessageContentPart(encoded []byte) (ContentPart, error) {
	wrapped := wrapJSONArrayItem(encoded)
	var content MessageContent
	if err := json.Unmarshal(wrapped, &content); err != nil {
		return nil, err
	}
	if len(content.Parts) != 1 {
		return nil, errors.New("expected one content part")
	}
	return content.Parts[0], nil
}

func decodeModelResponseOutputItem(encoded []byte) (ResponseOutputItem, error) {
	var response ModelResponse
	wrapped := append([]byte(`{"output":[`), encoded...)
	wrapped = append(wrapped, ']', '}')
	if err := json.Unmarshal(wrapped, &response); err != nil {
		return nil, err
	}
	if len(response.Output) != 1 {
		return nil, errors.New("expected one response output item")
	}
	return response.Output[0], nil
}

func decodeModelOutputContentPart(encoded []byte) (OutputContentPart, error) {
	var output OutputMessage
	wrapped := append([]byte(`{"content":[`), encoded...)
	wrapped = append(wrapped, ']', '}')
	if err := json.Unmarshal(wrapped, &output); err != nil {
		return nil, err
	}
	if len(output.Content) != 1 {
		return nil, errors.New("expected one output content part")
	}
	return output.Content[0], nil
}

func wrapJSONArrayItem(encoded []byte) []byte {
	wrapped := make([]byte, 0, len(encoded)+2)
	wrapped = append(wrapped, '[')
	wrapped = append(wrapped, encoded...)
	return append(wrapped, ']')
}

func cloneRawMessage(raw json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON raw message")
	}
	return append(json.RawMessage(nil), raw...), nil
}

func cloneSlice[T any](items []T) []T {
	if items == nil {
		return nil
	}
	return append([]T{}, items...)
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
