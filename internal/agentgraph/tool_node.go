package agentgraph

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/afterlune/luneCore/acceptance"
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
		if state.AcceptancePhase == "criteria" {
			state.AcceptanceClarifications = append(state.AcceptanceClarifications, state.UserAnswer)
		}
		output := model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("User's answer: " + state.UserAnswer)}
		state.UserQuestion, state.UserQuestionOptions = "", nil
		state.UserAnswer, state.UserAnswerReady = "", false
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
	var output model.InputFunctionCallOutput
	evidenceEligible := false
	switch {
	case call.Name == submitCriteriaToolName && a.options.AcceptanceEnabled && !state.TeamBranch && state.AcceptancePhase == "criteria" && len(state.AcceptanceCriteria) == 0:
		if len(state.PendingCalls) != 1 {
			return a.rejectAcceptanceCall(state, call, criteriaNode, "Submit task criteria in a single tool call.")
		}
		var submission criteriaSubmission
		if err := parseJSONStrict(call.Arguments, &submission); err != nil {
			return a.rejectAcceptanceCall(state, call, criteriaNode, "Criteria were not valid structured JSON: "+err.Error())
		}
		if err := validateCriteria(state, submission); err != nil {
			return a.rejectAcceptanceCall(state, call, criteriaNode, "Criteria were rejected: "+err.Error())
		}
		state.AcceptanceCriteria = append([]acceptance.Criterion(nil), submission.Criteria...)
		state.AcceptanceKind = submission.Kind
		state.AcceptanceFormatRetries = 0
		state.AcceptancePhase = "execution"
		state = appendToolOutput(state, model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Acceptance criteria saved for this execution.")})
		state.PendingCalls, state.PendingIndex, state.VisibleTools = nil, 0, nil
		a.observeAcceptance(state, "criteria_prepared", "", "")
		return graph.To(state, modelNode), nil
	case call.Name == submitAcceptanceToolName && a.options.AcceptanceEnabled && !state.TeamBranch && state.AcceptancePhase == "acceptance":
		if len(state.PendingCalls) != 1 {
			return a.rejectAcceptanceCall(state, call, acceptanceNode, "Submit one acceptance action in a single tool call.")
		}
		var submission acceptanceSubmission
		if err := parseJSONStrict(call.Arguments, &submission); err != nil {
			return a.rejectAcceptanceCall(state, call, acceptanceNode, "Acceptance report was not valid structured JSON: "+err.Error())
		}
		report, err := validateModelReport(state, submission)
		if err != nil {
			return a.rejectAcceptanceCall(state, call, acceptanceNode, "Acceptance report was rejected: "+err.Error())
		}
		state = appendToolOutput(state, model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Acceptance report received.")})
		state.PendingCalls, state.PendingIndex, state.VisibleTools = nil, 0, nil
		return a.acceptModelReport(ctx, state, report)
	case call.Name == requestEvidenceToolName && a.options.AcceptanceEnabled && !state.TeamBranch && state.AcceptancePhase == "acceptance":
		if len(state.PendingCalls) != 1 {
			return a.rejectAcceptanceCall(state, call, acceptanceNode, "Submit one acceptance action in a single tool call.")
		}
		var request evidenceRequest
		if err := parseJSONStrict(call.Arguments, &request); err != nil || strings.TrimSpace(request.Question) == "" {
			if err == nil {
				err = errors.New("question must not be empty")
			}
			return a.rejectAcceptanceCall(state, call, acceptanceNode, "Evidence request was rejected: "+err.Error())
		}
		state.AcceptanceEvidenceRequest = strings.TrimSpace(request.Question)
		state.AcceptancePhase = "execution"
		state = appendToolOutput(state, model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Returning to task execution to gather the requested evidence.")})
		state.PendingCalls, state.PendingIndex, state.VisibleTools = nil, 0, nil
		return graph.To(state, modelNode), nil
	case call.Name == askUserToolName && a.options.AskUser && !state.NoInteractiveDecisions && !state.TeamBranch:
		question, options, err := parseUserQuestion(call.Arguments)
		if err != nil {
			output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Question request rejected: " + err.Error())}
		} else {
			state.UserQuestion, state.UserQuestionOptions = question, append([]model.UserQuestionOption(nil), options...)
			return graph.Wait(state, UserQuestionContinuation, toolNode), nil
		}
	case call.Name == askUserToolName && state.NoInteractiveDecisions && !state.TeamBranch:
		return a.blockExecution(ctx, state, call.CallID, call.Name, "execution requires a user decision that was not resolved during planning"), nil
	case call.Name == teamDispatchToolName && a.options.Team != nil:
		assignments, minimumSuccesses, err := parseTeamAssignments(call.Arguments, a.options.Team)
		if err != nil {
			output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Team request rejected: " + err.Error())}
		} else {
			quorum := a.options.Execution.TeamQuorum
			if minimumSuccesses != nil {
				quorum = *minimumSuccesses
			}
			if quorum < 0 || quorum > len(assignments) {
				output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(fmt.Sprintf("Team request rejected: minimum success threshold must be between 0 and %d", len(assignments)))}
				break
			}
			state.TeamAssignments = append([]TeamAssignment(nil), assignments...)
			state.TeamMinimumSuccesses = quorum
			state.TeamDispatchID = state.RunID + ":" + state.ExecutionID + ":" + info.CallID
			branches := make([]graph.Branch[State], 0, len(assignments))
			taskOptions := TaskOptions{
				ExecutionID: state.ExecutionID, MaxToolRounds: state.MaxToolRounds,
				ReasoningEffort: state.ReasoningEffort, PermissionOverride: state.PermissionOverride,
				PersonalInstructions:     state.PersonalInstructions,
				ProjectInstructionsReady: state.ProjectInstructionsReady,
				ProjectInstructions:      cloneProjectInstructions(state.ProjectInstructions),
				PlanAuthorization:        state.PlanAuthorization, NoInteractiveDecisions: state.NoInteractiveDecisions,
			}
			for _, assignment := range assignments {
				input, inputErr := model.NewResponseInput(assignment.Task)
				if inputErr != nil {
					return graph.Transition[State]{}, inputErr
				}
				branch, branchErr := initialState(nil, nil, input)
				if branchErr != nil {
					return graph.Transition[State]{}, branchErr
				}
				branch.RunID, branch.ExecutionID = state.RunID, state.ExecutionID
				branch.AgentID = state.AgentID + "/" + assignment.ID
				branch.AgentName, branch.TeamTaskID = assignment.ID, assignment.ID
				branch.TeamModel = assignment.Model
				branch.TeamBranch = true
				branch.TeamRunID, branch.TeamDispatchID = state.TeamDispatchID, ""
				branch.TaskSettingsVersion = CurrentTaskSettingsVersion
				branch.MaxToolRounds = state.MaxToolRounds
				branch.Execution = state.Execution
				branch.PermissionOverride = state.PermissionOverride
				branch.PlanAuthorization = state.PlanAuthorization
				branch.NoInteractiveDecisions = state.NoInteractiveDecisions
				branch.ReasoningEffort = state.ReasoningEffort
				branch.PersonalInstructions = state.PersonalInstructions
				branch.ProjectInstructionsReady = state.ProjectInstructionsReady
				branch.ProjectInstructions = cloneProjectInstructions(state.ProjectInstructions)
				branch, branchErr = a.options.Team.PrepareBranch(ctx, branch, assignment, taskOptions)
				if branchErr != nil {
					return graph.Transition[State]{}, branchErr
				}
				branch.PermissionOverride = state.PermissionOverride
				branch.PlanAuthorization = state.PlanAuthorization
				branch.NoInteractiveDecisions = state.NoInteractiveDecisions
				branches = append(branches, graph.Branch[State]{Target: teamModelNode, State: branch, EntryTimeout: a.options.Execution.TeamEntryTimeout})
			}
			return graph.FanOut(state, branches...), nil
		}
	case a.categorized != nil && call.Name == model.CategorySelectorName:
		permissionMode := state.PermissionOverride
		if permissionMode == "" && a.options.CurrentPermissionMode != nil {
			permissionMode = a.options.CurrentPermissionMode()
		}
		categories := a.visibleCategories(permissionMode)
		category, err := model.ParseCategorySelection(call.Arguments, categories)
		text := "Category selected. Its tools will be available in the next request."
		if err != nil {
			a.options.Logger.WarnContext(ctx, "tool category selection rejected",
				"selected_category", state.SelectedCategory,
				"error_type", fmt.Sprintf("%T", err),
			)
			text = "Category was not changed: " + err.Error()
		} else {
			state.SelectedCategory = category
			a.options.Logger.InfoContext(ctx, "tool category selected", "selected_category", category)
		}
		output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(text)}
	case !contains(state.VisibleTools, call.Name):
		expectedCategory := a.categoryForTool(call.Name)
		a.options.Logger.WarnContext(ctx, "tool call is not in the visible tool set",
			"tool", call.Name,
			"selected_category", state.SelectedCategory,
			"visible_tools", state.VisibleTools,
			"expected_category", expectedCategory,
		)
		output = model.InputFunctionCallOutput{
			CallID: call.CallID,
			Output: model.NewToolOutputText(unavailableToolMessage(call.Name, expectedCategory, a.categorized != nil)),
		}
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
		evidenceEligible = true
	}

	if a.options.AcceptanceEnabled && !state.TeamBranch && state.AcceptancePhase == "execution" && evidenceEligible {
		state = saveToolEvidence(state, call.CallID, output)
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

func (a *Agent) rejectAcceptanceCall(state State, call model.OutputFunctionCall, target, reason string) (graph.Transition[State], error) {
	state = appendToolOutput(state, model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(reason)})
	state.PendingCalls, state.PendingIndex, state.VisibleTools = nil, 0, nil
	if state.AcceptanceFormatRetries < 1 {
		state.AcceptanceFormatRetries++
		return graph.To(state, target), nil
	}
	return a.pauseAcceptanceFormat(state, reason), nil
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
