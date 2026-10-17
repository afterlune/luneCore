// Package core exposes the agent runtime without terminal presentation details.
package core

import coreruntime "github.com/afterlune/luneCore/internal/runtime"

const (
	CommandRunning     = coreruntime.CommandRunning
	CommandStopping    = coreruntime.CommandStopping
	CommandSucceeded   = coreruntime.CommandSucceeded
	CommandFailed      = coreruntime.CommandFailed
	CommandCancelled   = coreruntime.CommandCancelled
	CommandTimedOut    = coreruntime.CommandTimedOut
	CommandInterrupted = coreruntime.CommandInterrupted
)

const (
	BudgetMaxSteps       = coreruntime.BudgetMaxSteps
	BudgetMaxInvocations = coreruntime.BudgetMaxInvocations
	BudgetMaxTokens      = coreruntime.BudgetMaxTokens
)

const (
	UsagePurposeMain       = coreruntime.UsagePurposeMain
	UsagePurposeCompaction = coreruntime.UsagePurposeCompaction
	UsagePurposeMemory     = coreruntime.UsagePurposeMemory
	UsagePurposeTeam       = coreruntime.UsagePurposeTeam
)

const (
	DefaultAutoCompactThreshold    = coreruntime.DefaultAutoCompactThreshold
	DefaultAttachmentMaxFiles      = coreruntime.DefaultAttachmentMaxFiles
	DefaultAttachmentMaxFileBytes  = coreruntime.DefaultAttachmentMaxFileBytes
	DefaultAttachmentMaxTotalBytes = coreruntime.DefaultAttachmentMaxTotalBytes
	CurrentSessionDataVersion      = coreruntime.CurrentSessionDataVersion
)

const (
	EventTaskStarted              = coreruntime.EventTaskStarted
	EventTaskFinished             = coreruntime.EventTaskFinished
	EventTaskDiscarded            = coreruntime.EventTaskDiscarded
	EventTaskQueued               = coreruntime.EventTaskQueued
	EventRuntimeStreamFailed      = coreruntime.EventRuntimeStreamFailed
	EventCommandStarted           = coreruntime.EventCommandStarted
	EventCommandFinished          = coreruntime.EventCommandFinished
	EventToolStarted              = coreruntime.EventToolStarted
	EventToolFinished             = coreruntime.EventToolFinished
	EventToolDetails              = coreruntime.EventToolDetails
	EventUsage                    = coreruntime.EventUsage
	EventApprovalRequired         = coreruntime.EventApprovalRequired
	EventApprovalCancelled        = coreruntime.EventApprovalCancelled
	EventUserQuestionRequired     = coreruntime.EventUserQuestionRequired
	EventUserInteractionRequired  = coreruntime.EventUserInteractionRequired
	EventUserInteractionCancelled = coreruntime.EventUserInteractionCancelled
	EventAgentStarted             = coreruntime.EventAgentStarted
	EventAgentFinished            = coreruntime.EventAgentFinished
	EventModelRetry               = coreruntime.EventModelRetry
	EventModelRecoveryRequired    = coreruntime.EventModelRecoveryRequired
	EventModelRecoveryResumed     = coreruntime.EventModelRecoveryResumed
	EventModelStream              = coreruntime.EventModelStream
	EventContextCompaction        = coreruntime.EventContextCompaction
	EventToolRecoveryRequired     = coreruntime.EventToolRecoveryRequired
	EventExecutionBlocked         = coreruntime.EventExecutionBlocked
	EventMemoryStarted            = coreruntime.EventMemoryStarted
	EventMemorySaved              = coreruntime.EventMemorySaved
	EventMemorySkipped            = coreruntime.EventMemorySkipped
	EventMemoryFailed             = coreruntime.EventMemoryFailed
)

const (
	PendingModelRecoveryRequest = coreruntime.PendingModelRecoveryRequest
	PendingApproval             = coreruntime.PendingApproval
	PendingUserQuestion         = coreruntime.PendingUserQuestion
	PendingUserInteraction      = coreruntime.PendingUserInteraction
	PendingToolRecovery         = coreruntime.PendingToolRecovery
	PendingExecutionBlocker     = coreruntime.PendingExecutionBlocker
)

const (
	PermissionReadOnly   = coreruntime.PermissionReadOnly
	PermissionWorkspace  = coreruntime.PermissionWorkspace
	PermissionAsk        = coreruntime.PermissionAsk
	PermissionFullAccess = coreruntime.PermissionFullAccess
)

const (
	PlanImplementSameContext = coreruntime.PlanImplementSameContext
	PlanImplementNewContext  = coreruntime.PlanImplementNewContext
)

const (
	StartIfIdle = coreruntime.StartIfIdle
	QueueInput  = coreruntime.QueueInput
)

const (
	SubmissionAccepted = coreruntime.SubmissionAccepted
	SubmissionQueued   = coreruntime.SubmissionQueued
)

const (
	TaskIdle      = coreruntime.TaskIdle
	TaskQueued    = coreruntime.TaskQueued
	TaskRunning   = coreruntime.TaskRunning
	TaskPaused    = coreruntime.TaskPaused
	TaskCompleted = coreruntime.TaskCompleted
	TaskFailed    = coreruntime.TaskFailed
	TaskCancelled = coreruntime.TaskCancelled
	TaskDiscarded = coreruntime.TaskDiscarded
)

const (
	ToolRecoveryRetry = coreruntime.ToolRecoveryRetry
	ToolRecoverySkip  = coreruntime.ToolRecoverySkip
)

var (
	ErrPlanUnavailable           = coreruntime.ErrPlanUnavailable
	ErrPlanOutdated              = coreruntime.ErrPlanOutdated
	ErrSessionBusy               = coreruntime.ErrSessionBusy
	ErrTaskPaused                = coreruntime.ErrTaskPaused
	ErrTurnNotFound              = coreruntime.ErrTurnNotFound
	ErrQueuedTurnStarted         = coreruntime.ErrQueuedTurnStarted
	ErrMCPServerLifecycleChanged = coreruntime.ErrMCPServerLifecycleChanged
	ErrModelRetryNotReady        = coreruntime.ErrModelRetryNotReady
	ErrStateDirInUse             = coreruntime.ErrStateDirInUse
	ErrToolInvocationPersistence = coreruntime.ErrToolInvocationPersistence
)

type ApprovalRequest = coreruntime.ApprovalRequest
type AttachmentLimits = coreruntime.AttachmentLimits
type BudgetReason = coreruntime.BudgetReason
type CommandInfo = coreruntime.CommandInfo
type CommandOptions = coreruntime.CommandOptions
type CommandOutput = coreruntime.CommandOutput
type CommandReadOptions = coreruntime.CommandReadOptions
type CommandStatus = coreruntime.CommandStatus
type Config = coreruntime.Config
type ContextCompactionDetails = coreruntime.ContextCompactionDetails
type ContextDefaults = coreruntime.ContextDefaults
type Core = coreruntime.Core
type Event = coreruntime.Event
type EventKind = coreruntime.EventKind
type ExecutionBlocker = coreruntime.ExecutionBlocker
type ExecutionOptions = coreruntime.ExecutionOptions
type MCPPrompt = coreruntime.MCPPrompt
type MCPPromptArgument = coreruntime.MCPPromptArgument
type MCPPromptResult = coreruntime.MCPPromptResult
type MCPResource = coreruntime.MCPResource
type MCPResourceTemplate = coreruntime.MCPResourceTemplate
type MCPServerStatus = coreruntime.MCPServerStatus
type MemoryConfig = coreruntime.MemoryConfig
type ModelFailure = coreruntime.ModelFailure
type ModelOption = coreruntime.ModelOption
type ModelStreamEvent = coreruntime.ModelStreamEvent
type PendingModelRecovery = coreruntime.PendingModelRecovery
type PendingRequest = coreruntime.PendingRequest
type PendingRequestKind = coreruntime.PendingRequestKind
type PermissionMode = coreruntime.PermissionMode
type PlanAuthorization = coreruntime.PlanAuthorization
type PlanImplementation = coreruntime.PlanImplementation
type PlanImplementationMode = coreruntime.PlanImplementationMode
type ProposedPlan = coreruntime.ProposedPlan
type QueuedTurnState = coreruntime.QueuedTurnState
type RuntimeState = coreruntime.RuntimeState
type Session = coreruntime.Session
type SessionRuntime = coreruntime.SessionRuntime
type SessionSettings = coreruntime.SessionSettings
type SubmissionMode = coreruntime.SubmissionMode
type SubmissionStatus = coreruntime.SubmissionStatus
type TaskInput = coreruntime.TaskInput
type TaskPausedError = coreruntime.TaskPausedError
type TaskRequest = coreruntime.TaskRequest
type TaskResult = coreruntime.TaskResult
type TaskState = coreruntime.TaskState
type TaskStatus = coreruntime.TaskStatus
type TokenInputDetails = coreruntime.TokenInputDetails
type TokenOutputDetails = coreruntime.TokenOutputDetails
type ToolCall = coreruntime.ToolCall
type ToolRecoveryDecision = coreruntime.ToolRecoveryDecision
type ToolRecoveryRequest = coreruntime.ToolRecoveryRequest
type ToolRecoveryRequiredError = coreruntime.ToolRecoveryRequiredError
type TurnRequest = coreruntime.TurnRequest
type TurnResult = coreruntime.TurnResult
type TurnSubmission = coreruntime.TurnSubmission
type Usage = coreruntime.Usage
type UsageRecord = coreruntime.UsageRecord
type UsageTotals = coreruntime.UsageTotals
type UsagePurpose = coreruntime.UsagePurpose
type UserInteractionRequest = coreruntime.UserInteractionRequest
type UserQuestionOption = coreruntime.UserQuestionOption
type UserQuestionItem = coreruntime.UserQuestionItem
type UserQuestionResponse = coreruntime.UserQuestionResponse
type UserQuestionDraft = coreruntime.UserQuestionDraft
type UserQuestionDraftResponse = coreruntime.UserQuestionDraftResponse
type UserQuestionAnswer = coreruntime.UserQuestionAnswer
type UserQuestionRequest = coreruntime.UserQuestionRequest

// New constructs an embedded runtime.
func New(config Config) (*Core, error) { return coreruntime.New(config) }

// NewTaskInput creates a task input from any JSON-serializable value.
func NewTaskInput[T any](value T) (TaskInput, error) { return coreruntime.NewTaskInput(value) }

// ParseTaskInput parses command-line text as JSON input or plain text.
func ParseTaskInput(line string) (TaskInput, error) { return coreruntime.ParseTaskInput(line) }
