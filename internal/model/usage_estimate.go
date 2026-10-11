package model

import (
	"encoding/json"
	"unicode"
)

// EstimateInputTokens makes a provider-neutral, text-only estimate from the
// request sent to the provider. Server-reported usage remains authoritative.
// Image contents and unknown multimodal items are excluded.
func EstimateInputTokens(request ModelRequest) int64 {
	estimate := tokenEstimator{}
	estimate.add(request.Instructions)
	if request.Input != nil {
		if request.Input.Text != nil {
			estimate.add(*request.Input.Text)
		}
		for _, item := range request.Input.Items {
			estimate.addInputItem(item)
		}
	}
	for _, tool := range request.Tools {
		estimate.addJSON(tool)
	}
	if request.Text != nil {
		estimate.addJSON(request.Text)
	}
	if request.ToolChoice != nil {
		estimate.addJSON(request.ToolChoice)
	}
	return estimate.value()
}

type tokenEstimator struct {
	tokens    int64
	asciiSize int64
}

func (estimate *tokenEstimator) add(text string) {
	for _, character := range text {
		switch {
		case character <= unicode.MaxASCII:
			estimate.asciiSize++
		case unicode.In(character, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana):
			estimate.flushASCII()
			estimate.tokens++
		case unicode.IsLetter(character) || unicode.IsNumber(character) || unicode.IsPunct(character):
			estimate.flushASCII()
			estimate.tokens++
		default:
			estimate.flushASCII()
			estimate.tokens += 2
		}
	}
}

func (estimate *tokenEstimator) addJSON(value any) {
	encoded, err := json.Marshal(value)
	if err == nil {
		estimate.add(string(encoded))
	}
}

func (estimate *tokenEstimator) addInputItem(item InputItem) {
	switch item := item.(type) {
	case InputMessage:
		estimate.add(string(item.Role))
		if item.Content.Text != nil {
			estimate.add(*item.Content.Text)
		}
		for _, part := range item.Content.Parts {
			estimate.addContentPart(part)
		}
	case InputFunctionCall:
		estimate.add(item.Name)
		estimate.add(item.Arguments)
	case InputFunctionCallOutput:
		estimate.addToolOutput(item.Output)
	case InputCustomToolCall:
		estimate.add(item.Name)
		estimate.add(item.Input)
	case InputCustomToolCallOutput:
		estimate.addToolOutput(item.Output)
	case InputReasoning:
		for _, part := range item.Content {
			estimate.add(part.Text)
		}
		// Unknown items may contain image or other non-text payloads.
	}
}

func (estimate *tokenEstimator) addContentPart(part ContentPart) {
	switch part := part.(type) {
	case InputText:
		estimate.add(part.Text)
	case OutputText:
		estimate.add(part.Text)
	case ReasoningText:
		estimate.add(part.Text)
		// Image payloads and unknown parts are deliberately excluded.
	}
}

func (estimate *tokenEstimator) addToolOutput(output ToolOutput) {
	if output.Text != nil {
		estimate.add(*output.Text)
	}
	for _, part := range output.Parts {
		estimate.addContentPart(part)
	}
}

func (estimate *tokenEstimator) flushASCII() {
	if estimate.asciiSize > 0 {
		estimate.tokens += (estimate.asciiSize + 3) / 4
		estimate.asciiSize = 0
	}
}

func (estimate *tokenEstimator) value() int64 {
	estimate.flushASCII()
	return estimate.tokens
}
