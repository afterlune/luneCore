package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/agentruntime"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/session"
	publictool "github.com/afterlune/luneCore/tool"
)

const (
	legacyRunID                 = "lune-cli-active"
	DefaultAutoCompactThreshold = projectconfig.DefaultAutoCompactThreshold
	DefaultAttachmentMaxFiles   = projectconfig.DefaultAttachmentMaxFiles
)

// ErrStateDirInUse indicates that another Core instance already owns the state directory.
var ErrStateDirInUse = session.ErrStateDirInUse

const (
	DefaultAttachmentMaxFileBytes  = projectconfig.DefaultAttachmentMaxFileBytes
	DefaultAttachmentMaxTotalBytes = projectconfig.DefaultAttachmentMaxTotalBytes
)

const (
	PermissionReadOnly   PermissionMode = "read-only"
	PermissionWorkspace  PermissionMode = "workspace"
	PermissionAsk        PermissionMode = "ask"
	PermissionFullAccess PermissionMode = "full-access"
)

type TaskStatus string

// BudgetReason identifies the per-call execution limit that paused a task.
type BudgetReason = agentruntime.BudgetReason

const (
	BudgetMaxSteps       = agentruntime.BudgetMaxSteps
	BudgetMaxInvocations = agentruntime.BudgetMaxInvocations
)

const (
	TaskIdle      TaskStatus = "idle"
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskPaused    TaskStatus = "paused"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
	TaskDiscarded TaskStatus = "discarded"
)

type EventKind string

const (
	EventTaskStarted              EventKind = "task_started"
	EventTaskFinished             EventKind = "task_finished"
	EventTaskDiscarded            EventKind = "task_discarded"
	EventTaskQueued               EventKind = "task_queued"
	EventRuntimeStreamFailed      EventKind = "runtime_stream_failed"
	EventToolStarted              EventKind = "tool_started"
	EventToolFinished             EventKind = "tool_finished"
	EventUsage                    EventKind = "usage"
	EventApprovalRequired         EventKind = "approval_required"
	EventApprovalCancelled        EventKind = "approval_cancelled"
	EventUserQuestionRequired     EventKind = "user_question_required"
	EventUserInteractionRequired  EventKind = "user_interaction_required"
	EventUserInteractionCancelled EventKind = "user_interaction_cancelled"
	EventAgentStarted             EventKind = "agent_started"
	EventAgentFinished            EventKind = "agent_finished"
	EventModelStream              EventKind = "model_stream"
	EventContextCompaction        EventKind = "context_compaction"
	EventToolRecoveryRequired     EventKind = "tool_recovery_required"
	EventAcceptanceStarted        EventKind = "acceptance_started"
	EventAcceptanceCriteriaReady  EventKind = "acceptance_criteria_ready"
	EventAcceptanceReported       EventKind = "acceptance_reported"
	EventAcceptanceRequired       EventKind = "acceptance_required"
	EventAcceptanceResolved       EventKind = "acceptance_resolved"
	EventExecutionBlocked         EventKind = "execution_blocked"
)

// Event is a structured runtime notification. Arguments and response payloads
// are transient; clients should not persist or log them without sanitizing.
type Event struct {
	Kind EventKind
	// Cursor is non-zero for events written to the durable runtime event log.
	// Model stream deltas are live-only and have a zero cursor.
	Cursor int64
	// OccurredAt is set for events recorded in the durable runtime event log.
	OccurredAt time.Time
	// QueueID identifies an accepted queued turn until it starts or is discarded.
	QueueID string
	TaskID  string
	// ExecutionID remains stable when a paused task resumes with a new TaskID.
	ExecutionID            string
	SessionID              string
	Status                 TaskStatus
	BudgetReason           BudgetReason
	ContextWindow          int64
	EstimatedInputTokens   int64
	Tool                   ToolCall
	Usage                  Usage
	Approval               *ApprovalRequest
	ApprovalID             string
	UserQuestion           *UserQuestionRequest
	Interaction            *UserInteractionRequest
	InteractionID          string
	Err                    string
	AgentID                string
	AgentName              string
	AgentRole              string
	TeamRunID              string
	Provider               string
	ModelProfile           string
	Model                  string
	Stream                 *ModelStreamEvent
	Compaction             *ContextCompactionDetails
	ToolRecovery           *ToolRecoveryRequest
	AcceptanceStatus       string
	AcceptanceSource       AcceptanceSource
	AcceptanceOutcome      AcceptanceOutcome
	AcceptanceReason       AcceptancePauseReason
	AcceptanceCandidateID  string
	AcceptanceCriterionIDs []string
	BlockerID              string
	BlockerTool            string
}

// ExecutionBlocker is a persisted, non-interactive report for an operation
// that was not approved before execution started.
type ExecutionBlocker struct {
	ID          string
	ExecutionID string
	Tool        string
	Reason      string
}

type ContextCompactionDetails struct {
	Stage        string
	Mode         string
	BeforeTokens int64
	AfterTokens  int64
	Threshold    int64
	Err          string
}

type ModelStreamEvent struct {
	Type          string
	ItemID        string
	OutputIndex   *int
	Delta         string
	ToolCallID    string
	ToolName      string
	ToolArguments string
}

type ToolCall struct {
	ID string
	// InvocationID remains stable across explicit recovery retries.
	InvocationID string
	AgentID      string
	Name         string
	Arguments    json.RawMessage
	Duration     time.Duration
	Started      bool
	Recovered    bool
	Failed       bool
	Canceled     bool
}

type ToolRecoveryDecision string

const (
	ToolRecoveryRetry ToolRecoveryDecision = "retry"
	ToolRecoverySkip  ToolRecoveryDecision = "skip"
)

type ToolRecoveryRequest struct {
	InvocationID string
	CallID       string
	AgentID      string
	Tool         string
	Arguments    json.RawMessage
}

type ToolRecoveryRequiredError = agentruntime.ToolRecoveryRequiredError

type ApprovalRequest struct {
	ID        string
	AgentID   string
	Tool      string
	Arguments json.RawMessage
}

// UserInteractionRequest is a transient server request that needs a human
// response. Request payloads must not be persisted or logged without review.
type UserInteractionRequest struct {
	ID         string
	AgentID    string
	ServerName string
	Request    json.RawMessage
}

// UserQuestionRequest identifies a durable wait for a lead-agent clarification.
type UserQuestionRequest struct {
	ID           string
	AgentID      string
	RunID        string
	InvocationID string
	CallID       string
	ExecutionID  string
	Question     string
	Options      []string
}

// UserQuestionAnswer resumes exactly one persisted clarification wait.
type UserQuestionAnswer struct {
	RunID        string
	InvocationID string
	CallID       string
	Answer       string
}

type userInteractionResult struct {
	Response json.RawMessage
	Err      error
}

type pendingApproval struct {
	response    chan bool
	request     ApprovalRequest
	sessionID   string
	taskID      string
	executionID string
}

type pendingInteraction struct {
	response    chan userInteractionResult
	request     UserInteractionRequest
	sessionID   string
	taskID      string
	executionID string
}

type Usage struct {
	InputTokens         int64              `json:"input_tokens"`
	InputTokensDetails  TokenInputDetails  `json:"input_tokens_details"`
	OutputTokens        int64              `json:"output_tokens"`
	OutputTokensDetails TokenOutputDetails `json:"output_tokens_details"`
	TotalTokens         int64              `json:"total_tokens"`
}

type TokenInputDetails struct {
	CachedTokens        int64 `json:"cached_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
}

type TokenOutputDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type UsageRecord struct {
	Request              int    `json:"request"`
	Usage                Usage  `json:"usage"`
	EstimatedInputTokens int64  `json:"estimated_input_tokens,omitempty"`
	Available            bool   `json:"available"`
	ContextWindow        int64  `json:"context_window,omitempty"`
	AgentID              string `json:"agent_id,omitempty"`
	Provider             string `json:"provider,omitempty"`
	ModelProfile         string `json:"model_profile,omitempty"`
	Model                string `json:"model,omitempty"`
}

type UsageTotals struct {
	Requests       int   `json:"requests"`
	Measured       int   `json:"measured"`
	Input          int64 `json:"input"`
	EstimatedInput int64 `json:"estimated_input,omitempty"`
	Output         int64 `json:"output"`
	Reasoning      int64 `json:"reasoning"`
	Total          int64 `json:"total"`
	Cached         int64 `json:"cached"`
	CacheCreation  int64 `json:"cache_creation,omitempty"`
}

// SessionSettings contains per-session overrides. Nil context settings inherit
// the project defaults; a running task keeps its resolved values in checkpoint.
type SessionSettings struct {
	ModelProfile         string   `json:"model_profile"`
	ReasoningEffort      string   `json:"reasoning_effort,omitempty"`
	Goal                 string   `json:"goal,omitempty"`
	ContextWindow        *int64   `json:"context_window,omitempty"`
	ReserveOutputTokens  *int64   `json:"reserve_output_tokens,omitempty"`
	AutoCompact          *bool    `json:"auto_compact,omitempty"`
	AutoCompactThreshold *float64 `json:"auto_compact_threshold,omitempty"`
}

type taskSettings struct {
	Goal                 string
	ContextWindow        int64
	ReserveOutputTokens  int64
	AutoCompact          bool
	AutoCompactThreshold float64
}

// ContextDefaults exposes project-level context values to frontends so they
// can distinguish inherited defaults from session overrides.
type ContextDefaults struct {
	ContextWindow        int64
	ReserveOutputTokens  int64
	AutoCompact          bool
	AutoCompactThreshold float64
}

// AttachmentLimits exposes the effective project limits to frontends before
// they read or encode image data.
type AttachmentLimits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// MCPServerStatus describes one configured MCP server without exposing its
// credentials or child-process environment.
type MCPServerStatus struct {
	Name      string
	Transport string
	Endpoint  string
	Command   string
	Enabled   bool
	Connected bool
	ToolCount int
	LastError string
}

// ModelOption is a model profile exposed to clients without leaking the
// internal model package through Core's public API.
type ModelOption struct {
	Profile  string `json:"profile"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Session contains durable conversation state, not TUI transcript caches.
type Session struct {
	ID          string          `json:"id"`
	Workspace   string          `json:"workspace"`
	UpdatedAt   time.Time       `json:"updated_at"`
	DataVersion int             `json:"data_version"`
	Title       string          `json:"title"`
	Archived    bool            `json:"archived"`
	Settings    SessionSettings `json:"settings"`
	// History is the complete provider-neutral conversation shown to clients.
	History json.RawMessage `json:"history,omitempty"`
	// ModelContext is a cached provider-specific request context when it differs
	// from History. It can contain opaque Responses state and is never rendered.
	ModelContext       json.RawMessage        `json:"model_context,omitempty"`
	ModelContextKey    string                 `json:"model_context_key,omitempty"`
	UsageRequests      []UsageRecord          `json:"usage_requests,omitempty"`
	AgentUsage         map[string]UsageTotals `json:"agent_usage,omitempty"`
	AgentUsageComplete bool                   `json:"agent_usage_complete"`
	TaskUsage          UsageTotals            `json:"task_usage,omitempty"`
	SessionUsage       UsageTotals            `json:"session_usage,omitempty"`
}

type TaskInput struct{ Raw json.RawMessage }

func NewTaskInput[T any](value T) (TaskInput, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return TaskInput{}, fmt.Errorf("encode task input: %w", err)
	}
	return TaskInput{Raw: raw}, nil
}

func ParseTaskInput(line string) (TaskInput, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return TaskInput{}, errors.New("input is empty")
	}
	if json.Valid([]byte(line)) {
		var value any
		if err := json.Unmarshal([]byte(line), &value); err == nil {
			switch value.(type) {
			case string, []any:
				return TaskInput{Raw: json.RawMessage(line)}, nil
			}
		}
	}
	return NewTaskInput(line)
}

type TaskRequest struct {
	SessionID string
	Input     TaskInput
	// Model optionally overrides the selected profile's API model identifier for this task.
	Model                  string
	MaxToolRounds          int
	Execution              ExecutionOptions
	PlanAuthorization      publictool.PlanAuthorization
	NoInteractiveDecisions bool
}

type TaskState struct {
	Status    TaskStatus
	SessionID string
	TaskID    string
	// ExecutionID identifies the logical run across pause and resume attempts.
	ExecutionID          string
	QueueID              string
	ContextWindow        int64
	ReserveOutputTokens  int64
	AutoCompact          bool
	AutoCompactThreshold float64
	Goal                 string
	MaxToolRounds        int
	ModelOverride        string
	ModelProfile         string
	Provider             string
	Model                string
	ReasoningEffort      string
	Execution            ExecutionOptions
}

// ExecutionOptions configures limits and timeouts for one Agent run. Config
// values become host defaults; task request values override only non-zero fields.
// Recovery options replace the saved set, so zero restores runtime defaults.
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
