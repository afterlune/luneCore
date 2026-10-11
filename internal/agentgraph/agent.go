// Package agentgraph runs the existing model and tool interfaces on luneGraph.
package agentgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
)

const (
	modelNode = "model_request"
	toolNode  = "execute_tool"
	machineID = "lune-agent-v1"
)

// CurrentTaskSettingsVersion identifies the task-settings snapshot stored in checkpoints.
const CurrentTaskSettingsVersion = 3

// Requester sends one Responses API request without managing history or tools.
type Requester interface {
	CreateResponse(context.Context, model.ModelRequest) (*model.ModelResponse, error)
}

type StreamingRequester interface {
	StreamResponse(context.Context, model.ModelRequest, func(model.ModelStreamEvent) error) (*model.ModelResponse, error)
}

type TeamAssignment struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Task  string `json:"task"`
}

type TeamMemberResult struct {
	ID       string
	Model    string
	Provider string
	Text     string
	Error    string
}

type TeamExecutor interface {
	Profiles() map[string]model.ModelProfile
	MaxParallelAgents() int
	Run(context.Context, string, []TeamAssignment, TaskOptions) ([]TeamMemberResult, error)
}

type AgentOptions struct {
	ID                 string
	Name               string
	Profile            string
	Provider           string
	Protocol           string
	ServerCompaction   bool
	SupportsInputAudio bool
	Model              string
	Team               TeamExecutor
	StreamObserver     func(model.ModelStreamEvent)
	CompactionObserver func(ContextCompactionEvent)
}

type ContextCompactionEvent struct {
	Stage        string
	Mode         string
	BeforeTokens int64
	AfterTokens  int64
	Threshold    int64
	Error        error
}

// State is the resumable state for one agent task. History uses ResponseInput's
// custom JSON decoder so typed and unknown multimodal input items survive clone
// and checkpoint round trips.
type State struct {
	RunID string `json:"run_id,omitempty"`
	// ExecutionID is stable across resume attempts and namespaces tool calls.
	ExecutionID              string                     `json:"execution_id,omitempty"`
	AgentID                  string                     `json:"agent_id,omitempty"`
	AgentName                string                     `json:"agent_name,omitempty"`
	Provider                 string                     `json:"provider,omitempty"`
	Protocol                 string                     `json:"protocol,omitempty"`
	ServerCompaction         bool                       `json:"server_compaction,omitempty"`
	SupportsInputAudio       bool                       `json:"supports_input_audio"`
	ServerCompactionFallback bool                       `json:"server_compaction_fallback,omitempty"`
	ModelProfile             string                     `json:"model_profile,omitempty"`
	Model                    string                     `json:"model,omitempty"`
	ModelOverride            string                     `json:"model_override,omitempty"`
	ReasoningEffort          string                     `json:"reasoning_effort,omitempty"`
	TaskSettingsVersion      int                        `json:"task_settings_version,omitempty"`
	Goal                     string                     `json:"goal,omitempty"`
	ContextWindow            int64                      `json:"context_window,omitempty"`
	ReserveOutputTokens      int64                      `json:"reserve_output_tokens,omitempty"`
	AutoCompact              bool                       `json:"auto_compact,omitempty"`
	AutoCompactThreshold     float64                    `json:"auto_compact_threshold,omitempty"`
	History                  model.ResponseInput        `json:"history"`
	Transcript               model.ResponseInput        `json:"transcript,omitempty"`
	CompactionWarningEmitted bool                       `json:"compaction_warning_emitted,omitempty"`
	PlanMode                 bool                       `json:"plan_mode,omitempty"`
	SelectedCategory         string                     `json:"selected_category,omitempty"`
	PendingCalls             []model.OutputFunctionCall `json:"pending_calls,omitempty"`
	PendingIndex             int                        `json:"pending_index,omitempty"`
	PendingInvocationID      string                     `json:"pending_invocation_id,omitempty"`
	VisibleTools             []string                   `json:"visible_tools,omitempty"`
	ToolRounds               int                        `json:"tool_rounds,omitempty"`
	MaxToolRounds            int                        `json:"max_tool_rounds,omitempty"`
	TaskError                string                     `json:"task_error,omitempty"`
	FinalResponse            *model.Response            `json:"final_response,omitempty"`
}

// TaskOptions configures one user task. A zero tool-round limit is unlimited.
type TaskOptions struct {
	ExecutionID   string
	MaxToolRounds int
	PlanMode      bool
	// Model optionally overrides the configured profile's model identifier for this task.
	Model                string
	ReasoningEffort      string
	Goal                 string
	ContextWindow        int64
	ReserveOutputTokens  int64
	AutoCompact          bool
	AutoCompactThreshold float64
}

// Agent runs one model request or one tool call per graph step.
type Agent struct {
	runner      *graph.Runner[State]
	requester   Requester
	tools       model.ToolExecutor
	categorized model.CategorizedToolExecutor
	categories  []model.ToolCategory
	options     AgentOptions
}

var _ Requester = (*model.Client)(nil)

func New(requester Requester, tools model.ToolExecutor) (*Agent, error) {
	return NewWithOptions(requester, tools, AgentOptions{
		ID: "lead", Name: "Lead", Profile: model.DeepSeekFlashModel,
		Provider: model.ProviderDeepSeek, Model: model.DeepSeekFlashModel,
	})
}

func NewWithOptions(requester Requester, tools model.ToolExecutor, options AgentOptions) (*Agent, error) {
	if requester == nil {
		return nil, errors.New("requester is required")
	}
	if tools == nil {
		return nil, errors.New("tool executor is required")
	}
	if options.ID == "" {
		options.ID = "lead"
	}
	if options.Name == "" {
		options.Name = options.ID
	}
	if options.Model == "" {
		options.Model = model.DeepSeekFlashModel
	}
	if options.Profile == "" {
		options.Profile = options.Model
	}

	agent := &Agent{requester: requester, tools: tools, options: options}
	if categorized, ok := tools.(model.CategorizedToolExecutor); ok {
		agent.categories = categorized.Categories()
		if len(agent.categories) > 0 {
			agent.categorized = categorized
		}
	}

	workflow := graph.New[State](modelNode)
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: modelNode, Run: agent.requestModel}); err != nil {
		return nil, fmt.Errorf("add model node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: toolNode, Run: agent.executeTool}); err != nil {
		return nil, fmt.Errorf("add tool node: %w", err)
	}
	if err := workflow.AddEdge(modelNode, toolNode); err != nil {
		return nil, fmt.Errorf("add model-to-tool edge: %w", err)
	}
	if err := workflow.AddEdge(toolNode, toolNode); err != nil {
		return nil, fmt.Errorf("add tool-to-tool edge: %w", err)
	}
	if err := workflow.AddEdge(toolNode, modelNode); err != nil {
		return nil, fmt.Errorf("add tool-to-model edge: %w", err)
	}
	runner, err := workflow.Compile(graph.Config[State]{MachineID: machineID, Clone: graph.JSONClone[State]})
	if err != nil {
		return nil, fmt.Errorf("compile agent graph: %w", err)
	}
	agent.runner = runner
	return agent, nil
}

func (a *Agent) Start(ctx context.Context, runID string, history, input *model.ResponseInput, taskOptions TaskOptions, options graph.Options[State]) (graph.Result[State], error) {
	return a.StartWithTranscript(ctx, runID, history, history, input, taskOptions, options)
}

// StartWithTranscript starts an agent with a provider-specific request context
// and a portable conversation transcript kept separately for display and
// rebuilding context after a provider change.
func (a *Agent) StartWithTranscript(ctx context.Context, runID string, history, transcript, input *model.ResponseInput, taskOptions TaskOptions, options graph.Options[State]) (graph.Result[State], error) {
	if taskOptions.MaxToolRounds < 0 {
		return graph.Result[State]{Status: graph.StatusFailed}, errors.New("MaxToolRounds must not be negative")
	}
	state, err := initialState(history, transcript, input)
	if err != nil {
		return graph.Result[State]{Status: graph.StatusFailed}, err
	}
	state.RunID = runID
	state.ExecutionID = taskOptions.ExecutionID
	if state.ExecutionID == "" {
		state.ExecutionID = runID
	}
	state.AgentID = a.options.ID
	state.AgentName = a.options.Name
	state.Provider = a.options.Provider
	state.Protocol = a.options.Protocol
	state.ServerCompaction = a.options.ServerCompaction
	state.SupportsInputAudio = a.options.SupportsInputAudio
	state.ModelProfile = a.options.Profile
	state.Model = a.options.Model
	state.ModelOverride = strings.TrimSpace(taskOptions.Model)
	state.ReasoningEffort = taskOptions.ReasoningEffort
	state.MaxToolRounds = taskOptions.MaxToolRounds
	state.PlanMode = taskOptions.PlanMode
	state.TaskSettingsVersion = CurrentTaskSettingsVersion
	state.Goal = taskOptions.Goal
	state.ContextWindow = taskOptions.ContextWindow
	state.ReserveOutputTokens = taskOptions.ReserveOutputTokens
	state.AutoCompact = taskOptions.AutoCompact
	state.AutoCompactThreshold = taskOptions.AutoCompactThreshold
	return a.runner.Start(ctx, runID, state, options)
}

func (a *Agent) Resume(ctx context.Context, checkpoint graph.Checkpoint[State], inputs []graph.ResumeInput, options graph.Options[State]) (graph.Result[State], error) {
	return a.runner.Resume(ctx, checkpoint, inputs, options)
}

func (a *Agent) Recover(ctx context.Context, runID string, inputs []graph.ResumeInput, options graph.Options[State]) (graph.Result[State], error) {
	return a.runner.Recover(ctx, runID, inputs, options)
}

func initialState(history, transcript, input *model.ResponseInput) (State, error) {
	if input == nil {
		return State{}, errors.New("input is nil")
	}
	items, err := responseInputItems(input)
	if err != nil {
		return State{}, err
	}
	if len(items) == 0 {
		return State{}, errors.New("input item list is empty")
	}
	previous, err := responseInputItemsOrEmpty(history)
	if err != nil {
		return State{}, fmt.Errorf("decode model context: %w", err)
	}
	transcriptItems, err := responseInputItemsOrEmpty(transcript)
	if err != nil {
		return State{}, fmt.Errorf("decode transcript: %w", err)
	}
	previous = append(previous, items...)
	transcriptItems = append(transcriptItems, items...)
	return State{
		History:    *model.NewResponseInputItems(previous...),
		Transcript: *model.NewResponseInputItems(transcriptItems...),
	}, nil
}

func responseInputItemsOrEmpty(input *model.ResponseInput) ([]model.InputItem, error) {
	if input == nil {
		return nil, nil
	}
	if input.Text == nil && input.Items == nil {
		return nil, nil
	}
	if input.Text != nil && input.Items != nil {
		return nil, errors.New("conversation cannot contain both text and items")
	}
	if input.Text != nil {
		return []model.InputItem{model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(*input.Text)}}, nil
	}
	return append([]model.InputItem(nil), input.Items...), nil
}

func responseInputItems(input *model.ResponseInput) ([]model.InputItem, error) {
	if input.Text != nil && input.Items != nil {
		return nil, errors.New("responses input cannot contain both text and items")
	}
	if input.Text != nil {
		return []model.InputItem{model.InputMessage{
			Role:    model.RoleUser,
			Content: model.NewMessageText(*input.Text),
		}}, nil
	}
	if len(input.Items) == 0 {
		return nil, errors.New("input item list is empty")
	}
	return append([]model.InputItem(nil), input.Items...), nil
}

func (a *Agent) requestModel(ctx context.Context, _ graph.CallInfo, state State) (graph.Transition[State], error) {
	ensureTranscript(&state)
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
	if state.Transcript.Items == nil && state.Transcript.Text == nil {
		state.Transcript = *model.NewResponseInputItems(state.History.Items...)
	}
	visibleTools, visibleNames, err := a.visibleTools(state.SelectedCategory, state.PlanMode)
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
		if streaming, ok := a.requester.(StreamingRequester); ok {
			return streaming.StreamResponse(ctx, request, func(event model.ModelStreamEvent) error {
				if a.options.StreamObserver != nil {
					a.options.StreamObserver(event)
				}
				return nil
			})
		}
		return a.requester.CreateResponse(ctx, request)
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
		return graph.EndExecution(state), nil
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
		return graph.EndExecution(state), nil
	}
	state.PendingCalls = calls
	state.PendingIndex = 0
	state.PendingInvocationID = pendingToolInvocationID(state.ExecutionID, state.RunID, state.AgentID, state.ToolRounds, state.PendingIndex, calls[0].CallID)
	state.VisibleTools = visibleNames
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
	return graph.EndExecution(state), nil
}

func (a *Agent) executeTool(ctx context.Context, _ graph.CallInfo, state State) (graph.Transition[State], error) {
	ensureTranscript(&state)
	if state.PendingIndex < 0 || state.PendingIndex >= len(state.PendingCalls) {
		return graph.Transition[State]{}, errors.New("pending tool-call index is out of range")
	}
	call := state.PendingCalls[state.PendingIndex]
	var output model.InputFunctionCallOutput
	switch {
	case call.Name == teamDispatchToolName && a.options.Team != nil:
		assignments, err := parseTeamAssignments(call.Arguments, a.options.Team)
		if err != nil {
			output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Team request rejected: " + err.Error())}
		} else {
			results, err := a.options.Team.Run(ctx, state.RunID+":"+state.ExecutionID+":"+call.CallID, assignments, TaskOptions{
				ExecutionID: state.ExecutionID, MaxToolRounds: state.MaxToolRounds,
				PlanMode: state.PlanMode, ReasoningEffort: state.ReasoningEffort,
			})
			if err != nil {
				return graph.Transition[State]{}, err
			}
			output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(formatTeamResults(results))}
		}
	case a.categorized != nil && call.Name == model.CategorySelectorName:
		categories := a.categories
		if state.PlanMode {
			categories = a.readOnlyCategories()
		}
		category, err := model.ParseCategorySelection(call.Arguments, categories)
		text := "Category selected. Its tools will be available in the next request."
		if err != nil {
			slog.WarnContext(ctx, "tool category selection rejected",
				"selected_category", state.SelectedCategory,
				"error_type", fmt.Sprintf("%T", err),
			)
			text = "Category was not changed: " + err.Error()
		} else {
			state.SelectedCategory = category
			slog.InfoContext(ctx, "tool category selected", "selected_category", category)
		}
		output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(text)}
	case state.PlanMode && !isReadOnlyTool(a.tools, call.Name):
		output = model.InputFunctionCallOutput{
			CallID: call.CallID,
			Output: model.NewToolOutputText("Tool is not available in plan mode."),
		}
	case !contains(state.VisibleTools, call.Name):
		expectedCategory := a.categoryForTool(call.Name)
		slog.WarnContext(ctx, "tool call is not in the visible tool set",
			"tool", call.Name,
			"selected_category", state.SelectedCategory,
			"visible_tools", state.VisibleTools,
			"expected_category", expectedCategory,
		)
		output = model.InputFunctionCallOutput{
			CallID: call.CallID,
			Output: model.NewToolOutputText(unavailableToolMessage(call.Name, expectedCategory, a.categorized != nil)),
		}
	default:
		toolContext := model.WithAgentID(ctx, state.AgentID)
		toolContext = model.WithInputAudioSupport(toolContext, state.SupportsInputAudio)
		identity := publictool.CallIdentityFromContext(toolContext)
		identity.RunID = state.RunID
		identity.ExecutionID = state.ExecutionID
		identity.AgentID = state.AgentID
		identity.InvocationID = state.PendingInvocationID
		toolContext = publictool.WithCallIdentity(toolContext, identity)
		if recoverable, ok := a.tools.(interface {
			ExecuteCallRecoverable(context.Context, model.OutputFunctionCall) (model.InputFunctionCallOutput, error)
		}); ok {
			var err error
			output, err = recoverable.ExecuteCallRecoverable(toolContext, call)
			if err != nil {
				return graph.Transition[State]{}, err
			}
		} else {
			output = a.tools.ExecuteCall(toolContext, call)
		}
	}

	transcriptOnly := output.TranscriptOnly
	output.TranscriptOnly = nil
	state.History.Items = append(state.History.Items, output)
	state.Transcript.Items = append(state.Transcript.Items, output)
	if len(transcriptOnly) > 0 {
		state.Transcript.Items = append(state.Transcript.Items, model.InputMessage{
			Role: model.RoleAssistant, Content: model.NewMessageContentParts(transcriptOnly...),
		})
	}
	state.PendingIndex++
	if state.PendingIndex < len(state.PendingCalls) {
		state.PendingInvocationID = pendingToolInvocationID(state.ExecutionID, state.RunID, state.AgentID, state.ToolRounds, state.PendingIndex, state.PendingCalls[state.PendingIndex].CallID)
		return graph.To(state, toolNode), nil
	}
	state.ToolRounds++
	state.PendingCalls = nil
	state.PendingIndex = 0
	state.PendingInvocationID = ""
	state.VisibleTools = nil
	return graph.To(state, modelNode), nil
}

func pendingToolInvocationID(executionID, runID, agentID string, toolRound, pendingIndex int, callID string) string {
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%s", executionID, runID, agentID, toolRound, pendingIndex, callID)
	digest := sha256.Sum256([]byte(identity))
	return "tool_" + hex.EncodeToString(digest[:])
}

func ensureTranscript(state *State) {
	if state == nil || state.Transcript.Items != nil || state.Transcript.Text != nil {
		return
	}
	if state.History.Text != nil {
		state.Transcript = *model.NewResponseInputText(*state.History.Text)
		return
	}
	state.Transcript = *model.NewResponseInputItems(state.History.Items...)
}

func (a *Agent) visibleTools(category string, planMode bool) ([]model.ResponseTool, []string, error) {
	addTeamTool := func(definitions []model.ResponseTool) []model.ResponseTool {
		if a.options.Team == nil || len(a.options.Team.Profiles()) == 0 || a.options.Team.MaxParallelAgents() < 1 {
			return definitions
		}
		return append(definitions, teamDispatchDefinition(a.options.Team))
	}
	if a.categorized == nil {
		definitions := a.tools.Definitions()
		if planMode {
			definitions = readOnlyDefinitions(a.tools, definitions)
		}
		definitions = addTeamTool(definitions)
		return definitions, toolNames(definitions), nil
	}
	categories := a.categories
	if planMode {
		categories = a.readOnlyCategories()
	}
	selector, err := model.CategorySelectorDefinition(categories)
	if err != nil {
		return nil, nil, fmt.Errorf("build category selector: %w", err)
	}
	definitions := []model.ResponseTool{selector}
	if category != "" {
		definitions = append(definitions, a.categorized.DefinitionsForCategory(category)...)
		if planMode {
			definitions = readOnlyDefinitions(a.tools, definitions)
			definitions = append([]model.ResponseTool{selector}, definitions...)
		}
	}
	definitions = addTeamTool(definitions)
	return definitions, toolNames(definitions), nil
}

func (a *Agent) readOnlyCategories() []model.ToolCategory {
	var categories []model.ToolCategory
	for _, category := range a.categories {
		if len(readOnlyDefinitions(a.tools, a.categorized.DefinitionsForCategory(category.Name))) > 0 {
			categories = append(categories, category)
		}
	}
	return categories
}

func (a *Agent) categoryForTool(name string) string {
	if a.categorized == nil {
		return ""
	}
	for _, category := range a.categories {
		for _, definition := range a.categorized.DefinitionsForCategory(category.Name) {
			if function, ok := definition.(model.FunctionTool); ok && function.Name == name {
				return category.Name
			}
		}
	}
	return ""
}

func unavailableToolMessage(name, category string, categorized bool) string {
	if category != "" {
		return fmt.Sprintf("Tool %q belongs to category %q, which is not visible in this request. Call %s with category %q, then retry the tool in the next request.",
			name, category, model.CategorySelectorName, category)
	}
	if categorized {
		return fmt.Sprintf("Tool %q is not registered in a discoverable category. Call %s to review available tools, then use a listed tool.",
			name, model.CategorySelectorName)
	}
	return fmt.Sprintf("Tool %q is not available in this request. Only call tools listed in the current request.", name)
}

func readOnlyDefinitions(tools model.ToolExecutor, definitions []model.ResponseTool) []model.ResponseTool {
	filtered := make([]model.ResponseTool, 0, len(definitions))
	for _, definition := range definitions {
		function, ok := definition.(model.FunctionTool)
		if ok && isReadOnlyTool(tools, function.Name) {
			filtered = append(filtered, definition)
		}
	}
	return filtered
}

func isReadOnlyTool(tools model.ToolExecutor, name string) bool {
	metadata, ok := tools.ToolMetadata(name)
	return ok && metadata.IsReadOnly()
}

func appendResponseHistory(history model.ResponseInput, output []model.ResponseOutputItem) (model.ResponseInput, error) {
	items := append([]model.InputItem(nil), history.Items...)
	for _, item := range output {
		encoded, err := json.Marshal(item)
		if err != nil {
			return model.ResponseInput{}, fmt.Errorf("encode response history: %w", err)
		}
		items = append(items, model.UnknownInputItem{Raw: encoded})
	}
	return *model.NewResponseInputItems(items...), nil
}

func responseFunctionCalls(items []model.ResponseOutputItem) []model.OutputFunctionCall {
	calls := make([]model.OutputFunctionCall, 0)
	for _, item := range items {
		if call, ok := item.(model.OutputFunctionCall); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

func toolNames(definitions []model.ResponseTool) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		if function, ok := definition.(model.FunctionTool); ok {
			names = append(names, function.Name)
		}
	}
	return names
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

const teamDispatchToolName = "delegate_team"

type teamDispatchRequest struct {
	Tasks []TeamAssignment `json:"tasks"`
}

func teamDispatchDefinition(team TeamExecutor) model.FunctionTool {
	profiles := team.Profiles()
	profileIDs := make([]string, 0, len(profiles))
	for profileID := range profiles {
		profileIDs = append(profileIDs, profileID)
	}
	sort.Strings(profileIDs)
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type":     "array",
				"minItems": 1,
				"maxItems": team.MaxParallelAgents(),
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":    map[string]any{"type": "string", "description": "Short unique name for this team member."},
						"model": map[string]any{"type": "string", "enum": profileIDs},
						"task":  map[string]any{"type": "string", "description": "A bounded, independent task for this member."},
					},
					"required":             []string{"id", "model", "task"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"tasks"},
		"additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name: teamDispatchToolName,
		Description: "Delegate independent work to multiple team members at once. Choose each member's model profile from the listed catalog. " +
			"Members have isolated context, share the workspace and current permissions, and return results for you to synthesize. Use this only when tasks can proceed independently.",
		Parameters: parameters,
		Strict:     &strict,
	}
}

func parseTeamAssignments(arguments string, team TeamExecutor) ([]TeamAssignment, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var request teamDispatchRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("expected one JSON object")
	}
	limit := team.MaxParallelAgents()
	if len(request.Tasks) == 0 || len(request.Tasks) > limit {
		return nil, fmt.Errorf("team task count must be between 1 and %d", limit)
	}
	profiles := team.Profiles()
	seen := make(map[string]struct{}, len(request.Tasks))
	for index := range request.Tasks {
		task := &request.Tasks[index]
		task.ID = strings.TrimSpace(task.ID)
		task.Model = strings.TrimSpace(task.Model)
		task.Task = strings.TrimSpace(task.Task)
		if task.ID == "" || task.Task == "" {
			return nil, errors.New("each team task requires a non-empty id and task")
		}
		if _, exists := seen[task.ID]; exists {
			return nil, fmt.Errorf("team member id %q is repeated", task.ID)
		}
		seen[task.ID] = struct{}{}
		if _, exists := profiles[task.Model]; !exists {
			return nil, fmt.Errorf("unknown team model profile %q", task.Model)
		}
	}
	return request.Tasks, nil
}

func formatTeamResults(results []TeamMemberResult) string {
	var output strings.Builder
	output.WriteString("Team results:")
	for _, result := range results {
		output.WriteString("\n\n## ")
		output.WriteString(result.ID)
		output.WriteString(" (model ")
		output.WriteString(result.Model)
		output.WriteString(")\n")
		if result.Error != "" {
			output.WriteString("Failed: ")
			output.WriteString(result.Error)
		} else {
			output.WriteString(result.Text)
		}
	}
	return output.String()
}
