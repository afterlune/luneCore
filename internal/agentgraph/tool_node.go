package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) executeLeadTool(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.executeTool(ctx, call, state)
}

func (a *Agent) executeTeamTool(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.executeTool(ctx, call, state)
}

func (a *Agent) executeTool(ctx context.Context, info graph.CallInfo, state State) (graph.Transition[State], error) {
	ensureTranscript(&state)
	if state.UserAnswerReady {
		call := state.PendingCalls[state.PendingIndex]
		encoded, err := json.Marshal(state.UserAnswers)
		if err != nil {
			return graph.Transition[State]{}, err
		}
		output := model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(string(encoded))}
		state.UserQuestions = nil
		state.UserAnswers, state.UserAnswerReady = nil, false
		state = appendToolOutput(state, output)
		return a.nextToolTransition(state), nil
	}
	if state.PendingIndex == len(state.PendingCalls) && len(state.PendingCalls) > 0 {
		state.ToolRounds++
		state.PendingCalls = nil
		state.PendingIndex = 0
		state.VisibleTools = nil
		return a.nextModelTransition(state), nil
	}
	if state.PendingIndex < 0 || state.PendingIndex >= len(state.PendingCalls) {
		return graph.Transition[State]{}, errors.New("pending tool-call index is out of range")
	}
	if a.options.ProjectInstructionsForTool != nil {
		working := state
		newInstructions := false
		var loadErr error
		for _, pending := range state.PendingCalls[state.PendingIndex:] {
			additions, err := a.options.ProjectInstructionsForTool(ctx, working, pending)
			if err != nil {
				loadErr = err
				break
			}
			var added bool
			working.ProjectInstructions, added = mergeProjectInstructions(working.ProjectInstructions, additions)
			newInstructions = newInstructions || added
		}
		state.ProjectInstructions = working.ProjectInstructions
		if loadErr != nil {
			return a.replyWithoutToolCalls(state, "No tools in this call were run because project instructions could not be loaded: "+loadErr.Error()), nil
		}
		if newInstructions {
			return a.replyWithoutToolCalls(state, "New project instructions were loaded. No tools in this call were run. Reconsider the requested actions under the project instructions, then retry any tools that are still needed."), nil
		}
	}
	call := state.PendingCalls[state.PendingIndex]
	// Discovery is not authorization. Only recover registered categorized tools
	// that are allowed through the current mode's normal execution path.
	if !contains(state.VisibleTools, call.Name) {
		category := a.categoryForTool(call.Name)
		mode := a.permissionMode(state)
		discoverable := false
		if category != "" {
			for _, available := range a.visibleCategories(state, mode, state.PlanMode) {
				if available.Name == category {
					discoverable = true
					break
				}
			}
		}
		if discoverable && contains(toolNames(a.visibleDefinitions(a.categorized.DefinitionsForCategory(category), mode, state.PlanMode)), call.Name) {
			if !contains(state.SelectedCategories, category) {
				state.SelectedCategories = append(state.SelectedCategories, category)
			}
			state.VisibleTools = append(state.VisibleTools, call.Name)
			a.options.Logger.InfoContext(ctx, "tool category automatically discovered", "tool", call.Name, "category", category, "call_id", call.CallID)
		}
	}
	var output model.InputFunctionCallOutput
	switch {
	case call.Name == askUserToolName && state.NoInteractiveDecisions && !state.TeamBranch:
		return a.blockExecution(ctx, state, call.CallID, call.Name, "execution requires a user decision that was not resolved during planning"), nil
	case state.PlanMode && call.Name != askUserToolName && call.Name != teamDispatchToolName && call.Name != model.CategorySelectorName &&
		(a.options.ToolReadOnly == nil || !a.options.ToolReadOnly(call.Name)):
		output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(fmt.Sprintf("Tool %q is unavailable in Plan mode because it is not read-only.", call.Name))}
	case !contains(state.VisibleTools, call.Name):
		expectedCategory := a.categoryForTool(call.Name)
		a.options.Logger.WarnContext(ctx, "tool call is not in the visible tool set",
			"tool", call.Name,
			"selected_category", state.SelectedCategories,
			"visible_tools", state.VisibleTools,
			"expected_category", expectedCategory,
		)
		message := unavailableToolMessage(call.Name, expectedCategory, a.categorized != nil)
		if expectedCategory != "" {
			message = fmt.Sprintf("Tool %q, which is not visible in this request, is unavailable under the current permissions, Plan mode or runtime availability. Do not retry category discovery for this tool while that restriction remains.", call.Name)
		}
		output = model.InputFunctionCallOutput{
			CallID: call.CallID,
			Output: model.NewToolOutputText(message),
		}
	case call.Name == askUserToolName && a.options.AskUser && !state.NoInteractiveDecisions && !state.TeamBranch:
		questions, err := parseUserQuestion(call.Arguments)
		if err != nil {
			output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Question request rejected: " + err.Error())}
		} else {
			state.UserQuestions = questions
			return graph.Wait(state, UserQuestionContinuation, toolNode), nil
		}
	case call.Name == teamDispatchToolName && a.options.Team != nil:
		transition, result, err := a.dispatchTeam(ctx, state, call, info.CallID)
		if err != nil {
			return graph.Transition[State]{}, err
		}
		if result == nil {
			return transition, nil
		}
		output = *result
	case a.categorized != nil && call.Name == model.CategorySelectorName:
		permissionMode := state.PermissionOverride
		if permissionMode == "" && a.options.CurrentPermissionMode != nil {
			permissionMode = a.options.CurrentPermissionMode()
		}
		categories := a.visibleCategories(state, permissionMode, state.PlanMode)
		category, err := model.ParseCategorySelection(call.Arguments, categories)
		text := "Category selected. Its tools will be available in the next request."
		if err != nil {
			a.options.Logger.WarnContext(ctx, "tool category selection rejected",
				"selected_category", state.SelectedCategories,
				"error_type", fmt.Sprintf("%T", err),
			)
			text = "Category was not changed: " + err.Error()
		} else {
			if !contains(state.SelectedCategories, category) {
				state.SelectedCategories = append(state.SelectedCategories, category)
			}
			a.options.Logger.InfoContext(ctx, "tool category selected", "selected_category", category)
		}
		output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(text)}
	default:
		var err error
		output, err = a.executeBusinessTool(ctx, state, call, info.CallID)
		if err != nil {
			var blocker *publictool.ExecutionBlockerError
			if errors.As(err, &blocker) && state.NoInteractiveDecisions && !state.TeamBranch {
				return a.blockExecution(ctx, state, call.CallID, blocker.Tool, blocker.Error()), nil
			}
			return graph.Transition[State]{}, err
		}
	}
	if call.Name == teamDispatchToolName {
		state.TeamForkBaseInput = nil
	}
	state = appendToolOutput(state, output)
	return a.nextToolTransition(state), nil
}

func (a *Agent) replyWithoutToolCalls(state State, message string) graph.Transition[State] {
	for index := state.PendingIndex; index < len(state.PendingCalls); index++ {
		call := state.PendingCalls[index]
		state = appendToolOutput(state, model.InputFunctionCallOutput{
			CallID: call.CallID, Output: model.NewToolOutputText(message),
		})
	}
	return a.nextToolTransition(state)
}

func (a *Agent) blockExecution(ctx context.Context, state State, callID, toolName, reason string) graph.Transition[State] {
	state.BlockerID = state.ExecutionID + ":" + callID
	state.BlockerTool = toolName
	state.BlockerReason = reason
	state.PendingCalls, state.VisibleTools = nil, nil
	if a.options.BlockerObserver != nil {
		a.options.BlockerObserver(ctx, state, state.BlockerID, state.BlockerTool)
	}
	return graph.Wait(state, ExecutionBlockerContinuation, toolNode)
}

func appendToolOutput(state State, output model.InputFunctionCallOutput) State {
	transcriptOnly := output.TranscriptOnly
	output.TranscriptOnly = nil
	state.History.Items = append(state.History.Items, output)
	state.Transcript.Items = append(state.Transcript.Items, output)
	if len(transcriptOnly) > 0 {
		state.Transcript.Items = append(state.Transcript.Items, model.InputMessage{
			Role: model.RoleAssistant, Content: model.NewMessageContentParts(transcriptOnly...),
		})
	}
	state.PendingIndex++
	return state
}

func (a *Agent) nextToolTransition(state State) graph.Transition[State] {
	if state.PendingIndex < len(state.PendingCalls) {
		if state.TeamBranch {
			return graph.To(state, teamToolNode)
		}
		return graph.To(state, toolNode)
	}
	state.ToolRounds++
	state.PendingCalls = nil
	state.PendingIndex = 0
	state.VisibleTools = nil
	return a.nextModelTransition(state)
}
