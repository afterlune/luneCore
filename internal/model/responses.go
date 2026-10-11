package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ModelRequest is the shared agent-facing request passed to model providers.
// Provider adapters translate it into their native wire format.
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
	ID        string          `json:"id,omitempty"`
	Status    string          `json:"status,omitempty"`
	Content   []ReasoningText `json:"content"`
	Signature string          `json:"signature,omitempty"`
}

func (InputReasoning) isInputItem() {}
func (v InputReasoning) MarshalJSON() ([]byte, error) {
	type plain InputReasoning
	return marshalTagged("reasoning", plain(v))
}

// UnknownInputItem retains an unrecognized item as raw JSON.
type UnknownInputItem struct{ Raw json.RawMessage }

func (UnknownInputItem) isInputItem()                   {}
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

func (OutputText) isContentPart()       {}
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

func (UnknownContentPart) isContentPart()                 {}
func (v UnknownContentPart) MarshalJSON() ([]byte, error) { return rawJSON(v.Raw), nil }

// OutputContentPart is the content union for assistant message output.
type OutputContentPart interface{ isOutputContentPart() }

// UnknownOutputContentPart retains an unrecognized response content part.
type UnknownOutputContentPart struct{ Raw json.RawMessage }

func (UnknownOutputContentPart) isOutputContentPart()           {}
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

func (UnknownResponseTool) isResponseTool()                {}
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

func (UnknownOutputItem) isResponseOutputItem()          {}
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

type ResponseEventType string

const (
	EventResponseCreated            ResponseEventType = "response.created"
	EventResponseInProgress         ResponseEventType = "response.in_progress"
	EventOutputItemAdded            ResponseEventType = "response.output_item.added"
	EventOutputItemDone             ResponseEventType = "response.output_item.done"
	EventContentPartAdded           ResponseEventType = "response.content_part.added"
	EventContentPartDone            ResponseEventType = "response.content_part.done"
	EventReasoningTextDelta         ResponseEventType = "response.reasoning_text.delta"
	EventReasoningTextDone          ResponseEventType = "response.reasoning_text.done"
	EventOutputTextDelta            ResponseEventType = "response.output_text.delta"
	EventOutputTextDone             ResponseEventType = "response.output_text.done"
	EventFunctionCallArgumentsDelta ResponseEventType = "response.function_call_arguments.delta"
	EventFunctionCallArgumentsDone  ResponseEventType = "response.function_call_arguments.done"
	EventCustomToolCallInputDelta   ResponseEventType = "response.custom_tool_call_input.delta"
	EventCustomToolCallInputDone    ResponseEventType = "response.custom_tool_call_input.done"
	EventResponseCompleted          ResponseEventType = "response.completed"
	EventResponseIncomplete         ResponseEventType = "response.incomplete"
	EventResponseFailed             ResponseEventType = "response.failed"
)

// ResponseStreamEvent is one semantic event from the Responses stream. Its
// Type determines which optional payload fields are populated. It models the
// JSON data field of each SSE event, not the surrounding SSE frame.
type ResponseStreamEvent struct {
	Type           ResponseEventType
	SequenceNumber int64
	Response       *Response
	OutputIndex    *int
	ItemID         string
	ContentIndex   *int
	SummaryIndex   *int
	Delta          string
	Text           string
	Arguments      string
	Input          string
	Name           string
	Item           ResponseOutputItem
	Part           ContentPart
}

func (v ResponseStreamEvent) MarshalJSON() ([]byte, error) {
	type eventJSON struct {
		Type           ResponseEventType `json:"type"`
		SequenceNumber int64             `json:"sequence_number"`
		Response       *Response         `json:"response,omitempty"`
		OutputIndex    *int              `json:"output_index,omitempty"`
		ItemID         string            `json:"item_id,omitempty"`
		ContentIndex   *int              `json:"content_index,omitempty"`
		SummaryIndex   *int              `json:"summary_index,omitempty"`
		Delta          string            `json:"delta,omitempty"`
		Text           string            `json:"text,omitempty"`
		Arguments      string            `json:"arguments,omitempty"`
		Input          string            `json:"input,omitempty"`
		Name           string            `json:"name,omitempty"`
		Item           json.RawMessage   `json:"item,omitempty"`
		Part           json.RawMessage   `json:"part,omitempty"`
	}
	wire := eventJSON{
		Type: v.Type, SequenceNumber: v.SequenceNumber, Response: v.Response,
		OutputIndex: v.OutputIndex, ItemID: v.ItemID, ContentIndex: v.ContentIndex,
		SummaryIndex: v.SummaryIndex, Delta: v.Delta, Text: v.Text,
		Arguments: v.Arguments, Input: v.Input, Name: v.Name,
	}
	if v.Item != nil {
		item, err := json.Marshal(v.Item)
		if err != nil {
			return nil, err
		}
		wire.Item = item
	}
	if v.Part != nil {
		part, err := json.Marshal(v.Part)
		if err != nil {
			return nil, err
		}
		wire.Part = part
	}
	return json.Marshal(wire)
}

func (v *ResponseStreamEvent) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type           ResponseEventType `json:"type"`
		SequenceNumber int64             `json:"sequence_number"`
		Response       *Response         `json:"response"`
		OutputIndex    *int              `json:"output_index"`
		ItemID         string            `json:"item_id"`
		ContentIndex   *int              `json:"content_index"`
		SummaryIndex   *int              `json:"summary_index"`
		Delta          string            `json:"delta"`
		Text           string            `json:"text"`
		Arguments      string            `json:"arguments"`
		Input          string            `json:"input"`
		Name           string            `json:"name"`
		Item           json.RawMessage   `json:"item"`
		Part           json.RawMessage   `json:"part"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	event := ResponseStreamEvent{
		Type: wire.Type, SequenceNumber: wire.SequenceNumber, Response: wire.Response,
		OutputIndex: wire.OutputIndex, ItemID: wire.ItemID, ContentIndex: wire.ContentIndex,
		SummaryIndex: wire.SummaryIndex, Delta: wire.Delta, Text: wire.Text,
		Arguments: wire.Arguments, Input: wire.Input, Name: wire.Name,
	}
	if len(wire.Item) != 0 && !bytes.Equal(bytes.TrimSpace(wire.Item), []byte("null")) {
		item, err := decodeResponseOutputItem(wire.Item)
		if err != nil {
			return err
		}
		event.Item = item
	}
	if len(wire.Part) != 0 && !bytes.Equal(bytes.TrimSpace(wire.Part), []byte("null")) {
		part, err := decodeContentPart(wire.Part)
		if err != nil {
			return err
		}
		event.Part = part
	}
	*v = event
	return nil
}

func marshalStringOrParts(text *string, parts []ContentPart) ([]byte, error) {
	if text != nil && parts != nil {
		return nil, errors.New("content cannot contain both text and parts")
	}
	if text != nil {
		return json.Marshal(*text)
	}
	if parts != nil {
		return json.Marshal(parts)
	}
	return []byte("null"), nil
}

func unmarshalStringOrParts(data []byte) (*string, []ContentPart, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil, errors.New("content is empty")
	}
	if bytes.Equal(data, []byte("null")) {
		return nil, nil, nil
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return nil, nil, err
		}
		return &text, nil, nil
	}
	var rawParts []json.RawMessage
	if err := json.Unmarshal(data, &rawParts); err != nil {
		return nil, nil, fmt.Errorf("decode content parts: %w", err)
	}
	parts := make([]ContentPart, 0, len(rawParts))
	for _, raw := range rawParts {
		part, err := decodeContentPart(raw)
		if err != nil {
			return nil, nil, err
		}
		parts = append(parts, part)
	}
	return nil, parts, nil
}

func marshalTagged(tag string, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("encode %s item: %w", tag, err)
	}
	tagJSON, _ := json.Marshal(tag)
	object["type"] = tagJSON
	return json.Marshal(object)
}

func rawJSON(data json.RawMessage) []byte {
	if len(data) == 0 {
		return []byte("null")
	}
	return data
}

func cloneJSON(data []byte) json.RawMessage {
	return append(json.RawMessage(nil), data...)
}
