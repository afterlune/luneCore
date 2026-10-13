package model

import (
	"encoding/json"
	"fmt"

	publicmodel "github.com/afterlune/luneCore/model"
)

type ModelRequest = publicmodel.ModelRequest
type ContextManagementItem = publicmodel.ContextManagementItem
type ResponseRequest = publicmodel.ResponseRequest
type ResponseInput = publicmodel.ResponseInput
type Role = publicmodel.Role
type InputItem = publicmodel.InputItem
type InputMessage = publicmodel.InputMessage
type InputFunctionCall = publicmodel.InputFunctionCall
type InputFunctionCallOutput = publicmodel.InputFunctionCallOutput
type InputCustomToolCall = publicmodel.InputCustomToolCall
type InputCustomToolCallOutput = publicmodel.InputCustomToolCallOutput
type InputReasoning = publicmodel.InputReasoning
type UnknownInputItem = publicmodel.UnknownInputItem
type MessageContent = publicmodel.MessageContent
type ContentPart = publicmodel.ContentPart
type InputText = publicmodel.InputText
type OutputText = publicmodel.OutputText
type InputImage = publicmodel.InputImage
type InputAudio = publicmodel.InputAudio
type ReasoningText = publicmodel.ReasoningText
type UnknownContentPart = publicmodel.UnknownContentPart
type OutputContentPart = publicmodel.OutputContentPart
type UnknownOutputContentPart = publicmodel.UnknownOutputContentPart
type ToolOutput = publicmodel.ToolOutput
type ReasoningConfig = publicmodel.ReasoningConfig
type ResponseText = publicmodel.ResponseText
type ResponseTextFormat = publicmodel.ResponseTextFormat
type ResponseTool = publicmodel.ResponseTool
type FunctionTool = publicmodel.FunctionTool
type CustomTool = publicmodel.CustomTool
type UnknownResponseTool = publicmodel.UnknownResponseTool
type ToolChoice = publicmodel.ToolChoice
type NamedToolChoice = publicmodel.NamedToolChoice
type ModelResponse = publicmodel.ModelResponse
type Response = publicmodel.Response
type ResponseError = publicmodel.ResponseError
type IncompleteDetails = publicmodel.IncompleteDetails
type ResponseUsage = publicmodel.ResponseUsage
type TokenInputDetails = publicmodel.TokenInputDetails
type TokenOutputDetails = publicmodel.TokenOutputDetails
type ResponseOutputItem = publicmodel.ResponseOutputItem
type OutputMessage = publicmodel.OutputMessage
type OutputReasoning = publicmodel.OutputReasoning
type ReasoningSummaryText = publicmodel.ReasoningSummaryText
type OutputFunctionCall = publicmodel.OutputFunctionCall
type OutputCustomToolCall = publicmodel.OutputCustomToolCall
type OutputCompaction = publicmodel.OutputCompaction
type UnknownOutputItem = publicmodel.UnknownOutputItem
type ResponseEventType = publicmodel.ResponseEventType
type ResponseStreamEvent = publicmodel.ResponseStreamEvent

const (
	RoleSystem    = publicmodel.RoleSystem
	RoleDeveloper = publicmodel.RoleDeveloper
	RoleUser      = publicmodel.RoleUser
	RoleAssistant = publicmodel.RoleAssistant

	TextFormatText         = publicmodel.TextFormatText
	TextFormatJSONObject   = publicmodel.TextFormatJSONObject
	TextFormatJSONSchema   = publicmodel.TextFormatJSONSchema
	ReasoningEffortNone    = publicmodel.ReasoningEffortNone
	ReasoningEffortMinimal = publicmodel.ReasoningEffortMinimal
	ReasoningEffortLow     = publicmodel.ReasoningEffortLow
	ReasoningEffortMedium  = publicmodel.ReasoningEffortMedium
	ReasoningEffortHigh    = publicmodel.ReasoningEffortHigh
	ReasoningEffortXHigh   = publicmodel.ReasoningEffortXHigh
	ReasoningEffortMax     = publicmodel.ReasoningEffortMax

	EventResponseCreated            = publicmodel.EventResponseCreated
	EventResponseInProgress         = publicmodel.EventResponseInProgress
	EventOutputItemAdded            = publicmodel.EventOutputItemAdded
	EventOutputItemDone             = publicmodel.EventOutputItemDone
	EventContentPartAdded           = publicmodel.EventContentPartAdded
	EventContentPartDone            = publicmodel.EventContentPartDone
	EventReasoningTextDelta         = publicmodel.EventReasoningTextDelta
	EventReasoningTextDone          = publicmodel.EventReasoningTextDone
	EventOutputTextDelta            = publicmodel.EventOutputTextDelta
	EventOutputTextDone             = publicmodel.EventOutputTextDone
	EventFunctionCallArgumentsDelta = publicmodel.EventFunctionCallArgumentsDelta
	EventFunctionCallArgumentsDone  = publicmodel.EventFunctionCallArgumentsDone
	EventCustomToolCallInputDelta   = publicmodel.EventCustomToolCallInputDelta
	EventCustomToolCallInputDone    = publicmodel.EventCustomToolCallInputDone
	EventResponseCompleted          = publicmodel.EventResponseCompleted
	EventResponseIncomplete         = publicmodel.EventResponseIncomplete
	EventResponseFailed             = publicmodel.EventResponseFailed
)

func NewResponseInputText(text string) *ResponseInput {
	return publicmodel.NewResponseInputText(text)
}

func NewResponseInputItems(items ...InputItem) *ResponseInput {
	return publicmodel.NewResponseInputItems(items...)
}

func NewMessageText(text string) MessageContent {
	return publicmodel.NewMessageText(text)
}

func NewMessageContentParts(parts ...ContentPart) MessageContent {
	return publicmodel.NewMessageContentParts(parts...)
}

func NewToolOutputText(text string) ToolOutput {
	return publicmodel.NewToolOutputText(text)
}

func NewToolOutputParts(parts ...ContentPart) ToolOutput {
	return publicmodel.NewToolOutputParts(parts...)
}

func decodeInputItem(data []byte) (InputItem, error) {
	wrapped := make([]byte, 0, len(data)+2)
	wrapped = append(wrapped, '[')
	wrapped = append(wrapped, data...)
	wrapped = append(wrapped, ']')

	var input ResponseInput
	if err := json.Unmarshal(wrapped, &input); err != nil {
		return nil, fmt.Errorf("decode input item: %w", err)
	}
	if len(input.Items) != 1 {
		return nil, fmt.Errorf("decode input item: expected one item, got %d", len(input.Items))
	}
	return input.Items[0], nil
}

func cloneJSON(data []byte) json.RawMessage {
	return append(json.RawMessage(nil), data...)
}
