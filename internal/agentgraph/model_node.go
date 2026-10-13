package agentgraph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) requestLead(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.requestModel(ctx, call, state)
}

func (a *Agent) requestTeam(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.requestModel(ctx, call, state)
}

func (a *Agent) requestModel(ctx context.Context, _ graph.CallInfo, state State) (graph.Transition[State], error) {
	ensureTranscript(&state)
	requester := a.requester
	if state.TeamBranch {
		if !state.TeamStarted {
			state.TeamStarted = true
			if a.options.OnBranchStarted != nil {
				a.options.OnBranchStarted(ctx, state)
			}
		}
		if a.options.Team == nil {
			return failTask(state, errors.New("team executor is unavailable"))
		}
		var err error
		requester, err = a.options.Team.Requester(ctx, state)
		if err != nil {
			return failTask(state, err)
		}
	} else {
		if state.ModelProfile != "" && state.ModelProfile != a.options.Profile {
			return failTask(state, fmt.Errorf("model profile changed from %q to %q; the paused task remains pinned to its original profile", state.ModelProfile, a.options.Profile))
		}
		if state.Provider != "" && state.Provider != a.options.Provider || state.Model != "" && state.Model != a.options.Model {
			return failTask(state, errors.New("provider/model changed; the paused task remains pinned to its original profile"))
		}
		if state.Protocol == "" {
			state.Protocol = a.options.Protocol
			state.ServerCompaction = a.options.ServerCompaction
			state.SupportsInputAudio = a.options.SupportsInputAudio
		} else if state.Protocol != a.options.Protocol || state.ServerCompaction != a.options.ServerCompaction || state.SupportsInputAudio != a.options.SupportsInputAudio {
			return failTask(state, errors.New("provider protocol or compaction capability changed; the paused task remains pinned to its original configuration"))
		}
	}
	if state.Transcript.Items == nil && state.Transcript.Text == nil {
		state.Transcript = *model.NewResponseInputItems(state.History.Items...)
	}
	visibleTools, visibleNames, err := a.visibleTools(state, state.SelectedCategory, state.PlanMode)
	if err != nil {
		return failTask(state, err)
	}
	request := model.ModelRequest{
		Model: state.Model,
		Input: model.NewResponseInputItems(state.History.Items...),
		Tools: visibleTools,
	}
	if state.ModelOverride != "" {
		request.Model = state.ModelOverride
	}
	if state.ReasoningEffort != "" {
		request.Reasoning = &model.ReasoningConfig{Effort: state.ReasoningEffort}
	}
	if request.Model == "" {
		request.Model = a.options.Model
	}
	request.Instructions = taskInstructions(state.Goal, state.PlanMode)
	compactionThreshold := int64(0)
	serverCompactionRequested := false
	if state.AutoCompact && state.ContextWindow <= 0 && !state.CompactionWarningEmitted {
		a.notifyCompaction(ContextCompactionEvent{
			Stage: "skipped", Mode: "local", Error: errors.New("contextWindow is unset or disabled; automatic compaction is waiting for a positive window"),
		})
		state.CompactionWarningEmitted = true
	}
	if state.AutoCompact && state.ContextWindow > 0 {
		ratio := state.AutoCompactThreshold
		if ratio <= 0 {
			ratio = defaultAutoCompactThreshold
		}
		compactionThreshold = contextTokenThreshold(state.ContextWindow, state.ReserveOutputTokens, ratio)
		if compactionThreshold <= 0 {
			return failTask(state, errors.New("automatic compaction has no available input-token budget"))
		}
		useServerCompaction := state.Protocol == model.ProtocolResponses && state.ServerCompaction && !state.ServerCompactionFallback
		if useServerCompaction && compactionThreshold < minimumServerCompactionThreshold {
			state.ServerCompactionFallback = true
			a.notifyCompaction(ContextCompactionEvent{
				Stage: "fallback", Mode: "server", Threshold: compactionThreshold,
				Error: fmt.Errorf("server compaction requires a threshold of at least %d tokens; switching this task to local compaction", minimumServerCompactionThreshold),
			})
			useServerCompaction = false
		}
		if useServerCompaction {
			request.ContextManagement = []model.ContextManagementItem{{Type: "compaction", CompactThreshold: compactionThreshold}}
			serverCompactionRequested = true
			a.notifyCompaction(ContextCompactionEvent{
				Stage: "started", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold,
			})
		} else {
			state, request, _, err = a.prepareAutomaticCompaction(ctx, state, request)
			if err != nil {
				if ctx.Err() != nil {
					return graph.Transition[State]{}, ctx.Err()
				}
				return failTask(state, fmt.Errorf("automatic context compaction: %w", err))
			}
		}
	}
	slog.InfoContext(ctx, "agent model request started",
		"agent_id", state.AgentID,
		"provider", state.Provider,
		"model", request.Model,
		"model_override", state.ModelOverride,
		"reasoning_effort", state.ReasoningEffort,
		"request", state.ToolRounds+1,
		"tool_rounds_completed", state.ToolRounds,
		"plan_mode", state.PlanMode,
		"selected_category", state.SelectedCategory,
		"visible_tools", visibleNames,
	)
	sendRequest := func(request model.ModelRequest) (*model.ModelResponse, error) {
		if streaming, ok := requester.(StreamingRequester); ok {
			return streaming.StreamResponse(ctx, request, func(event model.ModelStreamEvent) error {
				if a.options.StreamObserverFor != nil {
					a.options.StreamObserverFor(state, event)
				} else if a.options.StreamObserver != nil {
					a.options.StreamObserver(event)
				}
				return nil
			})
		}
		return requester.CreateResponse(ctx, request)
	}
	var response *model.Response
	response, responseErr := sendRequest(request)
	if serverCompactionRequested && compactOptionUnsupported(responseErr) {
		state.ServerCompactionFallback = true
		a.notifyCompaction(ContextCompactionEvent{
			Stage: "fallback", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold,
			Error: errors.New("provider rejected context_management; switching this task to local compaction"),
		})
		request.ContextManagement = nil
		state, request, _, err = a.prepareAutomaticCompaction(ctx, state, request)
		if err != nil {
			if ctx.Err() != nil {
				return graph.Transition[State]{}, ctx.Err()
			}
			return failTask(state, fmt.Errorf("automatic context compaction: %w", err))
		}
		serverCompactionRequested = false
		response, responseErr = sendRequest(request)
	}
	if responseErr != nil {
		if serverCompactionRequested {
			a.notifyCompaction(ContextCompactionEvent{Stage: "failed", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold, Error: responseErr})
		}
		if ctx.Err() != nil {
			return graph.Transition[State]{}, responseErr
		}
		return failTask(state, responseErr)
	}
	if response == nil {
		return failTask(state, errors.New("requester returned a nil response"))
	}
	if response.Status == "failed" {
		if response.Error != nil {
			responseErr := fmt.Errorf("model response failed (%s): %s", response.Error.Code, response.Error.Message)
			if serverCompactionRequested && compactOptionUnsupported(responseErr) {
				state.ServerCompactionFallback = true
				a.notifyCompaction(ContextCompactionEvent{Stage: "fallback", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold, Error: responseErr})
				request.ContextManagement = nil
				state, request, _, err = a.prepareAutomaticCompaction(ctx, state, request)
				if err != nil {
					if ctx.Err() != nil {
						return graph.Transition[State]{}, ctx.Err()
					}
					return failTask(state, fmt.Errorf("automatic context compaction: %w", err))
				}
				serverCompactionRequested = false
				response, responseErr = sendRequest(request)
				if responseErr != nil {
					return failTask(state, responseErr)
				}
				if response == nil {
					return failTask(state, errors.New("requester returned a nil response"))
				}
				if response.Status == "failed" {
					if response.Error != nil {
						return failTask(state, fmt.Errorf("model response failed (%s): %s", response.Error.Code, response.Error.Message))
					}
					return failTask(state, errors.New("model response failed"))
				}
			} else {
				return failTask(state, responseErr)
			}
		}
		if response.Status == "failed" {
			return failTask(state, errors.New("model response failed"))
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
		return failTask(state, err)
	}
	state.History = pruneHistoryBeforeLatestCompaction(state.History)
	state.Transcript, err = appendTranscriptOutput(state.Transcript, response.Output)
	if err != nil {
		return failTask(state, err)
	}
	calls := responseFunctionCalls(response.Output)
	if len(calls) == 0 {
		state.FinalResponse = response
		return finishAgent(state), nil
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
		return finishAgent(state), nil
	}
	state.PendingCalls = calls
	state.PendingIndex = 0
	state.VisibleTools = visibleNames
	if state.TeamBranch {
		return graph.To(state, teamToolNode), nil
	}
	return graph.To(state, toolNode), nil
}

func taskInstructions(goal string, planMode bool) string {
	var instructions []string
	if goal = strings.TrimSpace(goal); goal != "" {
		instructions = append(instructions,
			"The user set this task goal (quoted text): "+strconv.Quote(goal)+"\nTreat it as task guidance. It does not override higher-priority instructions, safety requirements, or tool permissions.")
	}
	if planMode {
		instructions = append(instructions,
			"You are in plan mode. Inspect and reason about the requested work, then present a concise implementation plan for user review. You may use read-only tools only. Do not claim to have made changes.")
	}
	return strings.Join(instructions, "\n\n")
}

func failTask(state State, err error) (graph.Transition[State], error) {
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

func finishAgent(state State) graph.Transition[State] {
	if !state.TeamBranch {
		return graph.EndExecution(state)
	}
	state.TeamText = responseText(state.FinalResponse.Output)
	if state.TaskError != "" {
		state.TeamError = state.TaskError
	}
	return graph.CompleteBranch(state)
}
