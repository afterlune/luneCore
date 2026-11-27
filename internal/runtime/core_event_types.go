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
	EventGoalUpdated              EventKind = "goal_updated"
	EventGoalCleared              EventKind = "goal_cleared"
	EventTaskStarted              EventKind = "task_started"
	EventTaskFinished             EventKind = "task_finished"
	EventTaskDiscarded            EventKind = "task_discarded"
	EventTaskQueued               EventKind = "task_queued"
	EventTaskInputInjected        EventKind = "task_input_injected"
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
	EventConfigReloaded           EventKind = "config_reloaded"
)

// Event is a structured runtime notification. Arguments and response payloads
// are transient; clients should not persist or log them without sanitizing.
type Event struct {
	Goal            *Goal                    `json:"goal,omitempty"`
	GoalID          string                   `json:"goalId,omitempty"`
	GoalRevision    int64                    `json:"goalRevision,omitempty"`
	AutomaticGoal   bool                     `json:"automaticGoal,omitempty"`
	RequestID       string                   `json:"requestId,omitempty"`
	RequestAttempt  int                      `json:"requestAttempt,omitempty"`
	RequestChanges  []string                 `json:"requestChanges,omitempty"`
	Kind            EventKind                `json:"kind"`
	CommandID       string                   `json:"commandId,omitempty"`
	CommandStatus   publictool.CommandStatus `json:"commandStatus,omitempty"`
	CommandExitCode *int                     `json:"commandExitCode,omitempty"`
	ModelFailure    *publicmodel.Failure     `json:"modelFailure,omitempty"`
	// Cursor is non-zero for events written to the durable runtime event log.
	// Model stream deltas are live-only and have a zero cursor.
	Cursor int64 `json:"cursor,omitempty"`
	// OccurredAt is set for events recorded in the durable runtime event log.
	OccurredAt time.Time `json:"occurredAt,omitempty"`
	// QueueID identifies an accepted queued turn until it starts or is discarded.
	QueueID string `json:"queueId,omitempty"`
	TaskID  string `json:"taskId,omitempty"`
	// InputExecutionID identifies a queued user message consumed by an active execution.
	InputExecutionID string `json:"inputExecutionId,omitempty"`
	// ParentExecutionID identifies the active execution that will consume a queued input.
	ParentExecutionID string `json:"parentExecutionId,omitempty"`
	// ExecutionID remains stable when a paused task resumes with a new TaskID.
	ExecutionID          string                    `json:"executionId,omitempty"`
	SessionID            string                    `json:"sessionId,omitempty"`
	Status               TaskStatus                `json:"status,omitempty"`
	BudgetReason         BudgetReason              `json:"budgetReason,omitempty"`
	ContextWindow        int64                     `json:"contextWindow,omitempty"`
	EstimatedInputTokens int64                     `json:"estimatedInputTokens,omitempty"`
	Tool                 ToolCall                  `json:"tool,omitempty"`
	Usage                Usage                     `json:"usage,omitempty"`
	UsagePurpose         UsagePurpose              `json:"usagePurpose,omitempty"`
	Approval             *ApprovalRequest          `json:"approval,omitempty"`
	ApprovalID           string                    `json:"approvalId,omitempty"`
	UserQuestion         *UserQuestionRequest      `json:"userQuestion,omitempty"`
	Interaction          *UserInteractionRequest   `json:"interaction,omitempty"`
	InteractionID        string                    `json:"interactionId,omitempty"`
	Err                  string                    `json:"error,omitempty"`
	AgentID              string                    `json:"agentId,omitempty"`
	AgentName            string                    `json:"agentName,omitempty"`
	AgentRole            string                    `json:"agentRole,omitempty"`
	AgentTask            string                    `json:"agentTask,omitempty"`
	TeamRunID            string                    `json:"teamRunId,omitempty"`
	Provider             string                    `json:"provider,omitempty"`
	ModelProfile         string                    `json:"modelProfile,omitempty"`
	Model                string                    `json:"model,omitempty"`
	Stream               *ModelStreamEvent         `json:"stream,omitempty"`
	Compaction           *ContextCompactionDetails `json:"compaction,omitempty"`
	ToolRecovery         *ToolRecoveryRequest      `json:"toolRecovery,omitempty"`
	BlockerID            string                    `json:"blockerId,omitempty"`
	BlockerTool          string                    `json:"blockerTool,omitempty"`
	MemoryStatus         string                    `json:"memoryStatus,omitempty"`
	MemoryIDs            []string                  `json:"memoryIds,omitempty"`
}

// ExecutionBlocker is a persisted, non-interactive report for an operation
// that was not approved before execution started.
type ExecutionBlocker struct {
	ID          string `json:"id"`
	ExecutionID string `json:"executionId"`
	Tool        string `json:"tool"`
	Reason      string `json:"reason"`
}

type ContextCompactionDetails struct {
	Stage        string `json:"stage"`
	Mode         string `json:"mode"`
	Reason       string `json:"reason"`
	Strategy     string `json:"strategy"`
	BeforeTokens int64  `json:"beforeTokens"`
	AfterTokens  int64  `json:"afterTokens"`
	Threshold    int64  `json:"threshold"`
	Turns        int    `json:"turns"`
	Err          string `json:"error,omitempty"`
}

type ModelStreamEvent struct {
	RequestID     string `json:"requestId,omitempty"`
	Purpose       string `json:"purpose,omitempty"`
	Type          string `json:"type"`
	ItemID        string `json:"itemId,omitempty"`
	OutputIndex   *int   `json:"outputIndex,omitempty"`
	Delta         string `json:"delta,omitempty"`
	ToolCallID    string `json:"toolCallId,omitempty"`
	ToolName      string `json:"toolName,omitempty"`
	ToolArguments string `json:"toolArguments,omitempty"`
}

type ToolCall struct {
	ExecutionID string                       `json:"executionId,omitempty"`
	Details     *publictool.ExecutionDetails `json:"-"`
	ID          string                       `json:"id,omitempty"`
	// InvocationID remains stable across explicit recovery retries.
	InvocationID string          `json:"invocationId,omitempty"`
	AgentID      string          `json:"agentId,omitempty"`
	Name         string          `json:"name,omitempty"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	Duration     time.Duration   `json:"duration,omitempty"`
	Started      bool            `json:"started,omitempty"`
	Recovered    bool            `json:"recovered,omitempty"`
	Failed       bool            `json:"failed,omitempty"`
	Canceled     bool            `json:"canceled,omitempty"`
}

type ToolRecoveryDecision string

const (
	ToolRecoveryRetry ToolRecoveryDecision = "retry"
	ToolRecoverySkip  ToolRecoveryDecision = "skip"
)

type ToolRecoveryRequest struct {
	InvocationID string          `json:"invocationId"`
	CallID       string          `json:"callId"`
	AgentID      string          `json:"agentId"`
	Tool         string          `json:"tool"`
	Arguments    json.RawMessage `json:"arguments"`
}

type ToolRecoveryRequiredError = agentruntime.ToolRecoveryRequiredError

type ApprovalRequest struct {
	ID               string                `json:"id"`
	AgentID          string                `json:"agentId"`
	Tool             string                `json:"tool"`
	Arguments        json.RawMessage       `json:"arguments"`
	PathGrant        *publictool.PathGrant `json:"pathGrant,omitempty"`
	AutoReviewReason string                `json:"autoReviewReason,omitempty"`
	Fingerprint      string                `json:"fingerprint,omitempty"`
}

// UserInteractionRequest is a transient server request that needs a human
// response. Request payloads must not be persisted or logged without review.
type UserInteractionRequest struct {
	ID         string          `json:"id"`
	AgentID    string          `json:"agentId"`
	ServerName string          `json:"serverName"`
	Request    json.RawMessage `json:"request"`
}

// UserQuestionRequest identifies a durable wait for a lead-agent clarification.
type UserQuestionOption = publicmodel.UserQuestionOption
type UserQuestionItem = publicmodel.UserQuestionItem
type UserQuestionResponse = publicmodel.UserQuestionResponse

type UserQuestionRequest struct {
	ID           string             `json:"id"`
	AgentID      string             `json:"agentId"`
	RunID        string             `json:"runId"`
	InvocationID string             `json:"invocationId"`
	CallID       string             `json:"callId"`
	ExecutionID  string             `json:"executionId"`
	Questions    []UserQuestionItem `json:"questions,omitempty"`
	Draft        *UserQuestionDraft `json:"draft,omitempty"`
	DraftPayload json.RawMessage    `json:"draftPayload,omitempty"`
}

// UserQuestionAnswer resumes exactly one persisted clarification wait.
type UserQuestionAnswer struct {
	ExecutionID  string                 `json:"executionId"`
	RunID        string                 `json:"runId"`
	InvocationID string                 `json:"invocationId"`
	CallID       string                 `json:"callId"`
	Answers      []UserQuestionResponse `json:"answers"`
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
	taskID           string
	executionID      string
}

type pendingInteraction struct {
	response    chan userInteractionResult
	request     UserInteractionRequest
	sessionID   string
	taskID      string
	executionID string
}
