package agentgraph

import (
	"context"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) handleModelResponse(ctx context.Context, state State, request model.ModelRequest, response *model.ModelResponse, visibleNames []string, compactionThreshold int64, serverCompactionRequested bool) (graph.Transition[State], error) {
	var err error
	clearTeamForkBase(&state)
	if a.options.Team != nil && request.Input != nil {
		for _, call := range responseFunctionCalls(response.Output) {
			if call.Name == teamDispatchToolName {
				state.TeamForkBaseInput = cloneResponseInput(request.Input)
				state.TeamForkBaseCache = teamCacheIdentity{
					Provider: state.Provider, Protocol: state.Protocol, Model: request.Model,
				}
				if request.PromptCache != nil {
					state.TeamForkBaseCache.Key = request.PromptCache.Key
				}
				break
			}
		}
	}
	if serverCompactionRequested {
		if responseContainsCompaction(response.Output) {
			a.notifyCompaction(ContextCompactionEvent{Stage: "completed", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold})
		} else if response.Usage.InputTokens >= compactionThreshold || response.Usage.InputTokens <= 0 && model.EstimateInputTokens(request) >= compactionThreshold {
			state.ServerCompactionFallback = true
			a.notifyCompaction(ContextCompactionEvent{
				Stage: "fallback", Mode: "server", BeforeTokens: response.Usage.InputTokens, Threshold: compactionThreshold,
				Error: errors.New("provider did not return a compaction state; local compaction will be used on the next request"),
			})
		} else {
			a.notifyCompaction(ContextCompactionEvent{Stage: "skipped", Mode: "server", BeforeTokens: response.Usage.InputTokens, Threshold: compactionThreshold})
		}
	}

	state.History, err = appendResponseHistory(state.History, response.Output)
	if err != nil {
		return a.failTask(state, err)
	}
	state.History = pruneHistoryBeforeLatestCompaction(state.History)
	if responseContainsCompaction(response.Output) {
		state.TeamForked = false
		state.TeamForkPrefixItems = 0
	}
	state.Transcript, err = appendTranscriptOutput(state.Transcript, response.Output)
	if err != nil {
		return a.failTask(state, err)
	}
	calls := responseFunctionCalls(response.Output)
	if len(calls) == 0 {
		if a.options.ActiveCommands != nil && a.options.ActiveCommands(state) {
			return a.deferCommandCompletion(ctx, state)
		}
		state.CommandFinalizeAttempts = 0
		state.FinalResponse = response
		return a.finishAgent(state)
	}
	if state.MaxToolRounds > 0 && state.ToolRounds >= state.MaxToolRounds {
		for _, call := range calls {
			output := model.InputFunctionCallOutput{
				CallID: call.CallID,
				Output: model.NewToolOutputText("Skipped because the tool-call round limit was reached."),
			}
			state.History.Items = append(state.History.Items, output)
			state.Transcript.Items = append(state.Transcript.Items, output)
		}
		state.FinalResponse = response
		state.TaskError = fmt.Sprintf("%v: %d", model.ErrToolCallLimit, state.MaxToolRounds)
		return a.finishAgent(state)
	}
	state.CommandFinalizeAttempts = 0
	state.PendingCalls = calls
	state.PendingIndex = 0
	state.VisibleTools = visibleNames
	if state.TeamBranch {
		return graph.To(state, teamToolNode), nil
	}
	return graph.To(state, toolNode), nil
}
