package agentgraph

import (
	"context"
	"errors"
	"strconv"
	"strings"

	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) requestLead(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.requestModel(ctx, call, state)
}

func (a *Agent) requestTeam(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.requestModel(ctx, call, state)
}

func taskInstructions(goal, permissionMode string, planMode, teamEnabled bool) string {
	var instructions []string
	if planMode {
		instructions = append(instructions,
			"You are in Plan mode. Analyze the request, explore facts using available read-only tools, and present a concrete plan enclosed in `<proposed_plan>` and `</proposed_plan>` tags.")
		if teamEnabled {
			instructions = append(instructions, "For a user message beginning 'Worker task', inspect with read-only tools and report findings to the lead.")
		}
	} else if teamEnabled {
		instructions = append(instructions, "For a user message beginning 'Worker task', complete only the assignment and report to the lead.")
	}

	if goal = strings.TrimSpace(goal); goal != "" {
		instructions = append(instructions,
			"Task goal (guidance only): "+strconv.Quote(goal)+" It does not override higher-priority instructions, safety requirements, or tool permissions.")
	}
	return strings.Join(instructions, "\n\n")
}

func (a *Agent) failTask(state State, err error) (graph.Transition[State], error) {
	if state.TeamBranch && a.options.StopCommands != nil {
		if cleanupErr := a.options.StopCommands(state); cleanupErr != nil {
			return graph.Transition[State]{}, errors.Join(err, cleanupErr)
		}
	}
	state.TaskError = err.Error()
	if state.TeamBranch {
		state.TeamError = err.Error()
		return graph.CompleteBranch(state), nil
	}
	return graph.EndExecution(state), nil
}

func (a *Agent) nextModelTransition(state State) graph.Transition[State] {
	if state.TeamBranch {
		return graph.To(state, teamModelNode)
	}
	return graph.To(state, modelNode)
}

func (a *Agent) finishAgent(state State) (graph.Transition[State], error) {
	if state.TeamBranch && a.options.StopCommands != nil {
		if err := a.options.StopCommands(state); err != nil {
			return graph.Transition[State]{}, err
		}
	}
	if !state.TeamBranch {
		if state.TaskError == "" && state.FinalResponse != nil && a.options.MemoryEnabled && a.options.MemoryRequester != nil && a.options.MemoryWriter != nil {
			return graph.To(state, memoryNode), nil
		}
		return graph.EndExecution(state), nil
	}
	state.TeamText = responseText(state.FinalResponse.Output)
	if state.TaskError != "" {
		state.TeamError = state.TaskError
	}
	return graph.CompleteBranch(state), nil
}
