package agentgraph

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
)

func initialState(history, transcript, input *model.ResponseInput) (State, error) {
	if input == nil {
		return State{}, errors.New("input is nil")
	}
	items, err := responseInputItems(input)
	if err != nil {
		return State{}, err
	}
	if len(items) == 0 {
		return State{}, errors.New("input item list is empty")
	}
	previous, err := responseInputItemsOrEmpty(history)
	if err != nil {
		return State{}, fmt.Errorf("decode model context: %w", err)
	}
	transcriptItems, err := responseInputItemsOrEmpty(transcript)
	if err != nil {
		return State{}, fmt.Errorf("decode transcript: %w", err)
	}
	previous = append(previous, items...)
	transcriptItems = append(transcriptItems, items...)
	return State{
		History:    *model.NewResponseInputItems(previous...),
		Transcript: *model.NewResponseInputItems(transcriptItems...),
	}, nil
}

func responseInputItemsOrEmpty(input *model.ResponseInput) ([]model.InputItem, error) {
	if input == nil {
		return nil, nil
	}
	if input.Text == nil && input.Items == nil {
		return nil, nil
	}
	if input.Text != nil && input.Items != nil {
		return nil, errors.New("conversation cannot contain both text and items")
	}
	if input.Text != nil {
		return []model.InputItem{model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(*input.Text)}}, nil
	}
	return append([]model.InputItem(nil), input.Items...), nil
}

func responseInputItems(input *model.ResponseInput) ([]model.InputItem, error) {
	if input.Text != nil && input.Items != nil {
		return nil, errors.New("responses input cannot contain both text and items")
	}
	if input.Text != nil {
		return []model.InputItem{model.InputMessage{
			Role:    model.RoleUser,
			Content: model.NewMessageText(*input.Text),
		}}, nil
	}
	if len(input.Items) == 0 {
		return nil, errors.New("input item list is empty")
	}
	return append([]model.InputItem(nil), input.Items...), nil
}

func ensureTranscript(state *State) {
	if state == nil || state.Transcript.Items != nil || state.Transcript.Text != nil {
		return
	}
	if state.History.Text != nil {
		state.Transcript = *model.NewResponseInputText(*state.History.Text)
		return
	}
	state.Transcript = *model.NewResponseInputItems(state.History.Items...)
}

func appendResponseHistory(history model.ResponseInput, output []model.ResponseOutputItem) (model.ResponseInput, error) {
	items := append([]model.InputItem(nil), history.Items...)
	for _, item := range output {
		encoded, err := json.Marshal(item)
		if err != nil {
			return model.ResponseInput{}, fmt.Errorf("encode response history: %w", err)
		}
		items = append(items, model.UnknownInputItem{Raw: encoded})
	}
	return *model.NewResponseInputItems(items...), nil
}

func responseFunctionCalls(items []model.ResponseOutputItem) []model.OutputFunctionCall {
	calls := make([]model.OutputFunctionCall, 0)
	for _, item := range items {
		if call, ok := item.(model.OutputFunctionCall); ok {
			calls = append(calls, call)
		}
	}
	return calls
}
