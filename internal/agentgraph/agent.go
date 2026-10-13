// Package agentgraph runs the existing model and tool interfaces on luneGraph.
package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
)

const (
	modelNode          = "model_request"
	toolNode           = "execute_tool"
	teamModelNode      = "team_model_request"
	teamToolNode       = "team_execute_tool"
	teamJoinNode       = "team_join"
	teamBranchJoinNode = "team_branch_join"
	machineID          = "lune-agent-v2"
)

const UserQuestionContinuation = "user_question"

// CurrentTaskSettingsVersion identifies the task-settings snapshot stored in checkpoints.
const CurrentTaskSettingsVersion = 5

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
	PrepareBranch(context.Context, State, TeamAssignment, TaskOptions) (State, error)
	Requester(context.Context, State) (Requester, error)
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
	Execution          ExecutionOptions
	Team               TeamExecutor
	AskUser            bool
	StreamObserverFor  func(State, model.ModelStreamEvent)
	OnBranchStarted    func(context.Context, State)
	OnBranchFinished   func(context.Context, State, error)
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
	Execution                ExecutionOptions           `json:"execution,omitempty"`
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
	VisibleTools             []string                   `json:"visible_tools,omitempty"`
	ToolRounds               int                        `json:"tool_rounds,omitempty"`
	MaxToolRounds            int                        `json:"max_tool_rounds,omitempty"`
	TaskError                string                     `json:"task_error,omitempty"`
	FinalResponse            *model.Response            `json:"final_response,omitempty"`
	TeamBranch               bool                       `json:"team_branch,omitempty"`
	TeamTaskID               string                     `json:"team_task_id,omitempty"`
	TeamModel                string                     `json:"team_model,omitempty"`
	TeamProvider             string                     `json:"team_provider,omitempty"`
	TeamText                 string                     `json:"team_text,omitempty"`
	TeamError                string                     `json:"team_error,omitempty"`
	TeamStarted              bool                       `json:"team_started,omitempty"`
	TeamRunID                string                     `json:"team_run_id,omitempty"`
	TeamDispatchID           string                     `json:"team_dispatch_id,omitempty"`
	TeamAssignments          []TeamAssignment           `json:"team_assignments,omitempty"`
	TeamMinimumSuccesses     int                        `json:"team_minimum_successes,omitempty"`
	UserQuestion             string                     `json:"user_question,omitempty"`
	UserQuestionOptions      []string                   `json:"user_question_options,omitempty"`
	UserAnswer               string                     `json:"user_answer,omitempty"`
	UserAnswerReady          bool                       `json:"user_answer_ready,omitempty"`
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
	Execution            ExecutionOptions
}

// ExecutionOptions contains the small set of controls that affect one Agent run.
// Zero values retain luneGraph's default or unlimited behavior.
type ExecutionOptions struct {
	MaxSteps         int           `json:"max_steps,omitempty"`
	MaxInvocations   int           `json:"max_invocations,omitempty"`
	MaxConcurrency   int           `json:"max_concurrency,omitempty"`
	ModelTimeout     time.Duration `json:"model_timeout,omitempty"`
	ToolTimeout      time.Duration `json:"tool_timeout,omitempty"`
	JoinTimeout      time.Duration `json:"join_timeout,omitempty"`
	TeamEntryTimeout time.Duration `json:"team_entry_timeout,omitempty"`
	TeamQuorum       int           `json:"team_quorum,omitempty"`
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
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: modelNode, Run: agent.requestLead, OnError: graph.FailExecution, Timeout: options.Execution.ModelTimeout}); err != nil {
		return nil, fmt.Errorf("add model node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: toolNode, Run: agent.executeLeadTool, OnError: graph.FailExecution, Timeout: options.Execution.ToolTimeout}); err != nil {
		return nil, fmt.Errorf("add tool node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: teamModelNode, Run: agent.requestTeam, OnError: graph.FailInvocation, Timeout: options.Execution.ModelTimeout}); err != nil {
		return nil, fmt.Errorf("add team model node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: teamToolNode, Run: agent.executeTeamTool, OnError: graph.FailInvocation, Timeout: options.Execution.ToolTimeout}); err != nil {
		return nil, fmt.Errorf("add team tool node: %w", err)
	}
	if err := workflow.AddJoin(graph.JoinSpec[State]{Name: teamJoinNode, From: toolNode, Merge: agent.mergeTeam, OnError: graph.FailExecution, Timeout: options.Execution.JoinTimeout, CanMerge: agent.teamQuorumReached}); err != nil {
		return nil, fmt.Errorf("add team join: %w", err)
	}
	if err := workflow.AddJoin(graph.JoinSpec[State]{Name: teamBranchJoinNode, From: teamToolNode, Merge: agent.mergeTeam, OnError: graph.FailInvocation, Timeout: options.Execution.JoinTimeout, CanMerge: agent.teamQuorumReached}); err != nil {
		return nil, fmt.Errorf("add team branch join: %w", err)
	}
	if err := graph.RegisterJSONContinuation[State, string](workflow, UserQuestionContinuation, func(_ context.Context, _ graph.CallInfo, state State, answer string) (State, error) {
		state.UserAnswer, state.UserAnswerReady = answer, true
		return state, nil
	}); err != nil {
		return nil, fmt.Errorf("register user question continuation: %w", err)
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
	for _, edge := range [][2]string{
		{toolNode, teamModelNode}, {teamModelNode, teamToolNode}, {teamToolNode, teamToolNode}, {teamToolNode, teamModelNode},
		{teamJoinNode, toolNode}, {teamBranchJoinNode, teamToolNode},
	} {
		if err := workflow.AddEdge(edge[0], edge[1]); err != nil {
			return nil, fmt.Errorf("add %s-to-%s edge: %w", edge[0], edge[1], err)
		}
	}
	runner, err := workflow.Compile(graph.Config[State]{MachineID: machineID, Clone: graph.JSONClone[State]})
	if err != nil {
		return nil, fmt.Errorf("compile agent graph: %w", err)
	}
	agent.runner = runner
	return agent, nil
}

func (a *Agent) Start(ctx context.Context, runID string, history, input *model.ResponseInput, taskOptions TaskOptions, store graph.Store[State], options graph.Options) (*graph.Execution[State], graph.Result[State], error) {
	return a.StartWithTranscript(ctx, runID, history, history, input, taskOptions, store, options)
}

// StartWithTranscript starts an agent with a provider-specific request context
// and a portable conversation transcript kept separately for display and
// rebuilding context after a provider change.
func (a *Agent) StartWithTranscript(ctx context.Context, runID string, history, transcript, input *model.ResponseInput, taskOptions TaskOptions, store graph.Store[State], options graph.Options) (*graph.Execution[State], graph.Result[State], error) {
	if taskOptions.MaxToolRounds < 0 {
		return nil, graph.Result[State]{Status: graph.StatusFailed}, errors.New("MaxToolRounds must not be negative")
	}
	state, err := initialState(history, transcript, input)
	if err != nil {
		return nil, graph.Result[State]{Status: graph.StatusFailed}, err
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
	state.Execution = taskOptions.Execution
	return a.runner.Start(ctx, runID, state, store, options)
}

func (a *Agent) Resume(ctx context.Context, checkpoint graph.Checkpoint[State], inputs []graph.ResumeInput, store graph.Store[State], options graph.Options) (*graph.Execution[State], graph.Result[State], error) {
	return a.runner.Resume(ctx, checkpoint, inputs, store, options)
}

func (a *Agent) Recover(ctx context.Context, runID string, inputs []graph.ResumeInput, store graph.Store[State], options graph.Options) (*graph.Execution[State], graph.Result[State], error) {
	return a.runner.Recover(ctx, runID, inputs, store, options)
}

func (a *Agent) Fork(ctx context.Context, newRunID string, source graph.Checkpoint[State]) (graph.Checkpoint[State], error) {
	return a.runner.Fork(ctx, newRunID, source, nil)
}

func RekeyForkState(state State, runID, executionID string) State {
	oldRunID, oldExecutionID := state.RunID, state.ExecutionID
	state.RunID, state.ExecutionID = runID, executionID
	state.TeamDispatchID = rekeyTeamRunID(state.TeamDispatchID, oldRunID, oldExecutionID, runID, executionID)
	state.TeamRunID = rekeyTeamRunID(state.TeamRunID, oldRunID, oldExecutionID, runID, executionID)
	return state
}

func rekeyTeamRunID(value, oldRunID, oldExecutionID, runID, executionID string) string {
	oldPrefix := oldRunID + ":" + oldExecutionID + ":"
	if strings.HasPrefix(value, oldPrefix) {
		return runID + ":" + executionID + ":" + strings.TrimPrefix(value, oldPrefix)
	}
	return value
}

func (a *Agent) MaxConcurrency() int {
	if a == nil || a.options.Team == nil {
		return 0
	}
	return a.options.Team.MaxParallelAgents()
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

func (a *Agent) executeLeadTool(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.executeTool(ctx, call, state)
}

func (a *Agent) executeTeamTool(ctx context.Context, call graph.CallInfo, state State) (graph.Transition[State], error) {
	return a.executeTool(ctx, call, state)
}

func (a *Agent) executeTool(ctx context.Context, info graph.CallInfo, state State) (graph.Transition[State], error) {
	ensureTranscript(&state)
	if state.UserAnswerReady {
		call := state.PendingCalls[state.PendingIndex]
		output := model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("User's answer: " + state.UserAnswer)}
		state.UserQuestion, state.UserQuestionOptions = "", nil
		state.UserAnswer, state.UserAnswerReady = "", false
		state = appendToolOutput(state, output)
		return a.nextToolTransition(state), nil
	}
	if state.PendingIndex == len(state.PendingCalls) && len(state.PendingCalls) > 0 {
		state.ToolRounds++
		state.PendingCalls = nil
		state.PendingIndex = 0
		state.VisibleTools = nil
		return a.nextModelTransition(state), nil
	}
	if state.PendingIndex < 0 || state.PendingIndex >= len(state.PendingCalls) {
		return graph.Transition[State]{}, errors.New("pending tool-call index is out of range")
	}
	call := state.PendingCalls[state.PendingIndex]
	var output model.InputFunctionCallOutput
	switch {
	case call.Name == askUserToolName && a.options.AskUser && !state.TeamBranch:
		question, options, err := parseUserQuestion(call.Arguments)
		if err != nil {
			output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Question request rejected: " + err.Error())}
		} else {
			state.UserQuestion, state.UserQuestionOptions = question, append([]string(nil), options...)
			return graph.Wait(state, UserQuestionContinuation, toolNode), nil
		}
	case call.Name == teamDispatchToolName && a.options.Team != nil:
		assignments, minimumSuccesses, err := parseTeamAssignments(call.Arguments, a.options.Team)
		if err != nil {
			output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText("Team request rejected: " + err.Error())}
		} else {
			quorum := a.options.Execution.TeamQuorum
			if minimumSuccesses != nil {
				quorum = *minimumSuccesses
			}
			if quorum < 0 || quorum > len(assignments) {
				output = model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(fmt.Sprintf("Team request rejected: minimum success threshold must be between 0 and %d", len(assignments)))}
				break
			}
			state.TeamAssignments = append([]TeamAssignment(nil), assignments...)
			state.TeamMinimumSuccesses = quorum
			state.TeamDispatchID = state.RunID + ":" + state.ExecutionID + ":" + info.CallID
			branches := make([]graph.Branch[State], 0, len(assignments))
			taskOptions := TaskOptions{
				ExecutionID: state.ExecutionID, MaxToolRounds: state.MaxToolRounds,
				PlanMode: state.PlanMode, ReasoningEffort: state.ReasoningEffort,
			}
			for _, assignment := range assignments {
				input, inputErr := model.NewResponseInput(assignment.Task)
				if inputErr != nil {
					return graph.Transition[State]{}, inputErr
				}
				branch, branchErr := initialState(nil, nil, input)
				if branchErr != nil {
					return graph.Transition[State]{}, branchErr
				}
				branch.RunID, branch.ExecutionID = state.RunID, state.ExecutionID
				branch.AgentID = state.AgentID + "/" + assignment.ID
				branch.AgentName, branch.TeamTaskID = assignment.ID, assignment.ID
				branch.TeamModel = assignment.Model
				branch.TeamBranch = true
				branch.TeamRunID, branch.TeamDispatchID = state.TeamDispatchID, ""
				branch.TaskSettingsVersion = CurrentTaskSettingsVersion
				branch.MaxToolRounds, branch.PlanMode = state.MaxToolRounds, state.PlanMode
				branch.Execution = state.Execution
				branch.ReasoningEffort = state.ReasoningEffort
				branch, branchErr = a.options.Team.PrepareBranch(ctx, branch, assignment, taskOptions)
				if branchErr != nil {
					return graph.Transition[State]{}, branchErr
				}
				branches = append(branches, graph.Branch[State]{Target: teamModelNode, State: branch, EntryTimeout: a.options.Execution.TeamEntryTimeout})
			}
			return graph.FanOut(state, branches...), nil
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
		identity.InvocationID = info.CallID
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

	state = appendToolOutput(state, output)
	return a.nextToolTransition(state), nil
}

func appendToolOutput(state State, output model.InputFunctionCallOutput) State {
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
	return state
}

func (a *Agent) nextToolTransition(state State) graph.Transition[State] {
	if state.PendingIndex < len(state.PendingCalls) {
		if state.TeamBranch {
			return graph.To(state, teamToolNode)
		}
		return graph.To(state, toolNode)
	}
	state.ToolRounds++
	state.PendingCalls = nil
	state.PendingIndex = 0
	state.VisibleTools = nil
	return a.nextModelTransition(state)
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

func (a *Agent) mergeTeam(ctx context.Context, _ graph.CallInfo, parent State, branches []graph.BranchResult[State]) (State, error) {
	results := make([]TeamMemberResult, len(branches))
	for index, branch := range branches {
		assignment := TeamAssignment{}
		if index < len(parent.TeamAssignments) {
			assignment = parent.TeamAssignments[index]
		}
		member := TeamMemberResult{ID: assignment.ID, Model: assignment.Model}
		if branch.OK() {
			member = TeamMemberResult{ID: branch.State.TeamTaskID, Model: branch.State.TeamModel, Provider: branch.State.TeamProvider, Text: branch.State.TeamText, Error: branch.State.TeamError}
			if a.options.OnBranchFinished != nil {
				var branchErr error
				if member.Error != "" {
					branchErr = errors.New(member.Error)
				}
				a.options.OnBranchFinished(ctx, branch.State, branchErr)
			}
		} else if branch.Pending() {
			member = TeamMemberResult{ID: assignment.ID, Model: assignment.Model, Error: "cancelled after the minimum success threshold was reached"}
			if profile, ok := a.options.Team.Profiles()[assignment.Model]; ok {
				member.Provider, member.Model = profile.Provider, profile.Model
			}
			if a.options.OnBranchFinished != nil {
				pending := branch.State
				pending.TeamRunID = parent.TeamDispatchID
				a.options.OnBranchFinished(ctx, pending, context.Canceled)
			}
		} else {
			member.Error = branch.Error
			if member.Error == "" {
				member.Error = "team branch ended without a result"
			}
			if profile, ok := a.options.Team.Profiles()[assignment.Model]; ok {
				member.Provider = profile.Provider
				member.Model = profile.Model
			}
			if a.options.OnBranchFinished != nil {
				failed := State{
					RunID: parent.RunID, ExecutionID: parent.ExecutionID, AgentID: parent.AgentID + "/" + assignment.ID,
					AgentName: assignment.ID, TeamTaskID: assignment.ID, TeamBranch: true, TeamStarted: true, TeamRunID: parent.TeamDispatchID,
					TeamModel: member.Model, TeamProvider: member.Provider, Model: member.Model, ModelProfile: assignment.Model,
				}
				a.options.OnBranchFinished(ctx, failed, errors.New(member.Error))
			}
		}
		results[index] = member
	}
	if parent.PendingIndex < 0 || parent.PendingIndex >= len(parent.PendingCalls) {
		return State{}, errors.New("team join lost its originating tool call")
	}
	call := parent.PendingCalls[parent.PendingIndex]
	parent = appendToolOutput(parent, model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(formatTeamResults(results))})
	parent.TeamAssignments, parent.TeamDispatchID = nil, ""
	parent.TeamMinimumSuccesses = 0
	return parent, nil
}

func (a *Agent) teamQuorumReached(parent State, branches []graph.BranchResult[State]) bool {
	if parent.TeamMinimumSuccesses <= 0 {
		return false
	}
	var successful int
	for _, branch := range branches {
		if branch.OK() && branch.State.TeamError == "" {
			successful++
		}
	}
	return successful >= parent.TeamMinimumSuccesses
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

func (a *Agent) visibleTools(state State, category string, planMode bool) ([]model.ResponseTool, []string, error) {
	addRuntimeTools := func(definitions []model.ResponseTool) []model.ResponseTool {
		if a.options.Team != nil && len(a.options.Team.Profiles()) > 0 && a.options.Team.MaxParallelAgents() > 0 {
			definitions = append(definitions, teamDispatchDefinition(a.options.Team))
		}
		if a.options.AskUser && !state.TeamBranch {
			definitions = append(definitions, askUserDefinition())
		}
		return definitions
	}
	if a.categorized == nil {
		definitions := a.tools.Definitions()
		if planMode {
			definitions = readOnlyDefinitions(a.tools, definitions)
		}
		definitions = addRuntimeTools(definitions)
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
	definitions = addRuntimeTools(definitions)
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
const askUserToolName = model.UserQuestionToolName

type userQuestionInput struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

func askUserDefinition() model.FunctionTool {
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "A concise clarification question for the user."},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Use an empty array when no choices are needed. The user may also provide a free-text answer.",
			},
		},
		"required":             []string{"question", "options"},
		"additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name: askUserToolName,
		Description: "Ask the user a concise question only when missing information blocks useful progress. " +
			"Optionally provide answer choices; the user may choose one or reply with free text.",
		Parameters: parameters,
		Strict:     &strict,
	}
}

func parseUserQuestion(arguments string) (string, []string, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var input userQuestionInput
	if err := decoder.Decode(&input); err != nil {
		return "", nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", nil, errors.New("expected one JSON object")
	}
	input.Question = strings.TrimSpace(input.Question)
	if input.Question == "" {
		return "", nil, errors.New("question must not be empty")
	}
	for index := range input.Options {
		input.Options[index] = strings.TrimSpace(input.Options[index])
		if input.Options[index] == "" {
			return "", nil, errors.New("answer options must not be empty")
		}
	}
	return input.Question, input.Options, nil
}

type teamDispatchRequest struct {
	Tasks            []TeamAssignment `json:"tasks"`
	MinimumSuccesses *int             `json:"minimum_successes,omitempty"`
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
			"minimum_successes": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "integer", "minimum": 0},
					map[string]any{"type": "null"},
				},
				"description": "Minimum successful members before results can merge. Use null to inherit the host default; zero waits for every member.",
			},
		},
		"required":             []string{"tasks", "minimum_successes"},
		"additionalProperties": false,
	})
	strict := true
	return model.FunctionTool{
		Name: teamDispatchToolName,
		Description: "Delegate independent work to multiple team members at once. Choose each member's model profile from the listed catalog. " +
			"Members have isolated context, share the workspace and current permissions, and return results for you to synthesize. Use this only when tasks can proceed independently. Set minimum_successes to null to use the host default or zero to wait for every member.",
		Parameters: parameters,
		Strict:     &strict,
	}
}

func parseTeamAssignments(arguments string, team TeamExecutor) ([]TeamAssignment, *int, error) {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var request teamDispatchRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, errors.New("expected one JSON object")
	}
	limit := team.MaxParallelAgents()
	if len(request.Tasks) == 0 || len(request.Tasks) > limit {
		return nil, nil, fmt.Errorf("team task count must be between 1 and %d", limit)
	}
	if request.MinimumSuccesses != nil && (*request.MinimumSuccesses < 0 || *request.MinimumSuccesses > len(request.Tasks)) {
		return nil, nil, fmt.Errorf("minimum_successes must be between 0 and %d", len(request.Tasks))
	}
	profiles := team.Profiles()
	seen := make(map[string]struct{}, len(request.Tasks))
	for index := range request.Tasks {
		task := &request.Tasks[index]
		task.ID = strings.TrimSpace(task.ID)
		task.Model = strings.TrimSpace(task.Model)
		task.Task = strings.TrimSpace(task.Task)
		if task.ID == "" || task.Task == "" {
			return nil, nil, errors.New("each team task requires a non-empty id and task")
		}
		if _, exists := seen[task.ID]; exists {
			return nil, nil, fmt.Errorf("team member id %q is repeated", task.ID)
		}
		seen[task.ID] = struct{}{}
		if _, exists := profiles[task.Model]; !exists {
			return nil, nil, fmt.Errorf("unknown team model profile %q", task.Model)
		}
	}
	return request.Tasks, request.MinimumSuccesses, nil
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
