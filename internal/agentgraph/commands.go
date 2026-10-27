package agentgraph

import (
	"context"
	"errors"

	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

var errRunningCommands = errors.New("agent submitted a final answer twice while commands were running")

func (a *Agent) deferCommandCompletion(ctx context.Context, state State) (graph.Transition[State], error) {
	state.CommandFinalizeAttempts++
	state.FinalResponse = nil
	if state.CommandFinalizeAttempts >= 2 {
		if state.TeamBranch {
			return a.failTask(state, errRunningCommands)
		}
		if a.options.StopCommands != nil {
			if err := a.options.StopCommands(state); err != nil {
				return graph.Transition[State]{}, err
			}
		}
		state.BlockerID = state.ExecutionID + ":running_commands"
		state.BlockerTool = "command"
		state.BlockerReason = "The agent twice submitted a final answer while commands were running. Commands were stopped; recover to inspect their results and continue."
		if a.options.BlockerObserver != nil {
			a.options.BlockerObserver(ctx, state, state.BlockerID, state.BlockerTool)
		}
		return graph.Wait(state, ExecutionBlockerContinuation, modelNode), nil
	}
	message := model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("Commands are still running. Use command_status until they finish, or explicitly command_stop them before submitting a final answer. A command start receipt is not evidence of success.")}
	state.History.Items = append(state.History.Items, message)
	state.Transcript.Items = append(state.Transcript.Items, message)
	return a.nextModelTransition(state), nil
}

func IsCommandCompletionWait(state State) bool {
	return state.CommandFinalizeAttempts >= 2 && state.BlockerTool == "command" && state.BlockerID == state.ExecutionID+":"+CommandCompletionBlocker
}
