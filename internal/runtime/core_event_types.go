package runtime

import (
	"encoding/json"
	"time"

	"github.com/afterlune/luneCore/internal/agentruntime"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type EventKind string

const (
	EventTaskStarted              EventKind = "task_started"
	EventTaskFinished             EventKind = "task_finished"
	EventTaskDiscarded            EventKind = "task_discarded"
	EventTaskQueued               EventKind = "task_queued"
	EventRuntimeStreamFailed      EventKind = "runtime_stream_failed"
	EventCommandStarted           EventKind = "command_started"
	EventCommandFinished          EventKind = "command_finished"
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
	EventModelRetry               EventKind = "model_retry"
	EventModelRecoveryRequired    EventKind = "model_recovery_required"
	EventModelRecoveryResumed     EventKind = "model_recovery_resumed"
	EventModelStream              EventKind = "model_stream"
	EventContextCompaction        EventKind = "context_compaction"
	EventToolRecoveryRequired     EventKind = "tool_recovery_required"
	EventAcceptanceStarted        EventKind = "acceptance_started"
	EventAcceptanceCriteriaReady  EventKind = "acceptance_criteria_ready"
	EventAcceptanceReported       EventKind = "acceptance_reported"
	EventAcceptanceRequired       EventKind = "acceptance_required"
	EventAcceptanceResolved       EventKind = "acceptance_resolved"
	EventExecutionBlocked         EventKind = "execution_blocked"
	EventMemoryStarted            EventKind = "memory_started"
	EventMemorySaved              EventKind = "memory_saved"
	EventMemorySkipped            EventKind = "memory_skipped"
	EventMemoryFailed             EventKind = "memory_failed"
)

// Event is a structured runtime notification. Arguments and response payloads
// are transient; clients should not persist or log them without sanitizing.
type Event struct {
	Kind            EventKind
	CommandID       string
	CommandStatus   publictool.CommandStatus
	CommandExitCode *int
	ModelFailure    *publicmodel.Failure
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
	MemoryStatus           string
	MemoryIDs              []string
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
