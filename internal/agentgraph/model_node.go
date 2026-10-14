package agentgraph

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
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

func (a *Agent) requestModel(ctx context.Context, _ graph.CallInfo, state State) (graph.Transition[State], error) {
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
		request.Instructions += "\n\nThe host has already approved this plan. Do not ask the user questions or request new permissions. If an unapproved action becomes necessary, stop and report the blocker."
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
		if ctx.Err() != nil {
			return graph.Transition[State]{}, responseErr
		}
		return a.failOrPauseModel(ctx, state, responseErr)
	}
	if response == nil {
		return a.failOrPauseModel(ctx, state, errors.New("requester returned a nil response"))
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
	state.Transcript, err = appendTranscriptOutput(state.Transcript, response.Output)
	if err != nil {
		return a.failTask(state, err)
	}
	calls := responseFunctionCalls(response.Output)
	if len(calls) == 0 {
		switch state.AcceptancePhase {
		case "criteria":
			return a.retryAcceptanceFormat(state, response, criteriaNode, "Submit the task criteria with prepare_task_criteria. Ask the user with ask_user if a key ambiguity blocks a correct scope.")
		case "acceptance":
			return a.retryAcceptanceFormat(state, response, acceptanceNode, "Submit a structured acceptance report with submit_acceptance, or request more evidence with request_acceptance_evidence.")
		default:
			if a.options.ActiveCommands != nil && a.options.ActiveCommands(state) {
				return a.deferCommandCompletion(ctx, state)
			}
			state.CommandFinalizeAttempts = 0
			state.FinalResponse = response
			if a.options.AcceptanceEnabled && !state.TeamBranch {
				if a.options.CommandEvidence != nil {
					evidence, err := a.options.CommandEvidence(ctx, state)
					if err != nil {
						return a.failTask(state, err)
					}
					known := map[string]bool{}
					for _, saved := range state.AcceptanceEvidence {
						known[saved.Reference] = true
					}
					for _, saved := range evidence {
						if !known[saved.Reference] {
							state.AcceptanceEvidence = append(state.AcceptanceEvidence, saved)
							known[saved.Reference] = true
						}
					}
				}
				state.AcceptancePhase = "acceptance"
				state.AcceptanceCandidate = responseText(response.Output)
				state.AcceptanceCandidateCount++
				state.AcceptanceCandidateID = fmt.Sprintf("candidate-%d", state.AcceptanceCandidateCount)
				state.AcceptanceEvidence = append(state.AcceptanceEvidence, modelEvidence("candidate:"+state.AcceptanceCandidateID, state.AcceptanceCandidate))
				a.observeAcceptance(state, "started", "", "")
				return graph.To(state, acceptanceNode), nil
			}
			return a.finishAgent(state)
		}
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
