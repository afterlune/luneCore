package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
)

func responseHistoryItems(history *model.ResponseInput) []model.InputItem {
	if history == nil {
		return nil
	}
	if history.Items != nil {
		return append([]model.InputItem(nil), history.Items...)
	}
	if history.Text != nil {
		return []model.InputItem{model.InputMessage{
			Role:    model.RoleUser,
			Content: model.NewMessageText(*history.Text),
		}}
	}
	return nil
}

func rewoundHistory(raw json.RawMessage, userTurns int) (json.RawMessage, int, error) {
	if userTurns <= 0 {
		return nil, 0, fmt.Errorf("rewind count must be greater than zero")
	}
	var history model.ResponseInput
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &history); err != nil {
			return nil, 0, fmt.Errorf("decode session history: %w", err)
		}
	}
	items := responseHistoryItems(&history)
	var starts []int
	for index, item := range items {
		if message, ok := item.(model.InputMessage); ok && message.Role == model.RoleUser {
			starts = append(starts, index)
		}
	}
	if len(starts) == 0 {
		return json.RawMessage("null"), 0, nil
	}
	removed := min(userTurns, len(starts))
	cut := starts[len(starts)-removed]
	encoded, err := json.Marshal(model.NewResponseInputItems(items[:cut]...))
	if err != nil {
		return nil, 0, fmt.Errorf("encode rewound history: %w", err)
	}
	return encoded, removed, nil
}
