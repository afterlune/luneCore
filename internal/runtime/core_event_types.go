package runtime

import (
	"encoding/json"
	"time"

	"github.com/afterlune/luneCore/internal/agentruntime"
	runtimepermission "github.com/afterlune/luneCore/internal/tool"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type EventKind string

const (
	EventGoalUpdated EventKind = "goal_updated"
	EventGoalCleared EventKind = "goal_cleared"
	EventTaskStarted              EventKind = "task_started"
	EventTaskFinished             EventKind = "task_finished"
	EventTaskDiscarded            EventKind = "task_discarded"
	EventTaskQueued               EventKind = "task_queued"
	EventRuntimeStreamFailed      EventKind = "runtime_stream_failed"
	EventRuntimeReplayComplete    EventKind = "runtime_replay_complete"
	EventCommandStarted           EventKind = "command_started"
	EventCommandFinished          EventKind = "command_finished"
	EventToolStarted              EventKind = "tool_started"
	EventToolFinished             EventKind = "tool_finished"
	EventToolDetails              EventKind = "tool_details"
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
	EventExecutionBlocked         EventKind = "execution_blocked"
	EventMemoryStarted            EventKind = "memory_started"
	EventMemorySaved              EventKind = "memory_saved"
	EventMemorySkipped            EventKind = "memory_skipped"
	EventMemoryFailed             EventKind = "memory_failed"
)

// Event is a structured runtime notification. Arguments and response payloads
// are transient; clients should not persist or log them without sanitizing.
type Event struct {
	Goal *Goal
	GoalID string
	GoalRevision int64
	AutomaticGoal bool
	RequestID       string
	RequestAttempt  int
	RequestChanges  []string
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
	ExecutionID          string
	SessionID            string
	Status               TaskStatus
	BudgetReason         BudgetReason
	ContextWindow        int64
	EstimatedInputTokens int64
	Tool                 ToolCall
	Usage                Usage
	UsagePurpose         UsagePurpose
	Approval             *ApprovalRequest
	ApprovalID           string
	UserQuestion         *UserQuestionRequest
	Interaction          *UserInteractionRequest
	InteractionID        string
	Err                  string
	AgentID              string
	AgentName            string
	AgentRole            string
	AgentTask            string
	TeamRunID            string
	Provider             string
	ModelProfile         string
	Model                string
	Stream               *ModelStreamEvent
	Compaction           *ContextCompactionDetails
	ToolRecovery         *ToolRecoveryRequest
	BlockerID            string
	BlockerTool          string
	MemoryStatus         string
	MemoryIDs            []string
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
	Reason       string
	Strategy     string
	BeforeTokens int64
	AfterTokens  int64
	Threshold    int64
	Turns        int
	Err          string
}

type ModelStreamEvent struct {
	RequestID     string
	Purpose       string
	Type          string
	ItemID        string
	OutputIndex   *int
	Delta         string
	ToolCallID    string
	ToolName      string
	ToolArguments string
}

type ToolCall struct {
	ExecutionID string
	Details     *publictool.ExecutionDetails `json:"-"`
	ID          string
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
	ID               string
	AgentID          string
	Tool             string
	Arguments        json.RawMessage
	PathGrant        *publictool.PathGrant
	AutoReviewReason string
	Fingerprint      string
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
type UserQuestionOption = publicmodel.UserQuestionOption
type UserQuestionItem = publicmodel.UserQuestionItem
type UserQuestionResponse = publicmodel.UserQuestionResponse

type UserQuestionRequest struct {
	ID           string
	AgentID      string
	RunID        string
	InvocationID string
	CallID       string
	ExecutionID  string
	Questions    []UserQuestionItem
	Draft        *UserQuestionDraft
	DraftPayload json.RawMessage
}

// UserQuestionAnswer resumes exactly one persisted clarification wait.
type UserQuestionAnswer struct {
	ExecutionID  string
	RunID        string
	InvocationID string
	CallID       string
	Answers      []UserQuestionResponse
}

type userInteractionResult struct {
	Response json.RawMessage
	Err      error
}

type pendingApproval struct {
	response         chan publictool.ApprovalDecision
	request          ApprovalRequest
	pathGrants       *runtimepermission.SessionPathGrantStore
	sessionApprovals *runtimepermission.SessionApprovalStore
	sessionID        string
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
