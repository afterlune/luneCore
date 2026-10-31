package agentgraph

import (
	"context"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) dispatchTeam(ctx context.Context, state State, call model.OutputFunctionCall, dispatchCallID string) (graph.Transition[State], *model.InputFunctionCallOutput, error) {
	reject := func(message string) (graph.Transition[State], *model.InputFunctionCallOutput, error) {
		clearTeamForkBase(&state)
		output := model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Team request rejected: " + message)}
		return graph.Transition[State]{}, &output, nil
	}

	remaining := state.Execution.MaxTeamChildrenPerAgent - state.TeamChildrenCreated
	if state.TeamDepth >= state.Execution.MaxTeamDepth || remaining <= 0 {
		return reject("this agent has reached its delegation limit")
	}
	assignments, mode, minimumSuccesses, err := parseTeamAssignments(call.Arguments, a.options.Team, remaining)
	if err != nil {
		return reject(err.Error())
	}
	quorum := a.options.Execution.TeamQuorum
	if minimumSuccesses != nil {
		quorum = *minimumSuccesses
	}
	if quorum < 0 || quorum > len(assignments) {
		return reject(fmt.Sprintf("minimum success threshold must be between 0 and %d", len(assignments)))
	}

	var forkPrefix []model.InputItem
	if mode == TeamDispatchFork {
		forkPrefix, err = responseInputItems(state.TeamForkBaseInput)
		if err != nil || len(forkPrefix) == 0 {
			return reject("fork dispatch has no pre-dispatch conversation to copy")
		}
	}

	state.TeamAssignments = append([]TeamAssignment(nil), assignments...)
	state.TeamBranchModes = make([]string, len(assignments))
	state.TeamMinimumSuccesses = quorum
	state.TeamDispatchMode = mode
	state.TeamDispatchID = state.RunID + ":" + state.ExecutionID + ":" + dispatchCallID
	branches := make([]graph.Branch[State], 0, len(assignments))
	taskOptions := TaskOptions{
		ExecutionID: state.ExecutionID, MaxToolRounds: state.MaxToolRounds,
		ReasoningEffort: state.ReasoningEffort, PermissionOverride: state.PermissionOverride, PlanMode: state.PlanMode,
		PersonalInstructions:     state.PersonalInstructions,
		ProjectInstructionsReady: state.ProjectInstructionsReady,
		ProjectInstructions:      cloneProjectInstructions(state.ProjectInstructions),
		PlanAuthorization:        state.PlanAuthorization, NoInteractiveDecisions: state.NoInteractiveDecisions,
	}
	for _, assignment := range assignments {
		profile := a.options.Team.Profiles()[assignment.Model]
		parentCache := state.TeamForkBaseCache
		inheritContext := mode == TeamDispatchFork && sameModelIdentity(
			parentCache.Provider, parentCache.Model,
			profile.Provider, profile.Model,
		)
		branchMode := TeamDispatchTask
		branchPrefix := []model.InputItem(nil)
		if inheritContext {
			branchMode = TeamDispatchFork
			branchPrefix = forkPrefix
		}
		state.TeamBranchModes[len(branches)] = branchMode
		input, prefixCount, inputErr := teamAssignmentInput(branchMode, branchPrefix, assignment.Task, state.PlanMode)
		if inputErr != nil {
			return graph.Transition[State]{}, nil, inputErr
		}
		branch, branchErr := initialState(nil, nil, input)
		if branchErr != nil {
			return graph.Transition[State]{}, nil, branchErr
		}
		branch.RunID, branch.ExecutionID = state.RunID, state.ExecutionID
		branch.AgentID = state.AgentID + "/" + assignment.ID
		branch.AgentName, branch.TeamTaskID = assignment.ID, assignment.ID
		branch.TeamTask = assignment.Task
		branch.TeamModel = assignment.Model
		branch.TeamBranch = true
		branch.TeamRunID, branch.TeamDispatchID = state.TeamDispatchID, ""
		branch.TeamDispatchMode = branchMode
		branch.TeamForked = inheritContext
		branch.TeamForkPrefixItems = prefixCount
		if inheritContext {
			branch.HostContext = state.HostContext
			branch.Goal = state.Goal
			branch.TeamForkSourceCache = parentCache
			branch.SelectedCategories = append([]string(nil), state.SelectedCategories...)
		}
		branch.TeamDepth = state.TeamDepth + 1
		branch.TeamChildrenCreated = 0
		branch.TaskSettingsVersion = CurrentTaskSettingsVersion
		branch.MaxToolRounds = state.MaxToolRounds
		branch.Execution = state.Execution
		branch.PermissionOverride = state.PermissionOverride
		branch.PlanMode = state.PlanMode
		branch.PlanAuthorization = state.PlanAuthorization
		branch.NoInteractiveDecisions = state.NoInteractiveDecisions
		branch.ReasoningEffort = state.ReasoningEffort
		branch.PersonalInstructions = state.PersonalInstructions
		branch.ProjectInstructionsReady = state.ProjectInstructionsReady
		branch.ProjectInstructions = cloneProjectInstructions(state.ProjectInstructions)
		branch, branchErr = a.options.Team.PrepareBranch(ctx, branch, assignment, taskOptions)
		if branchErr != nil {
			return graph.Transition[State]{}, nil, branchErr
		}
		branch.PermissionOverride = state.PermissionOverride
		branch.TeamDepth = state.TeamDepth + 1
		branch.TeamChildrenCreated = 0
		branch.PlanMode = state.PlanMode
		branch.PlanAuthorization = state.PlanAuthorization
		branch.NoInteractiveDecisions = state.NoInteractiveDecisions
		branches = append(branches, graph.Branch[State]{Target: teamModelNode, State: branch, EntryTimeout: a.options.Execution.TeamEntryTimeout})
	}
	state.TeamChildrenCreated += len(assignments)
	clearTeamForkBase(&state)
	return graph.FanOut(state, branches...), nil, nil
}

func teamAssignmentInput(mode string, prefix []model.InputItem, task string, planMode bool) (*model.ResponseInput, int, error) {
	message := delegatedTaskMessage(task, planMode)
	if mode != TeamDispatchFork {
		input, err := model.NewResponseInput(message)
		return input, 0, err
	}
	items := append([]model.InputItem(nil), prefix...)
	items = append(items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(message)})
	return model.NewResponseInputItems(items...), len(prefix), nil
}

func delegatedTaskMessage(task string, planMode bool) string {
	role := "Worker task:\n"
	if planMode {
		role = "Worker task (Plan mode):\n"
	}
	return role + task
}

func sameModelIdentity(sourceProvider, sourceModel, targetProvider, targetModel string) bool {
	return sourceProvider != "" && sourceModel != "" &&
		sourceProvider == targetProvider && sourceModel == targetModel
}
