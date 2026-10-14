package agentgraph

import (
	"encoding/json"
	"fmt"

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
	for _, answer := range state.AcceptanceClarifications {
		items = append(items, model.InputMessage{
			Role: model.RoleUser, Content: model.NewMessageText("Clarification answer: " + answer),
		})
	}
	return model.NewResponseInputItems(items...), nil
}
