// Package agentgraph runs the existing model and tool interfaces on luneGraph.
package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/afterlune/luneCore/acceptance"
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
	criteriaNode       = "prepare_acceptance_criteria"
	acceptanceNode     = "accept_candidate"
	machineID          = "lune-agent-v3"
)

const UserQuestionContinuation = "user_question"
const AcceptanceContinuation = "acceptance_resolution"
const ExecutionBlockerContinuation = "execution_blocker"

// CurrentTaskSettingsVersion identifies the task-settings snapshot stored in checkpoints.
const CurrentTaskSettingsVersion = 7

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
	AcceptanceEnabled  bool
	AcceptanceVerifier acceptance.Verifier
	AcceptanceObserver func(State, string, string, string)
	BlockerObserver    func(context.Context, State, string, string)
	// ToolVisible filters registered tools for each model request.
	ToolVisible           func(string) bool
	ToolVisibleForMode    func(string, string) bool
	CurrentPermissionMode func() string
	StreamObserverFor     func(State, model.ModelStreamEvent)
	OnBranchStarted       func(context.Context, State)
	OnBranchFinished      func(context.Context, State, error)
	StreamObserver        func(model.ModelStreamEvent)
	CompactionObserver    func(ContextCompactionEvent)
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
	ExecutionID               string                       `json:"execution_id,omitempty"`
	AgentID                   string                       `json:"agent_id,omitempty"`
	AgentName                 string                       `json:"agent_name,omitempty"`
	Provider                  string                       `json:"provider,omitempty"`
	Protocol                  string                       `json:"protocol,omitempty"`
	ServerCompaction          bool                         `json:"server_compaction,omitempty"`
	SupportsInputAudio        bool                         `json:"supports_input_audio"`
	ServerCompactionFallback  bool                         `json:"server_compaction_fallback,omitempty"`
	ModelProfile              string                       `json:"model_profile,omitempty"`
	Model                     string                       `json:"model,omitempty"`
	ModelOverride             string                       `json:"model_override,omitempty"`
	ReasoningEffort           string                       `json:"reasoning_effort,omitempty"`
	TaskSettingsVersion       int                          `json:"task_settings_version,omitempty"`
	Execution                 ExecutionOptions             `json:"execution,omitempty"`
	PermissionOverride        string                       `json:"permission_override,omitempty"`
	PlanAuthorization         publictool.PlanAuthorization `json:"plan_authorization,omitempty"`
	NoInteractiveDecisions    bool                         `json:"no_interactive_decisions,omitempty"`
	BlockerID                 string                       `json:"blocker_id,omitempty"`
	BlockerTool               string                       `json:"blocker_tool,omitempty"`
	BlockerReason             string                       `json:"blocker_reason,omitempty"`
	Goal                      string                       `json:"goal,omitempty"`
	ContextWindow             int64                        `json:"context_window,omitempty"`
	ReserveOutputTokens       int64                        `json:"reserve_output_tokens,omitempty"`
	AutoCompact               bool                         `json:"auto_compact,omitempty"`
	AutoCompactThreshold      float64                      `json:"auto_compact_threshold,omitempty"`
	History                   model.ResponseInput          `json:"history"`
	Transcript                model.ResponseInput          `json:"transcript,omitempty"`
	CompactionWarningEmitted  bool                         `json:"compaction_warning_emitted,omitempty"`
	SelectedCategory          string                       `json:"selected_category,omitempty"`
	PendingCalls              []model.OutputFunctionCall   `json:"pending_calls,omitempty"`
	PendingIndex              int                          `json:"pending_index,omitempty"`
	VisibleTools              []string                     `json:"visible_tools,omitempty"`
	ToolRounds                int                          `json:"tool_rounds,omitempty"`
	MaxToolRounds             int                          `json:"max_tool_rounds,omitempty"`
	TaskError                 string                       `json:"task_error,omitempty"`
	FinalResponse             *model.Response              `json:"final_response,omitempty"`
	TeamBranch                bool                         `json:"team_branch,omitempty"`
	TeamTaskID                string                       `json:"team_task_id,omitempty"`
	TeamModel                 string                       `json:"team_model,omitempty"`
	TeamProvider              string                       `json:"team_provider,omitempty"`
	TeamText                  string                       `json:"team_text,omitempty"`
	TeamError                 string                       `json:"team_error,omitempty"`
	TeamStarted               bool                         `json:"team_started,omitempty"`
	TeamRunID                 string                       `json:"team_run_id,omitempty"`
	TeamDispatchID            string                       `json:"team_dispatch_id,omitempty"`
	TeamAssignments           []TeamAssignment             `json:"team_assignments,omitempty"`
	TeamMinimumSuccesses      int                          `json:"team_minimum_successes,omitempty"`
	UserQuestion              string                       `json:"user_question,omitempty"`
	UserQuestionOptions       []string                     `json:"user_question_options,omitempty"`
	UserAnswer                string                       `json:"user_answer,omitempty"`
	UserAnswerReady           bool                         `json:"user_answer_ready,omitempty"`
	TaskInput                 json.RawMessage              `json:"task_input,omitempty"`
	AcceptancePhase           string                       `json:"acceptance_phase,omitempty"`
	AcceptanceKind            acceptance.TaskKind          `json:"acceptance_kind,omitempty"`
	AcceptanceClarifications  []string                     `json:"acceptance_clarifications,omitempty"`
	AcceptanceCriteria        []acceptance.Criterion       `json:"acceptance_criteria,omitempty"`
	AcceptanceCandidate       string                       `json:"acceptance_candidate,omitempty"`
	AcceptanceCandidateID     string                       `json:"acceptance_candidate_id,omitempty"`
	AcceptanceCandidateCount  int                          `json:"acceptance_candidate_count,omitempty"`
	AcceptanceEvidenceRequest string                       `json:"acceptance_evidence_request,omitempty"`
	AcceptanceReports         []acceptance.Report          `json:"acceptance_reports,omitempty"`
	AcceptanceEvidence        []acceptance.Evidence        `json:"acceptance_evidence,omitempty"`
	AcceptanceRepairs         int                          `json:"acceptance_repairs,omitempty"`
	AcceptanceFormatRetries   int                          `json:"acceptance_format_retries,omitempty"`
	AcceptancePauseReason     acceptance.PauseReason       `json:"acceptance_pause_reason,omitempty"`
	AcceptancePaused          bool                         `json:"acceptance_paused,omitempty"`
	AcceptanceDecision        acceptance.Decision          `json:"acceptance_decision,omitempty"`
	AcceptanceDecisionRunID   string                       `json:"acceptance_decision_run_id,omitempty"`
	AcceptanceDecisionCallID  string                       `json:"acceptance_decision_call_id,omitempty"`
}

// TaskOptions configures one user task. A zero tool-round limit is unlimited.
type TaskOptions struct {
	ExecutionID   string
	MaxToolRounds int
	// Model optionally overrides the configured profile's model identifier for this task.
	Model                  string
	ReasoningEffort        string
	Goal                   string
	ContextWindow          int64
	ReserveOutputTokens    int64
	AutoCompact            bool
	AutoCompactThreshold   float64
	Execution              ExecutionOptions
	PermissionOverride     string
	PlanAuthorization      publictool.PlanAuthorization
	NoInteractiveDecisions bool
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

	startNode := modelNode
	if options.AcceptanceEnabled {
		startNode = criteriaNode
	}
	workflow := graph.New[State](startNode)
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: criteriaNode, Run: agent.requestCriteria, OnError: graph.FailExecution, Timeout: options.Execution.ModelTimeout}); err != nil {
		return nil, fmt.Errorf("add acceptance criteria node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: modelNode, Run: agent.requestLead, OnError: graph.FailExecution, Timeout: options.Execution.ModelTimeout}); err != nil {
		return nil, fmt.Errorf("add model node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: toolNode, Run: agent.executeLeadTool, OnError: graph.FailExecution, Timeout: options.Execution.ToolTimeout}); err != nil {
		return nil, fmt.Errorf("add tool node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: acceptanceNode, Run: agent.requestAcceptance, OnError: graph.FailExecution, Timeout: options.Execution.ModelTimeout}); err != nil {
		return nil, fmt.Errorf("add acceptance node: %w", err)
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
	if err := graph.RegisterJSONContinuation[State, acceptanceResolution](workflow, AcceptanceContinuation, func(_ context.Context, call graph.CallInfo, state State, decision acceptanceResolution) (State, error) {
		state.AcceptanceDecision = decision.Decision
		state.AcceptanceDecisionRunID = call.RunID
		state.AcceptanceDecisionCallID = call.CallID
		return state, nil
	}); err != nil {
		return nil, fmt.Errorf("register acceptance continuation: %w", err)
	}
	if err := graph.RegisterJSONContinuation[State, string](workflow, ExecutionBlockerContinuation, func(_ context.Context, _ graph.CallInfo, state State, _ string) (State, error) {
		return state, errors.New("an execution blocker requires an updated plan")
	}); err != nil {
		return nil, fmt.Errorf("register execution blocker continuation: %w", err)
	}
	if err := workflow.AddEdge(criteriaNode, toolNode); err != nil {
		return nil, fmt.Errorf("add criteria-to-tool edge: %w", err)
	}
	for _, edge := range [][2]string{{criteriaNode, modelNode}, {criteriaNode, criteriaNode}, {criteriaNode, acceptanceNode}, {modelNode, acceptanceNode}, {toolNode, criteriaNode}, {toolNode, acceptanceNode}, {acceptanceNode, modelNode}, {acceptanceNode, criteriaNode}, {acceptanceNode, acceptanceNode}} {
		if err := workflow.AddEdge(edge[0], edge[1]); err != nil {
			return nil, fmt.Errorf("add %s-to-%s edge: %w", edge[0], edge[1], err)
		}
	}
	if err := workflow.AddEdge(modelNode, toolNode); err != nil {
		return nil, fmt.Errorf("add model-to-tool edge: %w", err)
	}
	if err := workflow.AddEdge(acceptanceNode, toolNode); err != nil {
		return nil, fmt.Errorf("add acceptance-to-tool edge: %w", err)
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
	state.TaskSettingsVersion = CurrentTaskSettingsVersion
	state.Goal = taskOptions.Goal
	state.ContextWindow = taskOptions.ContextWindow
	state.ReserveOutputTokens = taskOptions.ReserveOutputTokens
	state.AutoCompact = taskOptions.AutoCompact
	state.AutoCompactThreshold = taskOptions.AutoCompactThreshold
	state.Execution = taskOptions.Execution
	state.PermissionOverride = taskOptions.PermissionOverride
	state.PlanAuthorization = taskOptions.PlanAuthorization
	state.NoInteractiveDecisions = taskOptions.NoInteractiveDecisions
	if input != nil {
		state.TaskInput, err = json.Marshal(input)
		if err != nil {
			return nil, graph.Result[State]{Status: graph.StatusFailed}, fmt.Errorf("encode task input for acceptance: %w", err)
		}
	}
	if a.options.AcceptanceEnabled {
		state.AcceptancePhase = "criteria"
	} else {
		state.AcceptancePhase = "execution"
	}
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

const teamDispatchToolName = "delegate_team"

const askUserToolName = model.UserQuestionToolName

type userQuestionInput struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

type acceptanceResolution struct {
	Decision acceptance.Decision `json:"decision"`
}

type teamDispatchRequest struct {
	Tasks            []TeamAssignment `json:"tasks"`
	MinimumSuccesses *int             `json:"minimum_successes,omitempty"`
}
