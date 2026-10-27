package agentgraph

import (
	"context"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) requestModel(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	if len(state.PendingSteeringInputs) > 0 {
		accepted := state.PendingSteeringInputs
		if a.options.AcknowledgeSteeringInputs != nil {
			var err error
			accepted, err = a.options.AcknowledgeSteeringInputs(ctx, state.ExecutionID, accepted)
			if err != nil {
				return a.failTask(state, fmt.Errorf("acknowledge steering inputs: %w", err))
			}
		}
		if len(accepted) == 0 && state.SteeringFinalizing && a.options.LoadSteeringInputs != nil {
			// A user can cancel a queued message after the final-response boundary
			// observed it but before this node acknowledges it. Recheck while
			// atomically closing the gate when the queue is still empty.
			for len(accepted) == 0 {
				fresh, err := a.options.LoadSteeringInputs(ctx, state.ExecutionID, true)
				if err != nil {
					return a.failTask(state, fmt.Errorf("recheck steering inputs before final response: %w", err))
				}
				if len(fresh) == 0 {
					state.PendingSteeringInputs = nil
					state.SteeringFinalizing = false
					return a.finishAgent(state)
				}
				if a.options.AcknowledgeSteeringInputs != nil {
					accepted, err = a.options.AcknowledgeSteeringInputs(ctx, state.ExecutionID, fresh)
					if err != nil {
						return a.failTask(state, fmt.Errorf("acknowledge steering inputs: %w", err))
					}
				} else {
					accepted = fresh
				}
			}
		}
		var appendErr error
		state, appendErr = appendSteeringInputs(state, accepted)
		if appendErr != nil {
			return a.failTask(state, appendErr)
		}
		state.PendingSteeringInputs = nil
		state.SteeringFinalizing = false
		state.FinalResponse = nil
		state.TaskError = ""
	}
	ensureTranscript(&state)
	if a.options.GoalForTask != nil && state.GoalID != "" {
		goal, err := a.options.GoalForTask(ctx, state)
		if err != nil {
			return a.failTask(state, err)
		}
		state.Goal = goal
	}
	loadProjectInstructions := !state.ProjectInstructionsReady && a.options.ProjectInstructionsForTask != nil
	if loadProjectInstructions {
		instructions, err := a.options.ProjectInstructionsForTask(ctx, state)
		if err != nil {
			return a.failTask(state, fmt.Errorf("load workspace project instructions: %w", err))
		}
		state.ProjectInstructions, _ = mergeProjectInstructions(state.ProjectInstructions, instructions)
		state.ProjectInstructionsReady = true
	}
	requestCtx := ctx
	cancelRequest := func() {}
	if state.Execution.ModelTimeout > 0 {
		requestTimeout := state.Execution.ModelTimeout * 9 / 10
		if requestTimeout <= 0 {
			requestTimeout = state.Execution.ModelTimeout
		}
		requestCtx, cancelRequest = context.WithTimeout(ctx, requestTimeout)
	}
	defer cancelRequest()
	usagePurpose := model.UsagePurposeMain
	if state.TeamBranch {
		usagePurpose = model.UsagePurposeTeam
	}
	requestCtx = model.WithUsagePurpose(requestCtx, usagePurpose)
	requester := a.requester
	if state.TeamBranch {
		if !state.TeamStarted {
			state.TeamStarted = true
			if a.options.OnBranchStarted != nil {
				a.options.OnBranchStarted(ctx, state)
			}
		}
		if a.options.Team == nil {
			return a.failTask(state, errors.New("team executor is unavailable"))
		}
		var err error
		requester, err = a.options.Team.Requester(ctx, state)
		if err != nil {
			return a.failTask(state, err)
		}
	} else {
		if state.ModelProfile != "" && state.ModelProfile != a.options.Profile {
			return a.failTask(state, fmt.Errorf("model profile changed from %q to %q; the paused task remains pinned to its original profile", state.ModelProfile, a.options.Profile))
		}
		if state.Provider != "" && state.Provider != a.options.Provider || state.Model != "" && state.Model != a.options.Model {
			return a.failTask(state, errors.New("provider/model changed; the paused task remains pinned to its original profile"))
		}
		if state.Protocol == "" {
			state.Protocol = a.options.Protocol
			state.ServerCompaction = a.options.ServerCompaction
			state.SupportsInputAudio = a.options.SupportsInputAudio
		} else if state.Protocol != a.options.Protocol || state.ServerCompaction != a.options.ServerCompaction || state.SupportsInputAudio != a.options.SupportsInputAudio {
			return a.failTask(state, errors.New("provider protocol or compaction capability changed; the paused task remains pinned to its original configuration"))
		}
	}
	if state.Transcript.Items == nil && state.Transcript.Text == nil {
		state.Transcript = *model.NewResponseInputItems(state.History.Items...)
	}
	permissionMode := state.PermissionOverride
	if permissionMode == "" && a.options.CurrentPermissionMode != nil {
		permissionMode = a.options.CurrentPermissionMode()
	}
	visibleTools, visibleNames, err := a.visibleTools(state, permissionMode)
	if err != nil {
		return a.failTask(state, err)
	}
	requestInput := model.NewResponseInputItems(state.History.Items...)
	request := model.ModelRequest{
		Model: state.Model,
		Input: requestInput,
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
	state = a.prepareHostContext(ctx, state, permissionMode, visibleNames)
	request.Input = model.NewResponseInputItems(state.History.Items...)
	request.Instructions = stableInstructions
	compactionThreshold := int64(0)
	serverCompactionRequested := false
	forkCachePrefixValid := true
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
			return a.failOrPauseModel(ctx, state, errors.New("automatic compaction has no available input-token budget"))
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
		} else {
			var compacted bool
			state, request, compacted, err = a.prepareCompaction(requestCtx, state, request, "threshold", false)
			forkCachePrefixValid = !compacted
			if compacted {
				state.TeamForked = false
				state.TeamForkPrefixItems = 0
			}
			if err != nil {
				if ctx.Err() != nil {
					return graph.Transition[State]{}, ctx.Err()
				}
				return a.failOrPauseModel(ctx, state, fmt.Errorf("automatic context compaction: %w", err))
			}
		}
	}

	a.options.Logger.InfoContext(ctx, "agent model request started",
		"agent_id", state.AgentID,
		"provider", state.Provider,
		"model", request.Model,
		"model_override", state.ModelOverride,
		"reasoning_effort", state.ReasoningEffort,
		"request", state.ToolRounds+1,
		"tool_rounds_completed", state.ToolRounds,
		"discovered_categories", state.SelectedCategories,
		"visible_tools", visibleNames,
	)
	sendIndex := 0
	sendRequest := func(request *model.ModelRequest) (*model.ModelResponse, error) {
		request.PromptCache = requestPromptCacheHint(state, *request, visibleNames, forkCachePrefixValid)
		sendIndex++
		requestID := fmt.Sprintf("%s:%s:%d", state.ExecutionID, call.CallID, sendIndex)
		trace := traceRequest(state, *request, requestID, permissionMode)
		state.LastRequest = trace
		var emit func(model.ModelStreamEvent) error
		if a.options.StreamObserverFor != nil || a.options.StreamObserver != nil {
			emit = func(event model.ModelStreamEvent) error {
				event.RequestID = requestID
				event.Purpose = string(usagePurpose)
				if a.options.StreamObserverFor != nil {
					a.options.StreamObserverFor(state, event)
				} else {
					a.options.StreamObserver(event)
				}
				return nil
			}
		}
		return a.sendModel(model.WithRequestTrace(requestCtx, trace), state, requester, *request, emit)
	}
	response, responseErr := sendRequest(&request)
	if serverCompactionRequested && compactOptionUnsupported(responseErr) && !streamFailure(responseErr) {
		state.ServerCompactionFallback = true
		a.notifyCompaction(ContextCompactionEvent{
			Stage: "fallback", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold,
			Error: errors.New("provider rejected context_management; switching this task to local compaction"),
		})
		request.ContextManagement = nil
		var compacted bool
		state, request, compacted, err = a.prepareCompaction(requestCtx, state, request, "threshold", false)
		forkCachePrefixValid = !compacted
		if compacted {
			state.TeamForked = false
			state.TeamForkPrefixItems = 0
		}
		if err != nil {
			if ctx.Err() != nil {
				return graph.Transition[State]{}, ctx.Err()
			}
			return a.failOrPauseModel(ctx, state, fmt.Errorf("automatic context compaction: %w", err))
		}
		serverCompactionRequested = false
		response, responseErr = sendRequest(&request)
	}
	if responseErr != nil && model.IsContextWindowError(responseErr) && state.OverflowCompactions < 1 {
		// The provider rejected the request as too large: compact once with an
		// overflow budget, then retry the same request a single time.
		state.OverflowCompactions++
		if serverCompactionRequested {
			state.ServerCompactionFallback = true
			request.ContextManagement = nil
			serverCompactionRequested = false
		}
		var compacted bool
		var compactErr error
		state, request, compacted, compactErr = a.prepareCompaction(requestCtx, state, request, "overflow", true)
		forkCachePrefixValid = !compacted
		if compacted {
			state.TeamForked = false
			state.TeamForkPrefixItems = 0
		}
		if compactErr != nil {
			if ctx.Err() != nil {
				return graph.Transition[State]{}, ctx.Err()
			}
			return a.failOrPauseModel(ctx, state, fmt.Errorf("context overflow recovery: %w", compactErr))
		}
		if compacted {
			response, responseErr = sendRequest(&request)
		}
	}
	if responseErr != nil {
		a.options.Logger.ErrorContext(ctx, "agent model request failed",
			"agent_id", state.AgentID,
			"provider", state.Provider,
			"model", request.Model,
			"error", responseErr,
		)
		if serverCompactionRequested && (model.EstimateInputTokens(request) >= compactionThreshold || (state.LastInputTokens > 0 && state.LastInputTokens >= compactionThreshold)) {
			a.notifyCompaction(ContextCompactionEvent{Stage: "failed", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold, Error: responseErr})
		}
		if errors.Is(responseErr, model.ErrTokenBudgetExceeded) {
			return graph.Wait(state, TokenBudgetContinuation, a.tokenBudgetRetryNode(state)), nil
		}
		if ctx.Err() != nil {
			return graph.Transition[State]{}, responseErr
		}
		return a.failOrPauseModel(ctx, state, responseErr)
	}
	if response == nil {
		return a.failOrPauseModel(ctx, state, errors.New("requester returned a nil response"))
	}

	return a.handleModelResponse(ctx, state, request, response, visibleNames, compactionThreshold, serverCompactionRequested)
}

func (a *Agent) tokenBudgetRetryNode(state State) string {
	if state.TeamBranch {
		return teamModelNode
	}
	return modelNode
}
