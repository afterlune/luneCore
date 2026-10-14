package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

type ResponseInput struct {
	Text  *string
	Items []InputItem
}

func NewResponseInputText(text string) *ResponseInput {
	return &ResponseInput{Text: &text}
}

func NewResponseInputItems(items ...InputItem) *ResponseInput {
	return &ResponseInput{Items: append([]InputItem{}, items...)}
}

func (v ResponseInput) MarshalJSON() ([]byte, error) {
	if v.Text != nil && v.Items != nil {
		return nil, errors.New("responses input cannot contain both text and items")
	}
	if v.Text != nil {
		return json.Marshal(*v.Text)
	}
	if v.Items != nil {
		return json.Marshal(v.Items)
	}
	return []byte("null"), nil
}

func (v *ResponseInput) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*v = ResponseInput{}
		return nil
	}
	if len(data) == 0 {
		return errors.New("responses input is empty")
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*v = ResponseInput{Text: &text}
		return nil
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(data, &rawItems); err != nil {
		return fmt.Errorf("decode responses input: %w", err)
	}
	items := make([]InputItem, 0, len(rawItems))
	for _, raw := range rawItems {
		item, err := decodeInputItem(raw)
		if err != nil {
			return err
		}
		items = append(items, item)
	}
	*v = ResponseInput{Items: items}
	return nil
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// InputItem is implemented by the supported input item types below.
type InputItem interface{ isInputItem() }

type InputMessage struct {
	ID      string         `json:"id,omitempty"`
	Status  string         `json:"status,omitempty"`
	Role    Role           `json:"role"`
	Content MessageContent `json:"content"`
}

func (InputMessage) isInputItem() {}

func (v InputMessage) MarshalJSON() ([]byte, error) {
	type plain InputMessage
	return marshalTagged("message", plain(v))
}

type InputFunctionCall struct {
	ID        string `json:"id,omitempty"`
	Status    string `json:"status,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (InputFunctionCall) isInputItem() {}

func (v InputFunctionCall) MarshalJSON() ([]byte, error) {
	type plain InputFunctionCall
	return marshalTagged("function_call", plain(v))
}

type InputFunctionCallOutput struct {
	ID     string     `json:"id,omitempty"`
	Status string     `json:"status,omitempty"`
	CallID string     `json:"call_id"`
	Output ToolOutput `json:"output"`
	// TranscriptOnly is presentation content retained in the user transcript but
	// omitted from model history. It is populated by trusted client-side tools.
	TranscriptOnly []ContentPart `json:"-"`
}

func (InputFunctionCallOutput) isInputItem() {}

func (v InputFunctionCallOutput) MarshalJSON() ([]byte, error) {
	type plain InputFunctionCallOutput
	return marshalTagged("function_call_output", plain(v))
}

type InputCustomToolCall struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
}

func (InputCustomToolCall) isInputItem() {}

func (v InputCustomToolCall) MarshalJSON() ([]byte, error) {
	type plain InputCustomToolCall
	return marshalTagged("custom_tool_call", plain(v))
}

type InputCustomToolCallOutput struct {
	ID     string     `json:"id,omitempty"`
	Status string     `json:"status,omitempty"`
	CallID string     `json:"call_id"`
	Output ToolOutput `json:"output"`
}

func (InputCustomToolCallOutput) isInputItem() {}

func (v InputCustomToolCallOutput) MarshalJSON() ([]byte, error) {
	type plain InputCustomToolCallOutput
	return marshalTagged("custom_tool_call_output", plain(v))
}

type InputReasoning struct {
	ID               string          `json:"id,omitempty"`
	Status           string          `json:"status,omitempty"`
	Content          []ReasoningText `json:"content"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	Signature        string          `json:"signature,omitempty"`
}

func (InputReasoning) isInputItem() {}

func (v InputReasoning) MarshalJSON() ([]byte, error) {
	type plain InputReasoning
	return marshalTagged("reasoning", plain(v))
}

// UnknownInputItem retains an unrecognized item as raw JSON.
type UnknownInputItem struct{ Raw json.RawMessage }

func (UnknownInputItem) isInputItem() {}

func (v UnknownInputItem) MarshalJSON() ([]byte, error) { return rawJSON(v.Raw), nil }

func decodeInputItem(data []byte) (InputItem, error) {
	var header struct {
		Type string `json:"type"`
		Role Role   `json:"role"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("decode input item header: %w", err)
	}
	itemType := header.Type
	if itemType == "" && header.Role != "" {
		itemType = "message"
	}
	switch itemType {
	case "message":
		var item InputMessage
		return item, json.Unmarshal(data, &item)
	case "function_call":
		var item InputFunctionCall
		return item, json.Unmarshal(data, &item)
	case "function_call_output":
		var item InputFunctionCallOutput
		return item, json.Unmarshal(data, &item)
	case "custom_tool_call":
		var item InputCustomToolCall
		return item, json.Unmarshal(data, &item)
	case "custom_tool_call_output":
		var item InputCustomToolCallOutput
		return item, json.Unmarshal(data, &item)
	case "reasoning":
		var item InputReasoning
		return item, json.Unmarshal(data, &item)
	default:
		return UnknownInputItem{Raw: cloneJSON(data)}, nil
	}
}

// MessageContent is either a content string or a list of typed content parts.
type MessageContent struct {
	Text  *string
	Parts []ContentPart
}

func NewMessageText(text string) MessageContent { return MessageContent{Text: &text} }

func NewMessageContentParts(parts ...ContentPart) MessageContent {
	return MessageContent{Parts: append([]ContentPart{}, parts...)}
}

func (v MessageContent) MarshalJSON() ([]byte, error) {
	return marshalStringOrParts(v.Text, v.Parts)
}

func (v *MessageContent) UnmarshalJSON(data []byte) error {
	text, parts, err := unmarshalStringOrParts(data)
	if err != nil {
		return err
	}
	*v = MessageContent{Text: text, Parts: parts}
	return nil
}

// ContentPart covers the text, image, and reasoning content parts used by the
// documented Responses request and tool-output formats.
type ContentPart interface{ isContentPart() }

type InputText struct {
	Text string `json:"text"`
}

func (InputText) isContentPart() {}

func (v InputText) MarshalJSON() ([]byte, error) {
	type plain InputText
	return marshalTagged("input_text", plain(v))
}

type OutputText struct {
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations,omitempty"`
}

func (OutputText) isContentPart() {}

func (OutputText) isOutputContentPart() {}

func (v OutputText) MarshalJSON() ([]byte, error) {
	type plain OutputText
	return marshalTagged("output_text", plain(v))
}

type InputImage struct {
	ImageURL string `json:"image_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (InputImage) isContentPart() {}

func (v InputImage) MarshalJSON() ([]byte, error) {
	type plain InputImage
	return marshalTagged("input_image", plain(v))
}

// InputAudio carries base64 audio in a content part for providers whose
// configured model profile explicitly supports input audio.
type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

func (InputAudio) isContentPart() {}

func (v InputAudio) MarshalJSON() ([]byte, error) {
	type plain InputAudio
	return marshalTagged("input_audio", plain(v))
}

type ReasoningText struct {
	Text string `json:"text"`
}

func (ReasoningText) isContentPart() {}

func (v ReasoningText) MarshalJSON() ([]byte, error) {
	type plain ReasoningText
	return marshalTagged("reasoning_text", plain(v))
}

// UnknownContentPart retains an unrecognized content part as raw JSON.
type UnknownContentPart struct{ Raw json.RawMessage }

func (UnknownContentPart) isContentPart() {}

func (v UnknownContentPart) MarshalJSON() ([]byte, error) { return rawJSON(v.Raw), nil }

// OutputContentPart is the content union for assistant message output.
type OutputContentPart interface{ isOutputContentPart() }

// UnknownOutputContentPart retains an unrecognized response content part.
type UnknownOutputContentPart struct{ Raw json.RawMessage }

func (UnknownOutputContentPart) isOutputContentPart() {}

func (v UnknownOutputContentPart) MarshalJSON() ([]byte, error) { return rawJSON(v.Raw), nil }

func decodeContentPart(data []byte) (ContentPart, error) {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("decode content part header: %w", err)
	}
	switch header.Type {
	case "input_text":
		var part InputText
		return part, json.Unmarshal(data, &part)
	case "output_text":
		var part OutputText
		return part, json.Unmarshal(data, &part)
	case "input_image":
		var part InputImage
		return part, json.Unmarshal(data, &part)
	case "input_audio":
		var part InputAudio
		if err := json.Unmarshal(data, &part); err != nil {
			return nil, err
		}
		if part.Data == "" || part.Format == "" {
			return UnknownContentPart{Raw: cloneJSON(data)}, nil
		}
		return part, nil
	case "reasoning_text":
		var part ReasoningText
		return part, json.Unmarshal(data, &part)
	default:
		return UnknownContentPart{Raw: cloneJSON(data)}, nil
	}
}

// ToolOutput is either a string or a list of text/image content parts.
type ToolOutput struct {
	Text  *string
	Parts []ContentPart
}

func NewToolOutputText(text string) ToolOutput { return ToolOutput{Text: &text} }

func NewToolOutputParts(parts ...ContentPart) ToolOutput {
	return ToolOutput{Parts: append([]ContentPart{}, parts...)}
}

func (v ToolOutput) MarshalJSON() ([]byte, error) { return marshalStringOrParts(v.Text, v.Parts) }

func (v *ToolOutput) UnmarshalJSON(data []byte) error {
	text, parts, err := unmarshalStringOrParts(data)
	if err != nil {
		return err
	}
	*v = ToolOutput{Text: text, Parts: parts}
	return nil
}
