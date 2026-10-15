package agentgraph

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/afterlune/luneCore/internal/model"
)

func acceptancePhaseInput(state State) (*model.ResponseInput, error) {
	var task model.ResponseInput
	if len(state.TaskInput) > 0 {
		if err := json.Unmarshal(state.TaskInput, &task); err != nil {
			return nil, fmt.Errorf("decode current acceptance task input: %w", err)
		}
	}
	items, err := responseInputItemsOrEmpty(&task)
	if err != nil {
		return nil, fmt.Errorf("read current acceptance task input: %w", err)
	}
	if contextItems := recentAcceptanceExchange(state.Transcript, items); len(contextItems) > 0 {
		withContext := make([]model.InputItem, 0, len(contextItems)+len(items)+2)
		withContext = append(withContext, model.InputMessage{
			Role:    model.RoleUser,
			Content: model.NewMessageText("Recent conversation context for interpreting the current follow-up only; it is not a new request."),
		})
		withContext = append(withContext, contextItems...)
		withContext = append(withContext, model.InputMessage{
			Role:    model.RoleUser,
			Content: model.NewMessageText("Current user request; this turn authorizes the task below."),
		})
		items = append(withContext, items...)
	}
	for _, answer := range state.AcceptanceClarifications {
		items = append(items, model.InputMessage{
			Role: model.RoleUser, Content: model.NewMessageText("Clarification answer: " + answer),
		})
	}
	return model.NewResponseInputItems(items...), nil
}

func recentAcceptanceExchange(transcript model.ResponseInput, current []model.InputItem) []model.InputItem {
	transcriptItems, err := responseInputItemsOrEmpty(&transcript)
	if err != nil || len(transcriptItems) < len(current) {
		return nil
	}
	priorEnd := len(transcriptItems) - len(current)
	if !reflect.DeepEqual(transcriptItems[priorEnd:], current) {
		return nil
	}
	prior := transcriptItems[:priorEnd]

	latestUser, latestAssistant := -1, -1
	for index := len(prior) - 1; index >= 0 && (latestUser < 0 || latestAssistant < 0); index-- {
		message, ok := prior[index].(model.InputMessage)
		if !ok {
			continue
		}
		switch message.Role {
		case model.RoleUser:
			if latestUser < 0 {
				latestUser = index
			}
		case model.RoleAssistant:
			if latestAssistant < 0 {
				latestAssistant = index
			}
		}
	}

	if latestUser < 0 || latestAssistant < 0 {
		return nil
	}
	if latestUser < latestAssistant {
		return []model.InputItem{prior[latestUser], prior[latestAssistant]}
	}
	return []model.InputItem{prior[latestAssistant], prior[latestUser]}
}
