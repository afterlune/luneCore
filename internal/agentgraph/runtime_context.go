package agentgraph

import (
	"context"
	"fmt"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

// ConversationState is host-owned state saved atomically with the conversation.
// It is never inferred from user-authored message text.
type ConversationState struct {
	Categories  []string           `json:"categories,omitempty"`
	HostContext string             `json:"host_context,omitempty"`
	LastRequest model.RequestTrace `json:"last_request,omitzero"`
}

func (s State) ConversationState() ConversationState {
	return ConversationState{Categories: append([]string(nil), s.SelectedCategories...), HostContext: s.HostContext, LastRequest: s.LastRequest}
}

const stableInstructions = `You are Lune, an autonomous coding agent. Use available tools to fulfill the user's request accurately, directly, and efficiently. Reply in the same language as the user's prompt.`

func (a *Agent) prepareHostContext(ctx context.Context, state State, mode string, visible []string) State {
	if !state.MemoryLoaded {
		if !state.TeamBranch && a.options.MemorySummaryFor != nil {
			summary, err := a.options.MemorySummaryFor(ctx, state)
			if err != nil {
				a.options.Logger.WarnContext(ctx, "load workspace memory summary", "error_type", fmt.Sprintf("%T", err))
			} else {
				state.MemorySummary = summary
			}
		}
		state.MemoryLoaded = true
	}
	text := taskInstructions(state.Goal, mode, state.PlanMode, a.options.Team != nil)
	if contains(visible, teamDispatchToolName) {
		text += fmt.Sprintf("\n\nDelegation is available. This agent may create at most %d children per task. Current depth: %d; maximum depth: %d. Obey the remaining allowance reported in tool results.", state.Execution.MaxTeamChildrenPerAgent, state.TeamDepth, state.Execution.MaxTeamDepth)
	}
	if state.NoInteractiveDecisions {
		text += "\n\nThis task cannot request interactive decisions. Use only permitted or already approved actions; stop and report if a new user decision or approval is required."
	}
	for _, part := range []string{state.MemorySummary, personalInstructionContext(state.PersonalInstructions), projectInstructionContext(state.ProjectInstructions)} {
		if strings.TrimSpace(part) != "" {
			text += "\n\n" + part
		}
	}
	if state.HostContext != text || !hasHostContext(state.History, text) {
		message := hostContextMessage(text)
		index := len(state.History.Items)
		if state.ContextAtTaskStart {
			index = state.ContextInsertIndex
		}
		if index < 0 || index > len(state.History.Items) {
			index = len(state.History.Items)
		}
		items := make([]model.InputItem, 0, len(state.History.Items)+1)
		items = append(items, state.History.Items[:index]...)
		items = append(items, message)
		items = append(items, state.History.Items[index:]...)
		state.History.Items = items
		state.HostContext = text
	}
	state.ContextAtTaskStart = false
	return state
}

func hostContextMessage(text string) model.InputMessage {
	return model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("[Lune runtime context]\n" + text)}
}

func hasHostContext(history model.ResponseInput, text string) bool {
	for i := len(history.Items) - 1; i >= 0; i-- {
		message, ok := history.Items[i].(model.InputMessage)
		if ok && IsHostContextMessage(message) {
			return *message.Content.Text == "[Lune runtime context]\n"+text
		}
	}
	return false
}

// IsHostContextMessage classifies model-only housekeeping for compaction;
// it is never used to derive permissions or discovery state.
func IsHostContextMessage(message model.InputMessage) bool {
	return message.Role == model.RoleUser && message.Content.Text != nil && strings.HasPrefix(*message.Content.Text, "[Lune runtime context]\n")
}
