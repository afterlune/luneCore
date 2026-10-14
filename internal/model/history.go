package model

import (
	"encoding/json"
	"errors"
	"fmt"
)

func (c *Client) saveInterruptedHistory(history []InputItem, message string) {
	history = append(history, InputMessage{
		Role:    RoleAssistant,
		Content: NewMessageText(message),
	})
	c.history = history
}

func initialInputHistory(input *ResponseInput) ([]InputItem, error) {
	if input.Text != nil && input.Items != nil {
		return nil, errors.New("responses input cannot contain both text and items")
	}
	if input.Text != nil {
		return []InputItem{InputMessage{Role: RoleUser, Content: NewMessageText(*input.Text)}}, nil
	}
	if len(input.Items) == 0 {
		return nil, errors.New("input item list is empty")
	}
	return append([]InputItem(nil), input.Items...), nil
}

func appendResponseHistory(history []InputItem, output []ResponseOutputItem) ([]InputItem, error) {
	for _, item := range output {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("encode response history: %w", err)
		}
		history = append(history, UnknownInputItem{Raw: encoded})
	}
	return history, nil
}
