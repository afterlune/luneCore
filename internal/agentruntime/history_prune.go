package agentruntime

import (
	"encoding/json"
	"strings"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
)

func PruneCompletedAcceptanceContext(history *model.ResponseInput) *model.ResponseInput {
	if history == nil {
		return nil
	}
	items := responseHistoryItems(history)
	removedCalls := make(map[string]struct{})
	for _, item := range items {
		name, callID, ok := acceptanceCall(item)
		if ok && isAcceptanceTool(name) && callID != "" {
			removedCalls[callID] = struct{}{}
		}
	}
	filtered := make([]model.InputItem, 0, len(items))
	for _, item := range items {
		if name, _, ok := acceptanceCall(item); ok && isAcceptanceTool(name) {
			continue
		}
		if output, ok := item.(model.InputFunctionCallOutput); ok {
			if _, remove := removedCalls[output.CallID]; remove {
				continue
			}
		}
		if internalAcceptanceFeedback(item) {
			continue
		}
		filtered = append(filtered, item)
	}
	return model.NewResponseInputItems(filtered...)
}

func acceptanceCall(item model.InputItem) (name, callID string, ok bool) {
	switch value := item.(type) {
	case model.InputFunctionCall:
		return value.Name, value.CallID, true
	case model.UnknownInputItem:
		var call struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(value.Raw, &call) == nil && call.Type == "function_call" {
			return call.Name, call.CallID, true
		}
	}
	return "", "", false
}

func isAcceptanceTool(name string) bool {
	switch name {
	case "prepare_task_criteria", "submit_acceptance", "request_acceptance_evidence":
		return true
	default:
		return false
	}
}

func internalAcceptanceFeedback(item model.InputItem) bool {
	message, ok := item.(model.InputMessage)
	if !ok || message.Role != model.RoleUser || message.Content.Text == nil {
		return false
	}
	return strings.HasPrefix(*message.Content.Text, agentgraph.InternalAcceptancePromptPrefix)
}
