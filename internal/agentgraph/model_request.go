package agentgraph

import (
	"context"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) requestModel(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	ensureTranscript(&state)
	loadProjectInstructions := !state.ProjectInstructionsReady &&
		state.AcceptancePhase != "criteria" && state.AcceptancePhase != "acceptance" &&
		a.options.ProjectInstructionsForTask != nil
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
	switch state.AcceptancePhase {
	case "criteria":
		usagePurpose = model.UsagePurposeCriteria
	case "acceptance":
		usagePurpose = model.UsagePurposeAcceptance
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
	var visibleTools []model.ResponseTool
	var visibleNames []string
	var err error
	switch state.AcceptancePhase {
	case "criteria":
		visibleTools = []model.ResponseTool{criteriaDefinition()}
		visibleNames = []string{submitCriteriaToolName}
		if a.options.AskUser && !state.NoInteractiveDecisions && !state.TeamBranch {
			visibleTools = append(visibleTools, askUserDefinition())
			visibleNames = append(visibleNames, askUserToolName)
		}
	case "acceptance":
		visibleTools = []model.ResponseTool{acceptanceDefinition(), requestEvidenceDefinition()}
		visibleNames = []string{submitAcceptanceToolName, requestEvidenceToolName}
	default:
		visibleTools, visibleNames, err = a.visibleTools(state, state.SelectedCategory, permissionMode)
		if err != nil {
			return a.failTask(state, err)
		}
	}
	requestInput := model.NewResponseInputItems(state.History.Items...)
	if state.AcceptancePhase == "criteria" || state.AcceptancePhase == "acceptance" {
		var inputErr error
		requestInput, inputErr = acceptancePhaseInput(state)
		if inputErr != nil {
			return a.failTask(state, inputErr)
		}
	}
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
	request.Instructions = taskInstructions(state.Goal, permissionMode)
	switch state.AcceptancePhase {
	case "criteria":
		request.Instructions = acceptanceCriteriaInstructions(state)
	case "acceptance":
		request.Instructions = acceptanceReviewInstructions(state)
	default:
		if state.AcceptanceEvidenceRequest != "" {
			request.Instructions += "\n\nThe acceptance reviewer requested more evidence before deciding: " + state.AcceptanceEvidenceRequest
			state.AcceptanceEvidenceRequest = ""
		}
		if !state.TeamBranch && a.options.MemorySummaryFor != nil {
			if summary, summaryErr := a.options.MemorySummaryFor(ctx, state); summaryErr != nil {
				a.options.Logger.WarnContext(ctx, "load workspace memory summary", "error_type", fmt.Sprintf("%T", summaryErr))
			} else if summary != "" {
				request.Instructions += "\n\n" + summary
			}
		}
	}
	if state.AcceptancePhase != "criteria" && state.AcceptancePhase != "acceptance" {
		if personalContext := personalInstructionContext(state.PersonalInstructions); personalContext != "" {
			request.Instructions += "\n\n" + personalContext
		}
		if projectContext := projectInstructionContext(state.ProjectInstructions); projectContext != "" {
			request.Instructions += "\n\n" + projectContext
		}
	}
	if state.NoInteractiveDecisions {
		request.Instructions += "\n\nHost approved this plan. Do not ask the user or seek new permission. Stop and report if an unapproved action is required."
	}
	if state.AcceptancePhase == "criteria" {
		auto := "auto"
		request.ToolChoice = &model.ToolChoice{Mode: &auto}
	}
	if state.AcceptancePhase == "acceptance" {
		auto := "auto"
		request.ToolChoice = &model.ToolChoice{Mode: &auto}
	}
	if a.options.CommandSummary != nil && state.AcceptancePhase != "criteria" && state.AcceptancePhase != "acceptance" {
		summary, err := a.options.CommandSummary(ctx, state)
		if err != nil {
			return a.failTask(state, err)
		}
		if summary != "" {
			request.Instructions += "\n\n" + summary
		}
	}
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
			a.notifyCompaction(ContextCompactionEvent{
				Stage: "started", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold,
			})
		} else {
			state, request, _, err = a.prepareAutomaticCompaction(requestCtx, state, request)
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
		"selected_category", state.SelectedCategory,
		"visible_tools", visibleNames,
	)
	sendRequest := func(request model.ModelRequest) (*model.ModelResponse, error) {
		var emit func(model.ModelStreamEvent) error
		if a.options.StreamObserverFor != nil || a.options.StreamObserver != nil {
			emit = func(event model.ModelStreamEvent) error {
				event.RequestID = call.CallID
				event.Purpose = string(usagePurpose)
				if a.options.StreamObserverFor != nil {
					a.options.StreamObserverFor(state, event)
				} else {
					a.options.StreamObserver(event)
				}
				return nil
			}
		}
		return a.sendModel(requestCtx, state, requester, request, emit)
	}
	response, responseErr := sendRequest(request)
	if serverCompactionRequested && compactOptionUnsupported(responseErr) && !streamFailure(responseErr) {
		state.ServerCompactionFallback = true
		a.notifyCompaction(ContextCompactionEvent{
			Stage: "fallback", Mode: "server", BeforeTokens: model.EstimateInputTokens(request), Threshold: compactionThreshold,
			Error: errors.New("provider rejected context_management; switching this task to local compaction"),
		})
		request.ContextManagement = nil
		state, request, _, err = a.prepareAutomaticCompaction(requestCtx, state, request)
		if err != nil {
			if ctx.Err() != nil {
				return graph.Transition[State]{}, ctx.Err()
			}
			return a.failOrPauseModel(ctx, state, fmt.Errorf("automatic context compaction: %w", err))
		}
		serverCompactionRequested = false
		response, responseErr = sendRequest(request)
	}
	if responseErr != nil {
		if serverCompactionRequested {
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
	switch state.AcceptancePhase {
	case "criteria":
		return criteriaNode
	case "acceptance":
		return acceptanceNode
	default:
		return modelNode
	}
}
