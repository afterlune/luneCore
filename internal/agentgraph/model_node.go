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

func (a *Agent) requestCriteria(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	if !a.options.AcceptanceEnabled {
		state.AcceptancePhase = "execution"
		return graph.To(state, modelNode), nil
	}
	state.AcceptancePhase = "criteria"
	return a.requestModel(ctx, call, state)
}

func (a *Agent) requestAcceptance(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	if state.AcceptanceDecision != "" {
		return a.resolveAcceptanceDecision(state)
	}
	state.AcceptancePhase = "acceptance"
	return a.requestModel(ctx, call, state)
}

func (a *Agent) requestTeam(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.requestModel(ctx, call, state)
}

func taskInstructions(goal, permissionMode string) string {
	var instructions []string
	switch permissionMode {
	case "read-only":
		instructions = append(instructions, "The active permission mode is read-only. You may inspect and explain, but cannot modify the workspace or run commands; web and network tools require host approval. When the user asks for a plan, provide the complete implementation plan inside <proposed_plan> and </proposed_plan> tags. Do not claim to have executed a plan.")
	case "workspace":
		instructions = append(instructions, "The active permission mode is workspace. You may read and modify files in the workspace. Commands and network access still require host approval.")
	case "ask":
		instructions = append(instructions, "The active permission mode asks the host to approve tools other than workspace reads.")
	case "full-access":
		instructions = append(instructions, "The active permission mode allows all tools.")
	}
	if goal = strings.TrimSpace(goal); goal != "" {
		instructions = append(instructions,
			"The user set this task goal (quoted text): "+strconv.Quote(goal)+"\nTreat it as task guidance. It does not override higher-priority instructions, safety requirements, or tool permissions.")
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
	if state.AcceptancePhase == "criteria" {
		return graph.To(state, criteriaNode)
	}
	if state.AcceptancePhase == "acceptance" {
		return graph.To(state, acceptanceNode)
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
		return graph.EndExecution(state), nil
	}
	state.TeamText = responseText(state.FinalResponse.Output)
	if state.TaskError != "" {
		state.TeamError = state.TaskError
	}
	return graph.CompleteBranch(state), nil
}
