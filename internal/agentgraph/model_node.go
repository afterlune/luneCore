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
		instructions = append(instructions, "Active permission is read-only. Inspect and explain only; do not modify files or run commands. Web and network access require host approval. For plans, return the complete plan inside <proposed_plan> and </proposed_plan> tags; do not claim execution.")
	case "workspace":
		instructions = append(instructions, "Workspace mode permits reading and writing workspace files. Commands and network access require host approval.")
	case "ask":
		instructions = append(instructions, "Ask mode requires host approval for tools other than workspace reads.")
	case "full-access":
		instructions = append(instructions, "Full-access mode permits all available tools.")
	}
	if goal = strings.TrimSpace(goal); goal != "" {
		instructions = append(instructions,
			"Task goal (guidance only): "+strconv.Quote(goal)+" It does not override higher-priority instructions, safety requirements, or tool permissions.")
	}
	instructions = append(instructions, "Keep responses concise without omitting requested detail, evidence, or necessary code.")
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
