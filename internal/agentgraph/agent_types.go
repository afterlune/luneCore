package agentgraph

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/afterlune/luneCore/acceptance"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/memory"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const UserQuestionContinuation = "user_question"
const ModelRecoveryContinuation = "model_recovery"
const AcceptanceContinuation = "acceptance_resolution"
const CommandCompletionBlocker = "running_commands"
const ExecutionBlockerContinuation = "execution_blocker"

// CurrentTaskSettingsVersion identifies the task-settings snapshot stored in checkpoints.
const CurrentTaskSettingsVersion = 9

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
	ActiveCommands             func(State) bool
	StopCommands               func(State) error
	CommandSummary             func(context.Context, State) (string, error)
	CommandEvidence            func(context.Context, State) ([]acceptance.Evidence, error)
	ID                         string
	Name                       string
	Logger                     *slog.Logger
	ModelObserver              func(State, string, publicmodel.Failure)
	retry                      *retryTiming
	Profile                    string
	Provider                   string
	Protocol                   string
	ServerCompaction           bool
	SupportsInputAudio         bool
	Model                      string
	Execution                  ExecutionOptions
	Team                       TeamExecutor
	AskUser                    bool
	AcceptanceEnabled          bool
	AcceptanceVerifier         acceptance.Verifier
	AcceptanceObserver         func(State, string, string, string)
	MemoryEnabled              bool
	MemoryRequester            Requester
	MemorySummaryFor           func(context.Context, State) (string, error)
	MemoryWriter               func(context.Context, State, []memory.Candidate) ([]string, error)
	MemoryObserver             func(State, string, []string, error)
	ProjectInstructionsForTask func(context.Context, State) ([]ProjectInstruction, error)
	ProjectInstructionsForTool func(context.Context, State, model.OutputFunctionCall) ([]ProjectInstruction, error)
	BlockerObserver            func(context.Context, State, string, string)
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
	CommandFinalizeAttempts int                  `json:"command_finalize_attempts,omitempty"`
	ModelFailure            *publicmodel.Failure `json:"model_failure,omitempty"`
	RunID                   string               `json:"run_id,omitempty"`
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
	PersonalInstructions      string                       `json:"personal_instructions,omitempty"`
	ProjectInstructionsReady  bool                         `json:"project_instructions_ready,omitempty"`
	ProjectInstructions       []ProjectInstruction         `json:"project_instructions,omitempty"`
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
	Model                    string
	ReasoningEffort          string
	Goal                     string
	PersonalInstructions     string
	ProjectInstructionsReady bool
	ProjectInstructions      []ProjectInstruction
	ContextWindow            int64
	ReserveOutputTokens      int64
	AutoCompact              bool
	AutoCompactThreshold     float64
	Execution                ExecutionOptions
	PermissionOverride       string
	PlanAuthorization        publictool.PlanAuthorization
	NoInteractiveDecisions   bool
}

// ExecutionOptions contains the small set of controls that affect one Agent run.
// Zero values retain luneGraph's default or unlimited behavior.
type ExecutionOptions struct {
	// ModelMaxAttempts includes the first request; zero defaults to three, one disables retry.
	ModelMaxAttempts int           `json:"model_max_attempts,omitempty"`
	MaxSteps         int           `json:"max_steps,omitempty"`
	MaxInvocations   int           `json:"max_invocations,omitempty"`
	MaxConcurrency   int           `json:"max_concurrency,omitempty"`
	ModelTimeout     time.Duration `json:"model_timeout,omitempty"`
	ToolTimeout      time.Duration `json:"tool_timeout,omitempty"`
	JoinTimeout      time.Duration `json:"join_timeout,omitempty"`
	TeamEntryTimeout time.Duration `json:"team_entry_timeout,omitempty"`
	TeamQuorum       int           `json:"team_quorum,omitempty"`
}
