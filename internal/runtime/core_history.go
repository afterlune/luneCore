package runtime

import "github.com/afterlune/luneCore/internal/model"

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
