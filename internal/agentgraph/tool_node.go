package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/afterlune/luneCore/acceptance"
	"github.com/afterlune/luneCore/internal/model"
	runtimepermission "github.com/afterlune/luneCore/internal/tool"
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
			state.UserQuestion, state.UserQuestionOptions = question, append([]string(nil), options...)
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
		toolContext := runtimepermission.WithPermissionMode(ctx, state.PermissionOverride)
		toolContext = publictool.WithPlanAuthorization(toolContext, state.PlanAuthorization)
		toolContext = publictool.WithNoInteractiveDecisions(toolContext, state.NoInteractiveDecisions)
		toolContext = model.WithAgentID(toolContext, state.AgentID)
		toolContext = model.WithInputAudioSupport(toolContext, state.SupportsInputAudio)
		identity := publictool.CallIdentityFromContext(toolContext)
		identity.RunID = state.RunID
		identity.ExecutionID = state.ExecutionID
		identity.AgentID = state.AgentID
		identity.InvocationID = info.CallID
		toolContext = publictool.WithCallIdentity(toolContext, identity)
		if recoverable, ok := a.tools.(interface {
			ExecuteCallRecoverable(context.Context, model.OutputFunctionCall) (model.InputFunctionCallOutput, error)
		}); ok {
			var err error
			output, err = recoverable.ExecuteCallRecoverable(toolContext, call)
			if err != nil {
				var blocker *publictool.ExecutionBlockerError
				if errors.As(err, &blocker) && state.NoInteractiveDecisions && !state.TeamBranch {
					return a.blockExecution(ctx, state, call.CallID, blocker.Tool, blocker.Error()), nil
				}
				return graph.Transition[State]{}, err
			}
		} else {
			output = a.tools.ExecuteCall(toolContext, call)
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

func (a *Agent) visibleTools(state State, category, permissionMode string) ([]model.ResponseTool, []string, error) {
	if permissionMode == "" {
		permissionMode = state.PermissionOverride
	}
	if permissionMode == "" && a.options.CurrentPermissionMode != nil {
		permissionMode = a.options.CurrentPermissionMode()
	}
	addRuntimeTools := func(definitions []model.ResponseTool) []model.ResponseTool {
		if a.options.Team != nil && len(a.options.Team.Profiles()) > 0 && a.options.Team.MaxParallelAgents() > 0 {
			definitions = append(definitions, teamDispatchDefinition(a.options.Team))
		}
		if a.options.AskUser && !state.NoInteractiveDecisions && !state.TeamBranch {
			definitions = append(definitions, askUserDefinition())
		}
		return definitions
	}
	if a.categorized == nil {
		definitions := a.visibleDefinitions(a.tools.Definitions(), permissionMode)
		definitions = addRuntimeTools(definitions)
		return definitions, toolNames(definitions), nil
	}
	categories := a.visibleCategories(permissionMode)
	selector, err := model.CategorySelectorDefinition(categories)
	if err != nil {
		return nil, nil, fmt.Errorf("build category selector: %w", err)
	}
	definitions := []model.ResponseTool{selector}
	if category != "" {
		definitions = append(definitions, a.visibleDefinitions(a.categorized.DefinitionsForCategory(category), permissionMode)...)
	}
	definitions = addRuntimeTools(definitions)
	return definitions, toolNames(definitions), nil
}

func (a *Agent) visibleCategories(permissionMode string) []model.ToolCategory {
	var categories []model.ToolCategory
	for _, category := range a.categories {
		if len(a.visibleDefinitions(a.categorized.DefinitionsForCategory(category.Name), permissionMode)) > 0 {
			categories = append(categories, category)
		}
	}
	return categories
}

func (a *Agent) visibleDefinitions(definitions []model.ResponseTool, permissionMode string) []model.ResponseTool {
	if a.options.ToolVisibleForMode == nil && a.options.ToolVisible == nil {
		return definitions
	}
	visible := make([]model.ResponseTool, 0, len(definitions))
	for _, definition := range definitions {
		function, ok := definition.(model.FunctionTool)
		isVisible := !ok
		if ok {
			if a.options.ToolVisibleForMode != nil {
				isVisible = a.options.ToolVisibleForMode(function.Name, permissionMode)
			} else {
				isVisible = a.options.ToolVisible(function.Name)
			}
		}
		if isVisible {
			visible = append(visible, definition)
		}
	}
	return visible
}

func (a *Agent) categoryForTool(name string) string {
	if a.categorized == nil {
		return ""
	}
	for _, category := range a.categories {
		for _, definition := range a.categorized.DefinitionsForCategory(category.Name) {
			if function, ok := definition.(model.FunctionTool); ok && function.Name == name {
				return category.Name
			}
		}
	}
	return ""
}

func unavailableToolMessage(name, category string, categorized bool) string {
	if category != "" {
		return fmt.Sprintf("Tool %q belongs to category %q, which is not visible in this request. Call %s with category %q, then retry the tool in the next request.",
			name, category, model.CategorySelectorName, category)
	}
	if categorized {
		return fmt.Sprintf("Tool %q is not registered in a discoverable category. Call %s to review available tools, then use a listed tool.",
			name, model.CategorySelectorName)
	}
	return fmt.Sprintf("Tool %q is not available in this request. Only call tools listed in the current request.", name)
}

func toolNames(definitions []model.ResponseTool) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		if function, ok := definition.(model.FunctionTool); ok {
			names = append(names, function.Name)
		}
	}
	return names
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func askUserDefinition() model.FunctionTool {
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "A concise clarification question for the user."},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Use an empty array when no choices are needed. The user may also provide a free-text answer.",
			},
		},
		"required":             []string{"question", "options"},
		"additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name: askUserToolName,
		Description: "Ask the user a concise question only when missing information blocks useful progress. " +
			"Optionally provide answer choices; the user may choose one or reply with free text.",
		Parameters: parameters,
		Strict:     &strict,
	}
}

func parseUserQuestion(arguments string) (string, []string, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var input userQuestionInput
	if err := decoder.Decode(&input); err != nil {
		return "", nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", nil, errors.New("expected one JSON object")
	}
	input.Question = strings.TrimSpace(input.Question)
	if input.Question == "" {
		return "", nil, errors.New("question must not be empty")
	}
	for index := range input.Options {
		input.Options[index] = strings.TrimSpace(input.Options[index])
		if input.Options[index] == "" {
			return "", nil, errors.New("answer options must not be empty")
		}
	}
	return input.Question, input.Options, nil
}

func teamDispatchDefinition(team TeamExecutor) model.FunctionTool {
	profiles := team.Profiles()
	profileIDs := make([]string, 0, len(profiles))
	for profileID := range profiles {
		profileIDs = append(profileIDs, profileID)
	}
	sort.Strings(profileIDs)
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": team.MaxParallelAgents(),
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":    map[string]any{"type": "string", "description": "Short unique name for this team member."},
						"model": map[string]any{"type": "string", "enum": profileIDs},
						"task":  map[string]any{"type": "string", "description": "A bounded, independent task for this member."},
					},
					"required":             []string{"id", "model", "task"},
					"additionalProperties": false,
				},
			},
			"minimum_successes": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "integer", "minimum": 0},
					map[string]any{"type": "null"},
				},
				"description": "Minimum successful members before results can merge. Use null to inherit the host default; zero waits for every member.",
			},
		},
		"required":             []string{"tasks", "minimum_successes"},
		"additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name: teamDispatchToolName,
		Description: "Delegate independent work to multiple team members at once. Choose each member's model profile from the listed catalog. " +
			"Members have isolated context, share the workspace and current permissions, and return results for you to synthesize. Use this only when tasks can proceed independently. Set minimum_successes to null to use the host default or zero to wait for every member.",
		Parameters: parameters,
		Strict:     &strict,
	}
}

func parseTeamAssignments(arguments string, team TeamExecutor) ([]TeamAssignment, *int, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var request teamDispatchRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, errors.New("expected one JSON object")
	}
	limit := team.MaxParallelAgents()
	if len(request.Tasks) == 0 || len(request.Tasks) > limit {
		return nil, nil, fmt.Errorf("team task count must be between 1 and %d", limit)
	}
	if request.MinimumSuccesses != nil && (*request.MinimumSuccesses < 0 || *request.MinimumSuccesses > len(request.Tasks)) {
		return nil, nil, fmt.Errorf("minimum_successes must be between 0 and %d", len(request.Tasks))
	}
	profiles := team.Profiles()
	seen := make(map[string]struct{}, len(request.Tasks))
	for index := range request.Tasks {
		task := &request.Tasks[index]
		task.ID = strings.TrimSpace(task.ID)
		task.Model = strings.TrimSpace(task.Model)
		task.Task = strings.TrimSpace(task.Task)
		if task.ID == "" || task.Task == "" {
			return nil, nil, errors.New("each team task requires a non-empty id and task")
		}
		if _, exists := seen[task.ID]; exists {
			return nil, nil, fmt.Errorf("team member id %q is repeated", task.ID)
		}
		seen[task.ID] = struct{}{}
		if _, exists := profiles[task.Model]; !exists {
			return nil, nil, fmt.Errorf("unknown team model profile %q", task.Model)
		}
	}
	return request.Tasks, request.MinimumSuccesses, nil
}

func formatTeamResults(results []TeamMemberResult) string {
	var output strings.Builder
	output.WriteString("Team results:")
	for _, result := range results {
		output.WriteString("\n\n## ")
		output.WriteString(result.ID)
		output.WriteString(" (model ")
		output.WriteString(result.Model)
		output.WriteString(")\n")
		if result.Error != "" {
			output.WriteString("Failed: ")
			output.WriteString(result.Error)
		} else {
			output.WriteString(result.Text)
		}
	}
	return output.String()
}
