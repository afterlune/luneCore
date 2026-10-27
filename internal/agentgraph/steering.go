package agentgraph

import (
	"encoding/json"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
)

func appendSteeringInputs(state State, inputs []SteeringInput) (State, error) {
	if len(inputs) == 0 {
		return state, nil
	}
	if state.History.Items == nil {
		state.History = *model.NewResponseInputItems()
	}
	if state.Transcript.Items == nil {
		state.Transcript = *model.NewResponseInputItems(state.History.Items...)
	}
	items := make([]model.InputItem, 0)
	for _, input := range inputs {
		messageItems, err := responseInputItems(&input.Input)
		if err != nil {
			return state, fmt.Errorf("decode steering input %q: %w", input.ExecutionID, err)
		}
		items = append(items, messageItems...)
	}
	state.History.Items = append(state.History.Items, items...)
	state.Transcript.Items = append(state.Transcript.Items, items...)

	var taskInput *model.ResponseInput
	if len(state.TaskInput) > 0 {
		var err error
		taskInput, err = model.NewResponseInput(state.TaskInput)
		if err != nil {
			return state, fmt.Errorf("decode original task input: %w", err)
		}
	} else {
		taskInput = model.NewResponseInputItems()
	}
	taskItems, err := responseInputItems(taskInput)
	if err != nil {
		return state, fmt.Errorf("decode original task input items: %w", err)
	}
	taskItems = append(taskItems, items...)
	encoded, err := json.Marshal(model.NewResponseInputItems(taskItems...))
	if err != nil {
		return state, fmt.Errorf("encode task input with steering messages: %w", err)
	}
	state.TaskInput = encoded
	return state, nil
}
