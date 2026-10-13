// Package core exposes the agent runtime without terminal presentation details.
package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/afterlune/luneCore/internal/agentgraph"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/mcp"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneCore/internal/tool"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
	"github.com/afterlune/luneGraph/checkpoint/sqlite"
)

const (
	legacyRunID                 = "lune-cli-active"
	DefaultAutoCompactThreshold = projectconfig.DefaultAutoCompactThreshold
	DefaultAttachmentMaxFiles   = projectconfig.DefaultAttachmentMaxFiles
)

// ErrStateDirInUse indicates that another Core instance already owns the state directory.
var ErrStateDirInUse = errors.New("state directory is already in use by another Core instance")

const (
	DefaultAttachmentMaxFileBytes  = projectconfig.DefaultAttachmentMaxFileBytes
	DefaultAttachmentMaxTotalBytes = projectconfig.DefaultAttachmentMaxTotalBytes
)

type PermissionMode string

const (
	PermissionReadOnly   PermissionMode = "read-only"
	PermissionWorkspace  PermissionMode = "workspace"
	PermissionAsk        PermissionMode = "ask"
	PermissionFullAccess PermissionMode = "full-access"
)

type TaskStatus string

// BudgetReason identifies the per-call execution limit that paused a task.
type BudgetReason string

const (
	BudgetMaxSteps       BudgetReason = "max_steps"
	BudgetMaxInvocations BudgetReason = "max_invocations"
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
	ExecutionID          string
	SessionID            string
	Status               TaskStatus
	BudgetReason         BudgetReason
	ContextWindow        int64
	EstimatedInputTokens int64
	Tool                 ToolCall
	Usage                Usage
	Approval             *ApprovalRequest
	ApprovalID           string
	UserQuestion         *UserQuestionRequest
	Interaction          *UserInteractionRequest
	InteractionID        string
	Err                  string
	AgentID              string
	AgentName            string
	AgentRole            string
	TeamRunID            string
	Provider             string
	ModelProfile         string
	Model                string
	Stream               *ModelStreamEvent
	Compaction           *ContextCompactionDetails
	ToolRecovery         *ToolRecoveryRequest
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

type ToolRecoveryRequiredError struct{ InvocationID string }

func (e *ToolRecoveryRequiredError) Error() string {
	return fmt.Sprintf("tool invocation %q has an unknown outcome; resolve it as retry or skip before resuming", e.InvocationID)
}

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
	PlanMode  bool
	// Model optionally overrides the selected profile's API model identifier for this task.
	Model         string
	MaxToolRounds int
	Execution     ExecutionOptions
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
	PlanMode             bool
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

func agentExecutionOptions(options ExecutionOptions) agentgraph.ExecutionOptions {
	return agentgraph.ExecutionOptions{
		MaxSteps: options.MaxSteps, MaxInvocations: options.MaxInvocations, MaxConcurrency: options.MaxConcurrency,
		ModelTimeout: options.ModelTimeout, ToolTimeout: options.ToolTimeout, JoinTimeout: options.JoinTimeout,
		TeamEntryTimeout: options.TeamEntryTimeout, TeamQuorum: options.TeamQuorum,
	}
}

func coreExecutionOptions(options agentgraph.ExecutionOptions) ExecutionOptions {
	return ExecutionOptions{
		MaxSteps: options.MaxSteps, MaxInvocations: options.MaxInvocations, MaxConcurrency: options.MaxConcurrency,
		ModelTimeout: options.ModelTimeout, ToolTimeout: options.ToolTimeout, JoinTimeout: options.JoinTimeout,
		TeamEntryTimeout: options.TeamEntryTimeout, TeamQuorum: options.TeamQuorum,
	}
}

// Config constructs an embedded runtime. ModelConfig values override matching
// entries from config.json; omitted model settings are loaded from that file.
// ModelProviders replace protocol adapters with host implementations.
type Config struct {
	Workspace         string
	StateDir          string
	ModelConfig       *publicmodel.Config
	ModelProviders    map[string]publicmodel.Provider
	Tools             []publictool.Registration
	CommandExecutor   publictool.CommandExecutor
	AllowLocalCommand bool
	Execution         ExecutionOptions
	// MaxConcurrentCallbacks caps callback work across all sessions in this Core.
	MaxConcurrentCallbacks int
}

type Core struct {
	workspace         string
	registry          *tool.Registry
	models            *model.ProviderCatalog
	taskDefaults      taskSettings
	attachmentLimits  projectconfig.AttachmentLimits
	team              *teamExecutor
	runtime           *graphResponder
	mcpManager        *mcp.Manager
	checkpoints       *sqlite.Store[agentgraph.State]
	sessions          *session.Store
	executionDefaults ExecutionOptions
	callbackLimiter   *graph.Limiter
	// liveExecutions keeps warm Graph handles for Advance; checkpoints remain
	// the recovery source after process restart.
	liveExecutions map[string]*graph.Execution[agentgraph.State]
	stateDirLock   *stateDirectoryLock

	mu                  sync.RWMutex
	runtimeConfigMu     sync.RWMutex
	usageMu             sync.Mutex
	active              TaskState
	activeTasks         map[string]TaskState
	taskCancels         map[string]context.CancelFunc
	approvals           map[string]pendingApproval
	interactions        map[string]pendingInteraction
	interactiveSessions map[string]bool
	eventHubs           map[string]*sessionEventHub
	queueDraining       map[string]bool
	turnWaiters         map[string]chan turnResult
	executionRuns       map[string]chan struct{}
	interruptRequests   map[string]bool
	queueDrainWG        sync.WaitGroup
	closed              bool
}

func New(config Config) (_ *Core, err error) {
	if err := validateExecutionOptions(config.Execution); err != nil {
		return nil, fmt.Errorf("invalid execution defaults: %w", err)
	}
	if config.MaxConcurrentCallbacks < 0 {
		return nil, errors.New("maximum concurrent callbacks must not be negative")
	}
	var callbackLimiter *graph.Limiter
	if config.MaxConcurrentCallbacks > 0 {
		callbackLimiter, err = graph.NewLimiter(config.MaxConcurrentCallbacks)
		if err != nil {
			return nil, fmt.Errorf("create callback limiter: %w", err)
		}
	}
	workspace := config.Workspace
	if workspace == "" {
		workspace, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get workspace: %w", err)
		}
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	stateDir := config.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(workspace, ".luneagent")
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	stateLock, err := acquireStateDirectoryLock(stateDir)
	if err != nil {
		return nil, fmt.Errorf("acquire StateDir lock: %w", err)
	}
	keepStateLock := false
	defer func() {
		if !keepStateLock {
			if lockErr := stateLock.Close(); lockErr != nil {
				err = errors.Join(err, fmt.Errorf("release StateDir lock after initialization failure: %w", lockErr))
			}
		}
	}()
	projectConfig, err := loadProjectConfig(workspace, config.ModelConfig, config.ModelProviders)
	if err != nil {
		return nil, err
	}
	hostTools, err := tool.HostToolRegistrations(config.Tools)
	if err != nil {
		return nil, err
	}
	registry, err := tool.NewDefaultRegistry(workspace, tool.RegistryOptions{
		Tools: hostTools, CommandExecutor: config.CommandExecutor, AllowLocalCommands: config.AllowLocalCommand,
	})
	if err != nil {
		return nil, err
	}
	closeRegistry := true
	defer func() {
		if closeRegistry {
			_ = registry.Close()
		}
	}()
	models, err := model.NewProviderCatalogWithHostProviders(projectConfig.ModelConfig(), config.ModelProviders)
	if err != nil {
		return nil, err
	}
	checkpoints, err := sqlite.Open[agentgraph.State](context.Background(), filepath.Join(stateDir, "checkpoints-v8.db"), checkpoint.JSON[agentgraph.State]{})
	if err != nil {
		return nil, fmt.Errorf("open task checkpoints: %w", err)
	}
	closeCheckpoints := true
	defer func() {
		if closeCheckpoints {
			_ = checkpoints.Close()
		}
	}()
	sessions, err := session.Open(filepath.Join(stateDir, "sessions.db"))
	if err != nil {
		return nil, err
	}
	closeSessions := true
	defer func() {
		if closeSessions {
			_ = sessions.Close()
		}
	}()
	c := &Core{
		workspace:        workspace,
		registry:         registry,
		models:           models,
		attachmentLimits: projectConfig.Attachments,
		taskDefaults: taskSettings{
			ContextWindow: projectConfig.ContextWindow, ReserveOutputTokens: projectConfig.ReserveOutputTokens,
			AutoCompact: projectConfig.AutoCompact, AutoCompactThreshold: projectConfig.AutoCompactThreshold,
		},
		checkpoints:         checkpoints,
		sessions:            sessions,
		executionDefaults:   config.Execution,
		callbackLimiter:     callbackLimiter,
		liveExecutions:      make(map[string]*graph.Execution[agentgraph.State]),
		active:              TaskState{Status: TaskIdle},
		activeTasks:         make(map[string]TaskState),
		taskCancels:         make(map[string]context.CancelFunc),
		approvals:           make(map[string]pendingApproval),
		interactions:        make(map[string]pendingInteraction),
		interactiveSessions: make(map[string]bool),
		eventHubs:           make(map[string]*sessionEventHub),
		queueDraining:       make(map[string]bool),
		turnWaiters:         make(map[string]chan turnResult),
		executionRuns:       make(map[string]chan struct{}),
		interruptRequests:   make(map[string]bool),
		stateDirLock:        stateLock,
	}
	attachmentLimits := c.ProjectAttachmentLimits()
	mcpManager, err := mcp.NewManager(mcp.Options{
		Workspace: workspace, StateDir: stateDir, Servers: projectConfig.MCPServers, Registry: registry,
		Limits:             mcp.Limits{MaxBinaryBlocks: attachmentLimits.MaxFiles, MaxFileBytes: attachmentLimits.MaxFileBytes, MaxTotalBytes: attachmentLimits.MaxTotalBytes},
		RequestInteraction: c.requestUserInteraction,
	})
	if err != nil {
		return nil, err
	}
	c.mcpManager = mcpManager
	closeMCPManager := true
	defer func() {
		if closeMCPManager {
			_ = mcpManager.Close()
		}
	}()
	for _, restoreErr := range mcpManager.RestoreEnabled() {
		slog.Warn("could not restore enabled MCP server", "error_type", fmt.Sprintf("%T", restoreErr))
	}
	team := &teamExecutor{core: c, models: models, tools: registry}
	c.team = team
	c.runtime = &graphResponder{store: checkpoints}
	registry.SetToolEventObserver(c.onToolEvent)
	registry.SetToolCallController(c.executeToolCall)
	registry.SetPermissionApprovalHandler(c.requestApproval)
	if err := c.migrateLegacySession(context.Background()); err != nil {
		return nil, err
	}
	closeCheckpoints, closeSessions, closeRegistry, closeMCPManager = false, false, false, false
	keepStateLock = true
	return c, nil
}

func (c *Core) buildAgent(profileID string, taskState TaskState) (*agentgraph.Agent, *usageTrackingRequester, model.ModelProfile, error) {
	profile, ok := c.models.Profile(profileID)
	if !ok {
		return nil, nil, model.ModelProfile{}, fmt.Errorf("unknown model profile %q", profileID)
	}
	providerConfig, ok := c.models.ProviderConfig(profile.Provider)
	if !ok {
		return nil, nil, model.ModelProfile{}, fmt.Errorf("unknown provider %q for model profile %q", profile.Provider, profileID)
	}
	client, err := c.models.NewClient(profileID, c.registry)
	if err != nil {
		return nil, nil, model.ModelProfile{}, err
	}
	requester := &usageTrackingRequester{
		inner: client, agentID: "lead", profile: profileID,
		providerName: profile.Provider, modelName: profile.Model,
	}
	requester.SetContextAgentUsageObserver(c.onAgentUsageContext)
	agent, err := agentgraph.NewWithOptions(requester, c.registry, agentgraph.AgentOptions{
		ID: "lead", Name: "Lead", Profile: profileID, Provider: profile.Provider, Model: profile.Model,
		Protocol: providerConfig.Protocol, ServerCompaction: providerConfig.ServerCompaction,
		SupportsInputAudio: profile.SupportsInputAudio, Execution: agentExecutionOptions(taskState.Execution),
		Team:    c.team,
		AskUser: true,
		StreamObserverFor: func(state agentgraph.State, event model.ModelStreamEvent) {
			currentTask := taskState
			if current, ok := c.StateForSession(taskState.SessionID); ok {
				currentTask = current
			}
			activeModel := state.Model
			if state.ModelOverride != "" {
				activeModel = state.ModelOverride
			}
			c.onModelStreamForTask(currentTask, state.AgentID, state.AgentName, state.ModelProfile, state.Provider, activeModel, event)
		},
		OnBranchStarted: func(ctx context.Context, state agentgraph.State) {
			identity := publictool.CallIdentityFromContext(ctx)
			c.emit(Event{Kind: EventAgentStarted, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID,
				AgentID: state.AgentID, AgentName: state.AgentName, AgentRole: "worker", TeamRunID: state.TeamRunID,
				ModelProfile: state.ModelProfile, Provider: state.Provider, Model: state.Model, Status: TaskRunning})
		},
		OnBranchFinished: func(ctx context.Context, state agentgraph.State, branchErr error) {
			identity := publictool.CallIdentityFromContext(ctx)
			status := TaskCompleted
			if branchErr != nil {
				status = TaskFailed
				if errors.Is(branchErr, context.Canceled) {
					status = TaskCancelled
				}
			}
			c.emit(Event{Kind: EventAgentFinished, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID,
				AgentID: state.AgentID, AgentName: state.AgentName, AgentRole: "worker", TeamRunID: state.TeamRunID,
				ModelProfile: state.ModelProfile, Provider: state.Provider, Model: state.Model, Status: status, Err: errorString(branchErr)})
		},
		CompactionObserver: func(event agentgraph.ContextCompactionEvent) {
			currentTask := taskState
			if current, ok := c.StateForSession(taskState.SessionID); ok {
				currentTask = current
			}
			details := &ContextCompactionDetails{
				Stage: event.Stage, Mode: event.Mode, BeforeTokens: event.BeforeTokens,
				AfterTokens: event.AfterTokens, Threshold: event.Threshold, Err: errorString(event.Error),
			}
			c.emit(Event{Kind: EventContextCompaction, TaskID: currentTask.TaskID, ExecutionID: currentTask.ExecutionID, SessionID: currentTask.SessionID,
				Provider: profile.Provider, ModelProfile: profileID, Model: currentTask.Model, Err: errorString(event.Error), Compaction: details})
		},
	})
	if err != nil {
		return nil, nil, model.ModelProfile{}, err
	}
	return agent, requester, profile, nil
}

func (c *Core) configureRuntime(profileID, reasoningEffort string, taskState TaskState) (*graphResponder, model.ModelProfile, error) {
	agent, requester, profile, err := c.buildAgent(profileID, taskState)
	if err != nil {
		return nil, model.ModelProfile{}, err
	}
	runtime := &graphResponder{store: c.checkpoints, limiter: c.callbackLimiter}
	runtime.SetRunModel(agent, requester, profile.Model, reasoningEffort)
	return runtime, profile, nil
}

func (c *Core) Close() error {
	if c == nil {
		return nil
	}
	runtimeConfigLocked := c.runtimeConfigMu.TryLock()
	closingAfterQueueDrain := false
	if !runtimeConfigLocked {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil
		}
		if c.hasRunningTasksLocked() {
			c.mu.Unlock()
			return errors.New("cannot close core while a task is running")
		}
		if len(c.queueDraining) == 0 {
			c.mu.Unlock()
			return errors.New("cannot close core while runtime configuration changes are active")
		}
		c.closed = true
		c.mu.Unlock()
		c.queueDrainWG.Wait()
		for !c.runtimeConfigMu.TryLock() {
			time.Sleep(time.Millisecond)
		}
		runtimeConfigLocked = true
		closingAfterQueueDrain = true
	}
	defer func() {
		if runtimeConfigLocked {
			c.runtimeConfigMu.Unlock()
		}
	}()
	c.mu.Lock()
	if c.closed && !closingAfterQueueDrain {
		c.mu.Unlock()
		return nil
	}
	if !closingAfterQueueDrain && c.hasRunningTasksLocked() {
		c.mu.Unlock()
		return errors.New("cannot close core while a task is running")
	}
	if !closingAfterQueueDrain && len(c.queueDraining) > 0 {
		c.closed = true
		c.mu.Unlock()
		c.runtimeConfigMu.Unlock()
		runtimeConfigLocked = false
		c.queueDrainWG.Wait()
		for !c.runtimeConfigMu.TryLock() {
			time.Sleep(time.Millisecond)
		}
		runtimeConfigLocked = true
		c.mu.Lock()
	}
	c.closed = true
	for executionID, waiter := range c.turnWaiters {
		delete(c.turnWaiters, executionID)
		waiter <- turnResult{err: errors.New("core is closing")}
	}
	for id, pending := range c.approvals {
		delete(c.approvals, id)
		select {
		case pending.response <- false:
		default:
		}
	}
	for id, pending := range c.interactions {
		delete(c.interactions, id)
		select {
		case pending.response <- userInteractionResult{Err: errors.New("core is closing")}:
		default:
		}
	}
	c.mu.Unlock()
	var closeMCP error
	if c.mcpManager != nil {
		closeMCP = c.mcpManager.Close()
	}
	return errors.Join(closeMCP, c.checkpoints.Close(), c.sessions.Close(), c.registry.Close(), c.stateDirLock.Close())
}

func (c *Core) MCPServers() []MCPServerStatus {
	if c == nil || c.mcpManager == nil {
		return nil
	}
	servers := c.mcpManager.List()
	result := make([]MCPServerStatus, 0, len(servers))
	for _, server := range servers {
		result = append(result, MCPServerStatus{
			Name: server.Name, Transport: string(server.Transport), Endpoint: server.Endpoint, Command: server.Command, Enabled: server.Enabled,
			Connected: server.Connected, ToolCount: server.ToolCount, LastError: server.LastError,
		})
	}
	return result
}

func (c *Core) EnableMCPServer(name string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	if !c.runtimeConfigMu.TryLock() {
		return errors.New("cannot change MCP servers while a task is running")
	}
	defer c.runtimeConfigMu.Unlock()
	c.mu.RLock()
	closed := c.closed
	running := c.hasRunningTasksLocked()
	c.mu.RUnlock()
	if closed {
		return errors.New("core is closed")
	}
	if running {
		return errors.New("cannot change MCP servers while a task is running")
	}
	return c.mcpManager.Enable(name)
}

func (c *Core) DisableMCPServer(name string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	if !c.runtimeConfigMu.TryLock() {
		return errors.New("cannot change MCP servers while a task is running")
	}
	defer c.runtimeConfigMu.Unlock()
	c.mu.RLock()
	closed := c.closed
	running := c.hasRunningTasksLocked()
	c.mu.RUnlock()
	if closed {
		return errors.New("core is closed")
	}
	if running {
		return errors.New("cannot change MCP servers while a task is running")
	}
	return c.mcpManager.Disable(name)
}

func (c *Core) AuthenticateMCPServer(ctx context.Context, name string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	if !c.runtimeConfigMu.TryLock() {
		return errors.New("cannot authorize an MCP server while a task is running")
	}
	defer c.runtimeConfigMu.Unlock()
	c.mu.RLock()
	closed := c.closed
	running := c.hasRunningTasksLocked()
	c.mu.RUnlock()
	if closed {
		return errors.New("core is closed")
	}
	if running {
		return errors.New("cannot authorize an MCP server while a task is running")
	}
	return c.mcpManager.Authorize(ctx, name)
}

func (c *Core) OpenMCPURL(serverName, target string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	return c.mcpManager.OpenElicitationURL(serverName, target)
}

func (c *Core) requestUserInteraction(ctx context.Context, agentID, serverName string, request json.RawMessage) (json.RawMessage, error) {
	if agentID != "" && agentID != "lead" {
		return nil, errors.New("MCP user interaction from subagents is not supported")
	}
	identity := publictool.CallIdentityFromContext(ctx)
	c.mu.RLock()
	available := c.interactiveSessions[identity.SessionID]
	c.mu.RUnlock()
	if !available {
		return nil, errors.New("this client cannot display MCP user interaction; use the TUI")
	}
	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("create user interaction ID: %w", err)
	}
	channel := make(chan userInteractionResult, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("core is closed")
	}
	interaction := UserInteractionRequest{ID: id, AgentID: agentID, ServerName: serverName, Request: append(json.RawMessage(nil), request...)}
	c.interactions[id] = pendingInteraction{response: channel, request: interaction, sessionID: identity.SessionID, taskID: identity.TaskID, executionID: identity.ExecutionID}
	c.mu.Unlock()
	c.emit(Event{Kind: EventUserInteractionRequired, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID, Interaction: &interaction})
	select {
	case result := <-channel:
		return result.Response, result.Err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.interactions, id)
		c.mu.Unlock()
		c.emit(Event{Kind: EventUserInteractionCancelled, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID, InteractionID: id})
		return nil, ctx.Err()
	}
}

func (c *Core) resolveUserInteraction(sessionID, id string, response json.RawMessage) error {
	c.mu.Lock()
	pending, ok := c.interactions[id]
	if ok && pending.sessionID == sessionID {
		delete(c.interactions, id)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("user interaction %q is no longer pending in this session", id)
	}
	pending.response <- userInteractionResult{Response: append(json.RawMessage(nil), response...)}
	return nil
}

// State returns the state last changed by any session. Use StateForSession or
// ActiveTasks when a Core is running multiple sessions concurrently.
func (c *Core) State() TaskState {
	if c == nil {
		return TaskState{Status: TaskFailed}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active
}

// StateForSession returns the most recent task state for one session.
func (c *Core) StateForSession(sessionID string) (TaskState, bool) {
	if c == nil || sessionID == "" {
		return TaskState{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	state, ok := c.activeTasks[sessionID]
	return state, ok
}

func (c *Core) resolveUserQuestion(ctx context.Context, sessionID string, answer UserQuestionAnswer) error {
	if c == nil || ctx == nil || sessionID == "" || answer.RunID == "" || answer.InvocationID == "" || answer.CallID == "" {
		return errors.New("session runtime and exact user question wait identity are required")
	}
	question, pending, err := c.runtime.PendingUserQuestion(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("inspect pending user question: %w", err)
	}
	input := session.GraphResumeInput{RunID: answer.RunID, InvocationID: answer.InvocationID, CallID: answer.CallID}
	payload, err := json.Marshal(answer.Answer)
	if err != nil {
		return err
	}
	input.Payload = payload
	inserted, err := c.sessions.PutGraphResumeInput(ctx, sessionID, input)
	if err != nil {
		return err
	}
	matches := pending && question.RunID == answer.RunID && question.InvocationID == answer.InvocationID && question.CallID == answer.CallID
	if !matches {
		if inserted {
			_ = c.sessions.DeleteGraphResumeInput(ctx, sessionID, input)
			return fmt.Errorf("user question %q is stale or no longer pending in this session", answer.CallID)
		}
		return nil
	}
	if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskRunning && state.ExecutionID == question.ExecutionID {
		return nil
	}
	if _, _, err := c.recoverSession(ctx, sessionID, false); err != nil {
		if errors.Is(err, ErrSessionBusy) {
			return nil
		}
		return fmt.Errorf("resume answered user question: %w", err)
	}
	return nil
}

// ActiveTasks returns currently running tasks in stable session ID order.
func (c *Core) ActiveTasks() []TaskState {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, 0, len(c.activeTasks))
	for id, state := range c.activeTasks {
		if state.Status == TaskRunning {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	states := make([]TaskState, 0, len(ids))
	for _, id := range ids {
		states = append(states, c.activeTasks[id])
	}
	return states
}

func (c *Core) hasRunningTasksLocked() bool {
	for _, state := range c.activeTasks {
		if state.Status == TaskRunning {
			return true
		}
	}
	return false
}

func (c *Core) sessionIsRunningLocked(sessionID string) bool {
	if c.activeTasks[sessionID].Status == TaskRunning {
		return true
	}
	return c.active.Status == TaskRunning && c.active.SessionID == sessionID
}

func (c *Core) Workspace() string {
	if c == nil {
		return ""
	}
	return c.workspace
}

// ListModelProfiles returns configured profiles in stable name order.
func (c *Core) ListModelProfiles() []ModelOption {
	if c == nil || c.models == nil {
		return nil
	}
	profiles := c.models.Profiles()
	options := make([]ModelOption, 0, len(profiles))
	for id, profile := range profiles {
		options = append(options, ModelOption{Profile: id, Provider: profile.Provider, Model: profile.Model})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Profile < options[j].Profile })
	return options
}

// SetSessionSettings replaces the per-session overrides used by future tasks.
// A paused task continues with the settings saved in its checkpoint.
func (c *Core) SetSessionSettings(ctx context.Context, sessionID string, settings SessionSettings) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	settings, err := c.normalizeSessionSettings(settings)
	if err != nil {
		return err
	}
	// Usage writes and settings writes both replace the JSON session snapshot.
	// Serialize them so neither update can overwrite the other's fields.
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	saved.Settings = settings
	saved.UpdatedAt = time.Now()
	return c.saveSession(ctx, saved)
}

func (c *Core) ProjectContextDefaults() ContextDefaults {
	if c == nil {
		return ContextDefaults{}
	}
	return ContextDefaults{
		ContextWindow: c.taskDefaults.ContextWindow, ReserveOutputTokens: c.taskDefaults.ReserveOutputTokens,
		AutoCompact: c.taskDefaults.AutoCompact, AutoCompactThreshold: c.taskDefaults.AutoCompactThreshold,
	}
}

func (c *Core) ProjectAttachmentLimits() AttachmentLimits {
	limits := projectconfig.AttachmentLimits{}
	if c != nil {
		limits = c.attachmentLimits
	}
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = DefaultAttachmentMaxFiles
	}
	if limits.MaxFileBytes <= 0 {
		limits.MaxFileBytes = DefaultAttachmentMaxFileBytes
	}
	if limits.MaxTotalBytes <= 0 {
		limits.MaxTotalBytes = DefaultAttachmentMaxTotalBytes
	}
	return AttachmentLimits{
		MaxFiles: limits.MaxFiles, MaxFileBytes: limits.MaxFileBytes, MaxTotalBytes: limits.MaxTotalBytes,
	}
}

func (c *Core) normalizeSessionModelSettings(settings SessionSettings) (SessionSettings, error) {
	if settings.ModelProfile == "" {
		settings.ModelProfile = c.models.DefaultModel()
	}
	if _, ok := c.models.Profile(settings.ModelProfile); !ok {
		return SessionSettings{}, fmt.Errorf("unknown model profile %q", settings.ModelProfile)
	}
	effort := strings.TrimSpace(settings.ReasoningEffort)
	if effort == "default" {
		effort = ""
	}
	switch effort {
	case "", model.ReasoningEffortNone, model.ReasoningEffortMinimal, model.ReasoningEffortLow,
		model.ReasoningEffortMedium, model.ReasoningEffortHigh, model.ReasoningEffortXHigh, model.ReasoningEffortMax:
	default:
		return SessionSettings{}, fmt.Errorf("unsupported reasoning effort %q", settings.ReasoningEffort)
	}
	settings.ReasoningEffort = effort
	return settings, nil
}

func (c *Core) normalizeSessionSettings(settings SessionSettings) (SessionSettings, error) {
	settings, err := c.normalizeSessionModelSettings(settings)
	if err != nil {
		return SessionSettings{}, err
	}
	settings.Goal = strings.TrimSpace(settings.Goal)
	if settings.ContextWindow != nil && *settings.ContextWindow < 0 {
		return SessionSettings{}, errors.New("contextWindow override must not be negative")
	}
	if settings.ReserveOutputTokens != nil && *settings.ReserveOutputTokens < 0 {
		return SessionSettings{}, errors.New("reserveOutputTokens override must not be negative")
	}
	if settings.AutoCompactThreshold != nil && (math.IsNaN(*settings.AutoCompactThreshold) || math.IsInf(*settings.AutoCompactThreshold, 0) ||
		*settings.AutoCompactThreshold <= 0 || *settings.AutoCompactThreshold > 1) {
		return SessionSettings{}, errors.New("autoCompactThreshold override must be greater than 0 and at most 1")
	}
	if _, err := c.resolveTaskSettings(settings); err != nil {
		return SessionSettings{}, err
	}
	return settings, nil
}

func (c *Core) resolveTaskSettings(settings SessionSettings) (taskSettings, error) {
	effective := c.taskDefaults
	effective.Goal = strings.TrimSpace(settings.Goal)
	if settings.ContextWindow != nil {
		effective.ContextWindow = *settings.ContextWindow
	}
	if settings.ReserveOutputTokens != nil {
		effective.ReserveOutputTokens = *settings.ReserveOutputTokens
	}
	if settings.AutoCompact != nil {
		effective.AutoCompact = *settings.AutoCompact
	}
	if settings.AutoCompactThreshold != nil {
		effective.AutoCompactThreshold = *settings.AutoCompactThreshold
	}
	if effective.AutoCompact && effective.AutoCompactThreshold == 0 {
		effective.AutoCompactThreshold = projectconfig.DefaultAutoCompactThreshold
	}
	if effective.ContextWindow < 0 {
		return taskSettings{}, errors.New("contextWindow must not be negative")
	}
	if effective.ReserveOutputTokens < 0 {
		return taskSettings{}, errors.New("reserveOutputTokens must not be negative")
	}
	if effective.ContextWindow > 0 && effective.ReserveOutputTokens >= effective.ContextWindow {
		return taskSettings{}, errors.New("reserveOutputTokens must be smaller than contextWindow")
	}
	if math.IsNaN(effective.AutoCompactThreshold) || math.IsInf(effective.AutoCompactThreshold, 0) ||
		effective.AutoCompactThreshold < 0 || effective.AutoCompactThreshold > 1 {
		return taskSettings{}, errors.New("autoCompactThreshold must be between 0 and 1")
	}
	return effective, nil
}

func taskOptionsFromState(state agentgraph.State) agentgraph.TaskOptions {
	return agentgraph.TaskOptions{
		ExecutionID: state.ExecutionID,
		PlanMode:    state.PlanMode, MaxToolRounds: state.MaxToolRounds, Model: state.ModelOverride, ReasoningEffort: state.ReasoningEffort,
		Goal: state.Goal, ContextWindow: state.ContextWindow, ReserveOutputTokens: state.ReserveOutputTokens,
		AutoCompact: state.AutoCompact, AutoCompactThreshold: state.AutoCompactThreshold,
		Execution: state.Execution,
	}
}

func validateExecutionOptions(options ExecutionOptions) error {
	if options.MaxSteps < 0 || options.MaxInvocations < 0 || options.MaxConcurrency < 0 || options.TeamQuorum < 0 {
		return errors.New("execution limits must not be negative")
	}
	if options.ModelTimeout < 0 || options.ToolTimeout < 0 || options.JoinTimeout < 0 || options.TeamEntryTimeout < 0 {
		return errors.New("execution timeouts must not be negative")
	}
	return nil
}

func mergeExecutionOptions(defaults, overrides ExecutionOptions) ExecutionOptions {
	if overrides.MaxSteps != 0 {
		defaults.MaxSteps = overrides.MaxSteps
	}
	if overrides.MaxInvocations != 0 {
		defaults.MaxInvocations = overrides.MaxInvocations
	}
	if overrides.MaxConcurrency != 0 {
		defaults.MaxConcurrency = overrides.MaxConcurrency
	}
	if overrides.ModelTimeout != 0 {
		defaults.ModelTimeout = overrides.ModelTimeout
	}
	if overrides.ToolTimeout != 0 {
		defaults.ToolTimeout = overrides.ToolTimeout
	}
	if overrides.JoinTimeout != 0 {
		defaults.JoinTimeout = overrides.JoinTimeout
	}
	if overrides.TeamEntryTimeout != 0 {
		defaults.TeamEntryTimeout = overrides.TeamEntryTimeout
	}
	if overrides.TeamQuorum != 0 {
		defaults.TeamQuorum = overrides.TeamQuorum
	}
	return defaults
}

func checkpointSettingsVersionError(version int) error {
	return fmt.Errorf("cannot resume task checkpoint with task settings version %d (current version %d); discard it and start a new task", version, agentgraph.CurrentTaskSettingsVersion)
}

func (c *Core) PermissionMode() PermissionMode {
	if c == nil {
		return ""
	}
	return PermissionMode(c.registry.PermissionMode())
}

func (c *Core) SetPermissionMode(mode PermissionMode) error {
	if c == nil {
		return errors.New("core is nil")
	}
	return c.registry.SetPermissionMode(tool.PermissionMode(mode))
}

func (c *Core) resolveApproval(sessionID, id string, allowed bool) error {
	c.mu.Lock()
	pending, ok := c.approvals[id]
	if ok && pending.sessionID == sessionID {
		delete(c.approvals, id)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("approval request %q is no longer pending in this session", id)
	}
	pending.response <- allowed
	return nil
}

func (c *Core) RunTask(ctx context.Context, request TaskRequest) (json.RawMessage, error) {
	if c == nil || ctx == nil {
		return nil, errors.New("core and context are required")
	}
	return c.runSynchronous(ctx, request)
}

func (c *Core) ResumeTask(ctx context.Context, sessionID string, options ...ExecutionOptions) (json.RawMessage, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return nil, errors.New("core, context, and session id are required")
	}
	runtime, err := c.OpenSessionRuntime(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	cursor, err := c.sessions.LatestRuntimeEventCursor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	events, err := runtime.subscribeEvents(streamCtx, cursor, false)
	if err != nil {
		return nil, err
	}
	go func() {
		for range events {
		}
	}()
	submission, waiter, err := c.recoverSession(ctx, sessionID, true, options...)
	if err != nil {
		return nil, err
	}
	c.scheduleQueueDrain(sessionID)
	return c.waitForTurn(ctx, runtime, submission.ExecutionID, waiter)
}

func (c *Core) run(ctx context.Context, sessionID string, planMode bool, maxRounds int, modelOverride string, executionOptions ExecutionOptions, executionOverride bool, resume bool, input *model.ResponseInput, queueID, executionID string) (json.RawMessage, error) {
	if c == nil || ctx == nil {
		return nil, errors.New("core and context are required")
	}
	if sessionID == "" {
		return nil, errors.New("session id is required")
	}
	if maxRounds < 0 {
		return nil, errors.New("maximum tool rounds must not be negative")
	}
	if err := validateExecutionOptions(executionOptions); err != nil {
		return nil, err
	}
	if executionID != "" && c.takeInterruptRequest(executionID) {
		return nil, context.Canceled
	}
	c.runtimeConfigMu.RLock()
	defer c.runtimeConfigMu.RUnlock()
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return nil, errors.New("core is closed")
	}
	modelOverride = strings.TrimSpace(modelOverride)
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	settings := saved.Settings
	var pendingState agentgraph.State
	if resume {
		var ok bool
		pendingState, ok, err = c.pendingState(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("inspect paused task settings: %w", err)
		}
		if !ok {
			return nil, errors.New("no paused task exists in this session")
		}
		if pendingState.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
			return nil, checkpointSettingsVersionError(pendingState.TaskSettingsVersion)
		}
		if pendingState.ModelProfile != "" {
			settings.ModelProfile = pendingState.ModelProfile
		}
		settings.ReasoningEffort = pendingState.ReasoningEffort
		planMode = pendingState.PlanMode
	}
	profile, ok := c.models.Profile(settings.ModelProfile)
	if !ok {
		return nil, fmt.Errorf("session model profile %q is not configured", settings.ModelProfile)
	}
	providerConfig, ok := c.models.ProviderConfig(profile.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured", profile.Provider)
	}
	if resume && ((pendingState.Provider != "" && pendingState.Provider != profile.Provider) ||
		(pendingState.Model != "" && pendingState.Model != profile.Model)) {
		return nil, fmt.Errorf("paused task model profile %q changed in config; it remains pinned to provider %q and model %q", settings.ModelProfile, pendingState.Provider, pendingState.Model)
	}
	if resume && pendingState.Protocol != "" && (pendingState.Protocol != providerConfig.Protocol || pendingState.ServerCompaction != providerConfig.ServerCompaction) {
		return nil, fmt.Errorf("paused task provider protocol or compaction capability changed; it remains pinned to %q with serverCompaction=%t", pendingState.Protocol, pendingState.ServerCompaction)
	}
	if resume {
		settings, err = c.normalizeSessionModelSettings(settings)
	} else {
		settings, err = c.normalizeSessionSettings(settings)
	}
	if err != nil {
		return nil, err
	}
	profile, _ = c.models.Profile(settings.ModelProfile)
	var taskOptions agentgraph.TaskOptions
	if resume {
		taskOptions = taskOptionsFromState(pendingState)
		if executionOverride {
			taskOptions.Execution = agentExecutionOptions(executionOptions)
		}
		planMode = taskOptions.PlanMode
		maxRounds = taskOptions.MaxToolRounds
	} else {
		effective, resolveErr := c.resolveTaskSettings(settings)
		if resolveErr != nil {
			return nil, resolveErr
		}
		taskOptions = agentgraph.TaskOptions{
			PlanMode: planMode, MaxToolRounds: maxRounds, ReasoningEffort: settings.ReasoningEffort,
			Model: modelOverride,
			Goal:  effective.Goal, ContextWindow: effective.ContextWindow,
			ReserveOutputTokens: effective.ReserveOutputTokens, AutoCompact: effective.AutoCompact,
			AutoCompactThreshold: effective.AutoCompactThreshold,
			Execution:            agentExecutionOptions(mergeExecutionOptions(c.executionDefaults, executionOptions)),
		}
	}
	if resume {
		if taskOptions.ExecutionID == "" {
			return nil, errors.New("paused task checkpoint is missing its execution identity; discard it and start a new task")
		}
		if executionID != "" && executionID != taskOptions.ExecutionID {
			return nil, errors.New("queued recovery execution id does not match the paused checkpoint")
		}
	} else {
		if executionID != "" {
			taskOptions.ExecutionID = executionID
		} else {
			taskOptions.ExecutionID, err = newID()
			if err != nil {
				return nil, fmt.Errorf("create task execution ID: %w", err)
			}
		}
	}
	var transcript *model.ResponseInput
	if len(saved.History) > 0 && string(saved.History) != "null" {
		var decoded model.ResponseInput
		if err := json.Unmarshal(saved.History, &decoded); err != nil {
			return nil, fmt.Errorf("decode session history: %w", err)
		}
		transcript = &decoded
	}
	effectiveModel := profile.Model
	if taskOptions.Model != "" {
		effectiveModel = taskOptions.Model
	}
	contextKey := modelContextKey(profile.Provider, effectiveModel, providerConfig)
	modelContext := transcript
	if saved.ModelContextKey == contextKey && hasSessionHistory(saved.ModelContext) {
		var decoded model.ResponseInput
		if err := json.Unmarshal(saved.ModelContext, &decoded); err != nil {
			return nil, fmt.Errorf("decode provider model context: %w", err)
		}
		modelContext = &decoded
	}
	taskCtx, cancelTask := context.WithCancel(ctx)
	taskID, err := c.beginTask(taskCtx, sessionID, queueID, planMode, settings, profile, effectiveModel, taskOptions, saved.UpdatedAt, cancelTask)
	if err != nil {
		cancelTask()
		return nil, err
	}
	defer c.endTask(sessionID, taskID, cancelTask)
	ctx = taskCtx
	taskState, _ := c.StateForSession(sessionID)
	ctx = publictool.WithCallIdentity(ctx, publictool.CallIdentity{
		SessionID: sessionID, TaskID: taskState.TaskID, RunID: c.runtime.checkpointID(sessionID),
		ExecutionID: taskOptions.ExecutionID, AgentID: "lead",
	})
	if resume {
		if recoveryErr := c.checkToolRecovery(ctx, sessionID, taskState); recoveryErr != nil {
			status := TaskFailed
			var required *ToolRecoveryRequiredError
			if errors.As(recoveryErr, &required) {
				status = TaskPaused
			}
			c.finishTaskState(sessionID, status, recoveryErr)
			return nil, recoveryErr
		}
	}
	taskRuntime, _, err := c.configureRuntime(settings.ModelProfile, settings.ReasoningEffort, taskState)
	if err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return nil, err
	}
	if !resume {
		if err := c.resetTaskUsage(ctx, sessionID); err != nil {
			c.finishTaskState(sessionID, TaskFailed, err)
			return nil, err
		}
	}
	taskRuntime.SetSessionState(sessionID, transcript, modelContext, contextKey)
	taskRuntime.SetTaskOptions(taskOptions)
	taskRuntime.SetHistorySaver(func(id string, transcript, modelContext *model.ResponseInput, key string) error {
		return c.setSessionConversation(context.Background(), id, transcript, modelContext, key)
	})
	defer taskRuntime.SetHistorySaver(nil)
	taskRuntime.DeferCompletedCleanup(queueID != "")
	if resume {
		c.mu.RLock()
		taskRuntime.execution = c.liveExecutions[taskRuntime.runID()]
		c.mu.RUnlock()
	}
	var response *model.Response
	if resume {
		var inputs []graph.ResumeInput
		inputs, err = c.graphResumeInputs(ctx, sessionID, taskRuntime.runID())
		if err == nil {
			response, err = taskRuntime.Resume(ctx, inputs)
		}
		if reconcileErr := c.reconcileGraphResumeInputs(ctx, sessionID, taskRuntime.runID()); reconcileErr != nil {
			err = errors.Join(err, reconcileErr)
		}
	} else {
		response, err = taskRuntime.Respond(ctx, input)
	}
	pausedByGraph := errors.Is(err, ErrTaskPaused)
	if pausedByGraph {
		if question, found, questionErr := taskRuntime.PendingUserQuestion(ctx, sessionID); questionErr != nil {
			err = errors.Join(err, questionErr)
		} else if found {
			task, _ := c.StateForSession(sessionID)
			c.emit(Event{Kind: EventUserQuestionRequired, SessionID: sessionID, TaskID: task.TaskID, ExecutionID: question.ExecutionID,
				UserQuestion: &question})
		}
	}
	if response != nil && queueID == "" {
		_ = c.saveHistoryUsage(ctx, sessionID, taskRuntime)
	}
	var encoded json.RawMessage
	if response != nil {
		var encodeErr error
		encoded, encodeErr = json.Marshal(response)
		if encodeErr != nil {
			err = errors.Join(err, fmt.Errorf("encode task response: %w", encodeErr))
		}
	}
	status := TaskCompleted
	taskErr := err
	if pausedByGraph {
		status, taskErr = TaskPaused, err
	} else if err != nil {
		status = TaskFailed
		if ctx.Err() != nil {
			status = TaskPaused
		}
	}
	if pending, pendingErr := c.hasPending(ctx, sessionID); pendingErr == nil && pending && status != TaskCompleted {
		status = TaskPaused
	}
	if status == TaskCompleted && response == nil {
		status = TaskFailed
		err = errors.Join(err, errors.New("agent task completed without a response"))
		taskErr = err
	}
	c.mu.Lock()
	if taskRuntime.execution != nil && queueID != "" {
		if c.liveExecutions == nil {
			c.liveExecutions = make(map[string]*graph.Execution[agentgraph.State])
		}
		if status == TaskPaused {
			c.liveExecutions[taskRuntime.runID()] = taskRuntime.execution
		} else {
			delete(c.liveExecutions, taskRuntime.runID())
		}
	}
	c.mu.Unlock()
	if queueID != "" && status != TaskPaused {
		queued := session.QueuedTurn{SessionID: sessionID, QueueID: queueID, ExecutionID: executionID}
		if persistErr := c.completeQueuedTurnFromRuntime(ctx, queued, status, encoded, taskRuntime); persistErr != nil {
			err = errors.Join(err, fmt.Errorf("persist completed turn: %w", persistErr))
		}
	}
	c.finishTaskState(sessionID, status, taskErr)
	if queueID == "" && status != TaskPaused {
		if cleanupErr := c.sessions.DeleteToolInvocationsForSession(context.Background(), sessionID); cleanupErr != nil {
			slog.Warn("delete completed tool invocation records", "error", cleanupErr)
		}
	}
	if response == nil {
		return nil, err
	}
	return encoded, err
}

func (c *Core) graphResumeInputs(ctx context.Context, sessionID, runID string) ([]graph.ResumeInput, error) {
	stored, err := c.sessions.GraphResumeInputs(ctx, sessionID, runID)
	if err != nil || len(stored) == 0 {
		return nil, err
	}
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var inputs []graph.ResumeInput
	for _, input := range stored {
		for _, invocation := range cp.Invocations {
			if invocation.Status == graph.InvocationWaiting && invocation.Continuation == agentgraph.UserQuestionContinuation && invocation.ID == input.InvocationID && invocation.CallID == input.CallID {
				inputs = append(inputs, graph.ResumeInput{InvocationID: input.InvocationID, CallID: input.CallID, Payload: append([]byte(nil), input.Payload...)})
				break
			}
		}
	}
	return inputs, nil
}

func (c *Core) reconcileGraphResumeInputs(ctx context.Context, sessionID, runID string) error {
	stored, err := c.sessions.GraphResumeInputs(ctx, sessionID, runID)
	if err != nil || len(stored) == 0 {
		return err
	}
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, input := range stored {
		stillPending := false
		for _, invocation := range cp.Invocations {
			if invocation.Status == graph.InvocationWaiting && invocation.Continuation == agentgraph.UserQuestionContinuation && invocation.ID == input.InvocationID && invocation.CallID == input.CallID {
				stillPending = true
				break
			}
		}
		if !stillPending {
			if err := c.sessions.MarkGraphResumeInputConsumed(ctx, sessionID, input); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Core) beginTask(ctx context.Context, sessionID, queueID string, planMode bool, settings SessionSettings, profile model.ModelProfile, modelName string, taskOptions agentgraph.TaskOptions, expectedUpdatedAt time.Time, cancel context.CancelFunc) (string, error) {
	if ctx == nil {
		return "", errors.New("context is nil")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		c.mu.Unlock()
		return "", errors.New("a task is already running in this session")
	}
	if c.interruptRequests[taskOptions.ExecutionID] {
		delete(c.interruptRequests, taskOptions.ExecutionID)
		c.mu.Unlock()
		return "", context.Canceled
	}
	if queueID == "" {
		queued, err := c.sessions.QueuedTurns(ctx, sessionID)
		if err != nil {
			c.mu.Unlock()
			return "", err
		}
		if len(queued) > 0 {
			c.mu.Unlock()
			return "", ErrSessionBusy
		}
	}
	snapshot, err := c.sessions.Get(ctx, sessionID, c.workspace)
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	if !snapshot.UpdatedAt.Equal(expectedUpdatedAt) {
		c.mu.Unlock()
		return "", errors.New("session changed while preparing the task; retry with the latest session state")
	}
	id, err := newID()
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	state := TaskState{
		Status: TaskRunning, SessionID: sessionID, TaskID: id, ExecutionID: taskOptions.ExecutionID, QueueID: queueID, ContextWindow: taskOptions.ContextWindow,
		ReserveOutputTokens: taskOptions.ReserveOutputTokens, AutoCompact: taskOptions.AutoCompact,
		AutoCompactThreshold: taskOptions.AutoCompactThreshold, Goal: taskOptions.Goal, MaxToolRounds: taskOptions.MaxToolRounds,
		ModelOverride: taskOptions.Model, PlanMode: planMode,
		ModelProfile: settings.ModelProfile, Provider: profile.Provider, Model: modelName,
		ReasoningEffort: settings.ReasoningEffort, Execution: coreExecutionOptions(taskOptions.Execution),
	}
	if c.activeTasks == nil {
		c.activeTasks = make(map[string]TaskState)
	}
	c.activeTasks[sessionID] = state
	if cancel != nil {
		c.taskCancels[sessionID] = cancel
	} else {
		delete(c.taskCancels, sessionID)
	}
	c.active = state
	c.mu.Unlock()
	c.emit(Event{Kind: EventTaskStarted, TaskID: id, ExecutionID: taskOptions.ExecutionID, QueueID: queueID, SessionID: sessionID, Status: TaskRunning, ContextWindow: taskOptions.ContextWindow,
		ModelProfile: settings.ModelProfile, Provider: profile.Provider, Model: modelName})
	return id, nil
}

func (c *Core) endTask(sessionID, taskID string, cancel context.CancelFunc) {
	c.mu.Lock()
	state, ok := c.activeTasks[sessionID]
	if ok && state.TaskID == taskID {
		delete(c.taskCancels, sessionID)
		if state.Status == TaskRunning {
			state.Status = TaskIdle
			c.activeTasks[sessionID] = state
			c.active = state
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Core) finishTaskState(sessionID string, status TaskStatus, err error) {
	c.mu.Lock()
	state := c.activeTasks[sessionID]
	state.Status = status
	c.activeTasks[sessionID] = state
	c.active = state
	c.mu.Unlock()
	event := Event{Kind: EventTaskFinished, TaskID: state.TaskID, ExecutionID: state.ExecutionID, QueueID: state.QueueID, SessionID: sessionID, Status: status,
		BudgetReason:  budgetReasonFromError(err),
		ContextWindow: state.ContextWindow, Err: errorString(err), ModelProfile: state.ModelProfile, Provider: state.Provider, Model: state.Model}
	if state.QueueID != "" {
		if status != TaskPaused {
			// The queue worker commits terminal state, result, conversation, and
			// event together after the graph has produced a durable checkpoint.
			return
		}
		persistErr := c.persistQueuedEvent(context.Background(), event, func(payload json.RawMessage) (int64, error) {
			return c.sessions.PauseQueuedTurnWithReason(context.Background(), sessionID, state.QueueID, state.ExecutionID, string(TaskPaused), string(event.BudgetReason), payload)
		})
		if persistErr != nil {
			slog.Warn("persist paused queued task", "error", persistErr)
		}
		return
	}
	c.emit(event)
}

func (c *Core) onToolEvent(event tool.ToolEvent) {
	kind := EventToolFinished
	if event.Started {
		kind = EventToolStarted
	}
	c.emit(Event{Kind: kind, SessionID: event.SessionID, TaskID: event.TaskID, ExecutionID: event.ExecutionID, Tool: ToolCall{
		ID: event.CallID, InvocationID: event.InvocationID, AgentID: event.AgentID, Name: event.Name, Arguments: event.Arguments, Started: event.Started,
		Duration: event.Duration, Recovered: event.Recovered, Failed: event.Failed, Canceled: event.Canceled,
	}})
}

func (c *Core) onModelStream(agentID, agentName, profile, provider, modelID string, event model.ModelStreamEvent) {
	c.onModelStreamForTask(c.State(), agentID, agentName, profile, provider, modelID, event)
}

func (c *Core) onModelStreamForTask(state TaskState, agentID, agentName, profile, provider, modelID string, event model.ModelStreamEvent) {
	stream := &ModelStreamEvent{
		Type: string(event.Type), ItemID: event.ItemID, OutputIndex: event.OutputIndex,
		Delta: event.Delta,
	}
	if event.ToolCall != nil {
		stream.ToolCallID, stream.ToolName, stream.ToolArguments = event.ToolCall.CallID, event.ToolCall.Name, event.ToolCall.Arguments
	}
	c.emit(Event{
		Kind: EventModelStream, SessionID: state.SessionID, TaskID: state.TaskID, ExecutionID: state.ExecutionID,
		AgentID: agentID, AgentName: agentName, ModelProfile: profile, Provider: provider, Model: modelID,
		Stream: stream,
	})
}

func (c *Core) onUsage(usage model.ResponseUsage) {
	c.onAgentUsage(usage, "", "", "", "", 0)
}

func (c *Core) onAgentUsage(usage model.ResponseUsage, agentID, profile, provider, modelID string, estimatedInputTokens int64) {
	c.onAgentUsageForTask(c.State(), usage, agentID, profile, provider, modelID, estimatedInputTokens)
}

func (c *Core) onAgentUsageContext(ctx context.Context, usage model.ResponseUsage, agentID, profile, provider, modelID string, estimatedInputTokens int64) {
	identity := publictool.CallIdentityFromContext(ctx)
	state, ok := c.StateForSession(identity.SessionID)
	if !ok {
		state = TaskState{SessionID: identity.SessionID, TaskID: identity.TaskID}
	}
	c.onAgentUsageForTask(state, usage, agentID, profile, provider, modelID, estimatedInputTokens)
}

func (c *Core) onAgentUsageForTask(state TaskState, usage model.ResponseUsage, agentID, profile, provider, modelID string, estimatedInputTokens int64) {
	value := Usage{InputTokens: usage.InputTokens, InputTokensDetails: TokenInputDetails{
		CachedTokens: usage.InputTokensDetails.CachedTokens, CacheCreationTokens: usage.InputTokensDetails.CacheCreationTokens,
	},
		OutputTokens: usage.OutputTokens, OutputTokensDetails: TokenOutputDetails{ReasoningTokens: usage.OutputTokensDetails.ReasoningTokens}, TotalTokens: usage.TotalTokens}
	contextWindow := state.ContextWindow
	if agentID != "" && agentID != "lead" {
		contextWindow = 0
	}
	if state.SessionID != "" {
		if err := c.recordUsageForAgent(context.Background(), state.SessionID, value, agentID, profile, provider, modelID, contextWindow, estimatedInputTokens); err != nil {
			slog.Warn("persist token usage", "error", err)
		}
	}
	c.emit(Event{Kind: EventUsage, SessionID: state.SessionID, TaskID: state.TaskID, ExecutionID: state.ExecutionID, Usage: value,
		AgentID: agentID, ModelProfile: profile, Provider: provider, Model: modelID, ContextWindow: contextWindow,
		EstimatedInputTokens: estimatedInputTokensIfMissing(value.InputTokens, estimatedInputTokens)})
}

func (c *Core) requestApproval(ctx context.Context, request tool.PermissionRequest) bool {
	if ctx.Err() != nil {
		return false
	}
	id, err := newID()
	if err != nil {
		return false
	}
	response := make(chan bool, 1)
	identity := publictool.CallIdentityFromContext(ctx)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	approval := ApprovalRequest{ID: id, AgentID: request.AgentID, Tool: request.Tool, Arguments: append(json.RawMessage(nil), request.Arguments...)}
	c.approvals[id] = pendingApproval{response: response, request: approval, sessionID: identity.SessionID, taskID: identity.TaskID, executionID: identity.ExecutionID}
	c.mu.Unlock()
	c.emit(Event{Kind: EventApprovalRequired, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID,
		AgentID: request.AgentID, Approval: &approval})
	select {
	case allowed := <-response:
		return allowed
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.approvals, id)
		c.mu.Unlock()
		c.emit(Event{Kind: EventApprovalCancelled, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID, ApprovalID: id})
		return false
	}
}

func (c *Core) CreateSession(ctx context.Context, history json.RawMessage) (Session, error) {
	if c == nil || ctx == nil {
		return Session{}, errors.New("core and context are required")
	}
	return c.createSession(ctx, history, SessionSettings{ModelProfile: c.models.DefaultModel()})
}

func (c *Core) createSession(ctx context.Context, history json.RawMessage, settings SessionSettings) (Session, error) {
	if len(history) == 0 {
		history = json.RawMessage("null")
	}
	if !json.Valid(history) {
		return Session{}, errors.New("session history must be valid JSON")
	}
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	saved := Session{
		ID: id, Workspace: c.workspace, UpdatedAt: time.Now(), DataVersion: CurrentSessionDataVersion,
		Settings: settings, AgentUsage: map[string]UsageTotals{}, AgentUsageComplete: true,
		History: append(json.RawMessage(nil), history...),
	}
	if err := c.saveSession(ctx, saved); err != nil {
		return Session{}, err
	}
	return saved, nil
}

func (c *Core) ForkSession(ctx context.Context, sessionID string) (Session, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return Session{}, errors.New("core, context, and session id are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Session{}, errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		return Session{}, errors.New("cannot fork a session while its task is running")
	}
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	return c.createSession(ctx, saved.History, saved.Settings)
}

// ForkPausedTask clones a paused, nonterminal task checkpoint into a sibling
// session. The returned session remains paused until Recover is called.
func (c *Core) ForkPausedTask(ctx context.Context, sessionID string) (Session, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return Session{}, errors.New("core, context, and session id are required")
	}
	c.runtimeConfigMu.RLock()
	defer c.runtimeConfigMu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Session{}, errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) || c.queueDraining[sessionID] {
		return Session{}, errors.New("cannot fork a task while its session is running")
	}
	c.usageMu.Lock()
	defer c.usageMu.Unlock()

	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	sourceRunID := c.runtime.checkpointID(sessionID)
	sourceCheckpoint, err := c.checkpoints.Load(ctx, sourceRunID)
	if err != nil {
		return Session{}, fmt.Errorf("load paused task checkpoint: %w", err)
	}
	if sourceCheckpoint.Completed {
		return Session{}, graph.ErrRunCompleted
	}
	var pending agentgraph.State
	foundPending := false
	for _, invocation := range sourceCheckpoint.Invocations {
		switch invocation.Status {
		case graph.InvocationEnded, graph.InvocationFailed, graph.InvocationJoined:
			continue
		default:
			pending, foundPending = invocation.State, true
			break
		}
		if foundPending {
			break
		}
	}
	if !foundPending || pending.ExecutionID == "" || pending.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
		return Session{}, errors.New("session does not contain a forkable paused task")
	}
	queuedTurns, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	var sourceTurn *session.QueuedTurn
	for i := range queuedTurns {
		if queuedTurns[i].ExecutionID == pending.ExecutionID && queuedTurns[i].Status == session.QueuedTurnPaused {
			sourceTurn = &queuedTurns[i]
			break
		}
	}
	if sourceTurn == nil {
		return Session{}, errors.New("paused task is missing its durable turn record")
	}
	var turnRequest storedTurnRequest
	if err := json.Unmarshal(sourceTurn.Request, &turnRequest); err != nil {
		return Session{}, fmt.Errorf("decode paused task request: %w", err)
	}
	if turnRequest.ExecutionOverride {
		pending.Execution = agentExecutionOptions(turnRequest.Execution)
	}
	turnRequest.Resume = true
	requestJSON, err := json.Marshal(turnRequest)
	if err != nil {
		return Session{}, fmt.Errorf("encode forked task request: %w", err)
	}
	newExecutionID, err := newID()
	if err != nil {
		return Session{}, err
	}
	newQueueID, err := newID()
	if err != nil {
		return Session{}, err
	}
	forkedSession, err := c.createSession(ctx, saved.History, saved.Settings)
	if err != nil {
		return Session{}, err
	}
	newRunID := c.runtime.checkpointID(forkedSession.ID)
	rollback := func(cause error) (Session, error) {
		if deleteErr := c.checkpoints.Delete(context.Background(), newRunID); deleteErr != nil && !errors.Is(deleteErr, checkpoint.ErrNotFound) {
			cause = errors.Join(cause, deleteErr)
		}
		cause = errors.Join(cause, c.sessions.DeleteToolInvocationsForSession(context.Background(), forkedSession.ID))
		cause = errors.Join(cause, c.sessions.DeleteGraphResumeInputs(context.Background(), forkedSession.ID, newRunID))
		if _, deleteErr := c.sessions.Delete(context.Background(), forkedSession.ID, c.workspace); deleteErr != nil {
			cause = errors.Join(cause, deleteErr)
		}
		return Session{}, cause
	}
	profileID := pending.ModelProfile
	if profileID == "" {
		profileID = saved.Settings.ModelProfile
	}
	forkRuntime, _, err := c.configureRuntime(profileID, pending.ReasoningEffort, TaskState{SessionID: forkedSession.ID, Execution: coreExecutionOptions(pending.Execution)})
	if err != nil {
		return rollback(fmt.Errorf("prepare paused task fork: %w", err))
	}
	forkedCheckpoint, err := forkRuntime.agent.Fork(ctx, newRunID, sourceCheckpoint)
	if err != nil {
		return rollback(fmt.Errorf("clone paused task checkpoint: %w", err))
	}
	for i := range forkedCheckpoint.Invocations {
		state := agentgraph.RekeyForkState(forkedCheckpoint.Invocations[i].State, newRunID, newExecutionID)
		state.Execution = pending.Execution
		forkedCheckpoint.Invocations[i].State = state
	}
	if forkedCheckpoint.Final != nil {
		state := agentgraph.RekeyForkState(*forkedCheckpoint.Final, newRunID, newExecutionID)
		state.Execution = pending.Execution
		forkedCheckpoint.Final = &state
	}
	if err := c.checkpoints.Create(ctx, forkedCheckpoint); err != nil {
		return rollback(fmt.Errorf("save forked task checkpoint: %w", err))
	}
	var budgetReason string
	result, found, resultErr := c.sessions.TurnResult(ctx, sessionID, pending.ExecutionID)
	if resultErr != nil {
		return rollback(fmt.Errorf("read paused turn result: %w", resultErr))
	}
	if found {
		budgetReason = result.BudgetReason
	}
	event := Event{Kind: EventTaskFinished, SessionID: forkedSession.ID, QueueID: newQueueID, ExecutionID: newExecutionID,
		Status: TaskPaused, BudgetReason: BudgetReason(budgetReason)}
	if err := c.sessions.ForkPausedTask(ctx, sessionID, sourceRunID, pending.ExecutionID, forkedSession.ID, newRunID,
		newExecutionID, newQueueID, requestJSON, mustMarshalEvent(event)); err != nil {
		return rollback(fmt.Errorf("copy paused task receipts: %w", err))
	}
	return forkedSession, nil
}

func (c *Core) LatestSession(ctx context.Context) (Session, bool, error) {
	if c == nil || ctx == nil {
		return Session{}, false, errors.New("core and context are required")
	}
	snapshot, err := c.sessions.Latest(ctx, c.workspace)
	if err != nil || snapshot == nil {
		return Session{}, false, err
	}
	saved, err := c.decodeSession(ctx, *snapshot)
	return saved, err == nil, err
}

func (c *Core) ListSessions(ctx context.Context, limit int, includeArchived bool) ([]Session, error) {
	if c == nil || ctx == nil {
		return nil, errors.New("core and context are required")
	}
	snapshots, err := c.sessions.List(ctx, c.workspace, limit, includeArchived)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(snapshots))
	for _, snapshot := range snapshots {
		saved, err := c.decodeSession(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, saved)
	}
	return sessions, nil
}

func (c *Core) SetSessionTitle(ctx context.Context, sessionID, title string) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > 120 {
		return errors.New("session title must be at most 120 characters")
	}
	return c.sessions.UpdateTitle(ctx, sessionID, c.workspace, title)
}

func (c *Core) SetSessionArchived(ctx context.Context, sessionID string, archived bool) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	return c.sessions.SetArchived(ctx, sessionID, c.workspace, archived)
}

func (c *Core) DeleteSession(ctx context.Context, sessionID string) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		return errors.New("cannot delete a session while its task is running")
	}
	if c.queueDraining[sessionID] {
		return errors.New("cannot delete a session while queued work is starting")
	}
	// Save-based usage/settings updates must not recreate a session after its
	// row has been deleted. The session row is removed last for retry safety.
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return err
	}
	queuedTurns, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return err
	}
	parentID := c.runtime.checkpointID(sessionID)
	delete(c.liveExecutions, parentID)
	if err := c.checkpoints.Delete(ctx, parentID); err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
		return fmt.Errorf("delete task checkpoint for session %q: %w", sessionID, err)
	}
	if err := c.sessions.DeleteToolInvocationsForSession(ctx, sessionID); err != nil {
		return err
	}
	if err := c.sessions.DeleteGraphResumeInputs(ctx, sessionID, parentID); err != nil {
		return err
	}
	deleted, err := c.sessions.Delete(ctx, sessionID, c.workspace)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("session %q does not exist in workspace", sessionID)
	}
	for _, queued := range queuedTurns {
		if waiter := c.turnWaiters[queued.ExecutionID]; waiter != nil {
			delete(c.turnWaiters, queued.ExecutionID)
			waiter <- turnResult{err: errors.New("session was deleted before the turn started")}
		}
	}
	if hub := c.eventHubs[sessionID]; hub != nil {
		hub.close()
	}
	delete(c.interactiveSessions, sessionID)
	delete(c.eventHubs, sessionID)
	return nil
}

func (c *Core) RewindSession(ctx context.Context, sessionID string, userTurns int) (int, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return 0, errors.New("core, context, and session id are required")
	}
	if userTurns <= 0 {
		return 0, errors.New("rewind count must be greater than zero")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		return 0, errors.New("cannot rewind a session while its task is running")
	}
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if len(queued) > 0 {
		return 0, errors.New("cannot rewind a session while turns are queued")
	}
	pending, err := c.hasPending(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if pending {
		return 0, errors.New("cannot rewind a session while a task is paused")
	}
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	var history model.ResponseInput
	if len(saved.History) > 0 && string(saved.History) != "null" {
		if err := json.Unmarshal(saved.History, &history); err != nil {
			return 0, fmt.Errorf("decode session history: %w", err)
		}
	}
	items := responseHistoryItems(&history)
	var starts []int
	for index, item := range items {
		if message, ok := item.(model.InputMessage); ok && message.Role == model.RoleUser {
			starts = append(starts, index)
		}
	}
	if len(starts) == 0 {
		return 0, nil
	}
	removed := min(userTurns, len(starts))
	cut := starts[len(starts)-removed]
	encoded, err := json.Marshal(model.NewResponseInputItems(items[:cut]...))
	if err != nil {
		return 0, fmt.Errorf("encode rewound history: %w", err)
	}
	if err := c.SetSessionHistory(ctx, sessionID, encoded); err != nil {
		return 0, err
	}
	return removed, nil
}

func (c *Core) GetSession(ctx context.Context, id string) (Session, error) {
	if c == nil || ctx == nil || id == "" {
		return Session{}, errors.New("core, context, and session id are required")
	}
	snapshot, err := c.sessions.Get(ctx, id, c.workspace)
	if err != nil {
		return Session{}, err
	}
	return c.decodeSession(ctx, *snapshot)
}

func (c *Core) SetSessionHistory(ctx context.Context, id string, history json.RawMessage) error {
	if c == nil || ctx == nil || id == "" {
		return errors.New("core, context, and session id are required")
	}
	if len(history) == 0 || !json.Valid(history) {
		return errors.New("session history must be valid JSON")
	}
	if _, err := model.NewResponseInput(history); err != nil {
		return err
	}
	return c.sessions.UpdateHistory(ctx, id, c.workspace, history)
}

func (c *Core) setSessionConversation(ctx context.Context, id string, transcript, modelContext *model.ResponseInput, contextKey string) error {
	if c == nil || ctx == nil || id == "" {
		return errors.New("core, context, and session id are required")
	}
	if transcript == nil {
		transcript = model.NewResponseInputItems()
	}
	transcriptJSON, err := json.Marshal(transcript)
	if err != nil {
		return fmt.Errorf("encode session transcript: %w", err)
	}
	var contextJSON json.RawMessage = json.RawMessage("null")
	contextKeyToSave := contextKey
	if modelContext != nil {
		contextJSON, err = json.Marshal(modelContext)
		if err != nil {
			return fmt.Errorf("encode provider model context: %w", err)
		}
		if bytes.Equal(contextJSON, transcriptJSON) {
			// Avoid storing the same potentially large inline image payload twice.
			// A task rebuilds this provider context from History when it is absent.
			contextJSON = json.RawMessage("null")
			contextKeyToSave = ""
		}
	}
	return c.sessions.UpdateConversation(ctx, id, c.workspace, transcriptJSON, contextJSON, contextKeyToSave)
}

func (c *Core) hasPending(ctx context.Context, sessionID string) (bool, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return false, errors.New("core, context, and session id are required")
	}
	return c.runtime.HasPendingFor(sessionID, ctx)
}

func (c *Core) Compact(ctx context.Context, sessionID string) (int, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return 0, errors.New("core, context, and session id are required")
	}
	c.runtimeConfigMu.RLock()
	defer c.runtimeConfigMu.RUnlock()
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return 0, errors.New("core is closed")
	}
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	var transcript *model.ResponseInput
	if len(saved.History) > 0 {
		var decoded model.ResponseInput
		if err := json.Unmarshal(saved.History, &decoded); err != nil {
			return 0, err
		}
		transcript = &decoded
	}
	settings, err := c.normalizeSessionSettings(saved.Settings)
	if err != nil {
		return 0, err
	}
	effective, err := c.resolveTaskSettings(settings)
	if err != nil {
		return 0, err
	}
	profile, _ := c.models.Profile(settings.ModelProfile)
	providerConfig, ok := c.models.ProviderConfig(profile.Provider)
	if !ok {
		return 0, fmt.Errorf("provider %q is not configured", profile.Provider)
	}
	contextKey := modelContextKey(profile.Provider, profile.Model, providerConfig)
	modelContext := transcript
	if saved.ModelContextKey == contextKey && hasSessionHistory(saved.ModelContext) {
		var decoded model.ResponseInput
		if err := json.Unmarshal(saved.ModelContext, &decoded); err != nil {
			return 0, fmt.Errorf("decode provider model context: %w", err)
		}
		modelContext = &decoded
	}
	compactOptions := agentgraph.TaskOptions{
		ReasoningEffort: settings.ReasoningEffort, Goal: effective.Goal,
		ContextWindow: effective.ContextWindow, ReserveOutputTokens: effective.ReserveOutputTokens,
		AutoCompact: effective.AutoCompact, AutoCompactThreshold: effective.AutoCompactThreshold,
	}
	taskID, err := c.beginTask(ctx, sessionID, "", false, settings, profile, profile.Model, compactOptions, saved.UpdatedAt, nil)
	if err != nil {
		return 0, err
	}
	defer c.endTask(sessionID, taskID, nil)
	taskState, _ := c.StateForSession(sessionID)
	ctx = publictool.WithCallIdentity(ctx, publictool.CallIdentity{
		SessionID: sessionID, TaskID: taskState.TaskID, RunID: c.runtime.checkpointID(sessionID), AgentID: "lead",
	})
	taskRuntime, _, err := c.configureRuntime(settings.ModelProfile, settings.ReasoningEffort, taskState)
	if err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return 0, err
	}
	if err := c.resetTaskUsage(ctx, sessionID); err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return 0, err
	}
	taskRuntime.SetSessionState(sessionID, transcript, modelContext, contextKey)
	compacted, err := taskRuntime.Compact(ctx)
	if err == nil {
		err = c.saveHistoryUsage(ctx, sessionID, taskRuntime)
	}
	status := TaskCompleted
	if err != nil {
		status = TaskFailed
		if ctx.Err() != nil {
			status = TaskPaused
		}
	}
	c.finishTaskState(sessionID, status, err)
	if err != nil {
		return compacted, err
	}
	return compacted, nil
}

func (c *Core) resetTaskUsage(ctx context.Context, sessionID string) error {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	saved.TaskUsage = UsageTotals{}
	saved.UpdatedAt = time.Now()
	return c.saveSession(ctx, saved)
}

func (c *Core) saveHistoryUsage(ctx context.Context, sessionID string, runtime *graphResponder) error {
	return c.setSessionConversation(ctx, sessionID, runtime.SessionHistory(), runtime.ModelContextHistory(), runtime.ContextKey())
}

func modelContextKey(providerName, effectiveModel string, providerConfig model.ProviderConfig) string {
	identity, _ := json.Marshal(struct {
		Provider         string `json:"provider"`
		Protocol         string `json:"protocol"`
		BaseURL          string `json:"base_url"`
		Model            string `json:"model"`
		ServerCompaction bool   `json:"server_compaction"`
	}{
		Provider: providerName, Protocol: providerConfig.Protocol, BaseURL: providerConfig.BaseURL,
		Model: effectiveModel, ServerCompaction: providerConfig.ServerCompaction,
	})
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:])
}

func (c *Core) recordUsage(ctx context.Context, id string, usage Usage) error {
	return c.recordUsageForAgent(ctx, id, usage, "", "", "", "", 0, 0)
}

func (c *Core) recordUsageForAgent(ctx context.Context, id string, usage Usage, agentID, profile, provider, modelID string, contextWindow, estimatedInputTokens int64) error {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	saved, err := c.GetSession(ctx, id)
	if err != nil {
		return err
	}
	available := usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.OutputTokensDetails.ReasoningTokens != 0 || usage.TotalTokens != 0 ||
		usage.InputTokensDetails.CachedTokens != 0 || usage.InputTokensDetails.CacheCreationTokens != 0
	record := UsageRecord{
		Request: saved.SessionUsage.Requests + 1, Usage: usage, Available: available,
		ContextWindow: contextWindow, EstimatedInputTokens: estimatedInputTokensIfMissing(usage.InputTokens, estimatedInputTokens),
		AgentID: agentID, ModelProfile: profile, Provider: provider, Model: modelID,
	}
	saved.UsageRequests = append(saved.UsageRequests, record)
	if len(saved.UsageRequests) > 200 {
		saved.UsageRequests = append([]UsageRecord(nil), saved.UsageRequests[len(saved.UsageRequests)-200:]...)
	}
	if saved.AgentUsage == nil {
		saved.AgentUsage = make(map[string]UsageTotals)
	}
	agentTotals := saved.AgentUsage[agentID]
	addUsage(&agentTotals, usage, available)
	estimatedInput := estimatedInputTokensIfMissing(usage.InputTokens, estimatedInputTokens)
	agentTotals.EstimatedInput += estimatedInput
	saved.AgentUsage[agentID] = agentTotals
	addUsage(&saved.SessionUsage, usage, available)
	addUsage(&saved.TaskUsage, usage, available)
	saved.SessionUsage.EstimatedInput += estimatedInput
	saved.TaskUsage.EstimatedInput += estimatedInput
	saved.UpdatedAt = time.Now()
	return c.saveSession(ctx, saved)
}

func estimatedInputTokensIfMissing(inputTokens, estimatedInputTokens int64) int64 {
	if inputTokens > 0 || estimatedInputTokens <= 0 {
		return 0
	}
	return estimatedInputTokens
}

func addUsage(totals *UsageTotals, usage Usage, available bool) {
	totals.Requests++
	if !available {
		return
	}
	totals.Measured++
	totals.Input += usage.InputTokens
	totals.Output += usage.OutputTokens
	totals.Reasoning += usage.OutputTokensDetails.ReasoningTokens
	totals.Total += usage.TotalTokens
	totals.Cached += usage.InputTokensDetails.CachedTokens
	totals.CacheCreation += usage.InputTokensDetails.CacheCreationTokens
}

func (c *Core) saveSession(ctx context.Context, saved Session) error {
	var err error
	saved, _, err = migrateSessionData(saved)
	if err != nil {
		return err
	}
	if saved.UpdatedAt.IsZero() {
		saved.UpdatedAt = time.Now()
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	return c.sessions.Save(ctx, session.Snapshot{ID: saved.ID, Workspace: c.workspace, UpdatedAt: saved.UpdatedAt, Data: data})
}

func (c *Core) decodeSession(ctx context.Context, snapshot session.Snapshot) (Session, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var saved Session
		if err := json.Unmarshal(snapshot.Data, &saved); err != nil {
			return Session{}, fmt.Errorf("decode session %q: %w", snapshot.ID, err)
		}
		saved.ID, saved.Workspace, saved.UpdatedAt = snapshot.ID, snapshot.Workspace, snapshot.UpdatedAt
		migrated, changed, err := migrateSessionData(saved)
		if err != nil {
			return Session{}, fmt.Errorf("migrate session %q: %w", snapshot.ID, err)
		}
		saved = migrated
		if len(saved.History) == 0 {
			saved.History = json.RawMessage("null")
		}
		if saved.Settings.ModelProfile == "" {
			saved.Settings.ModelProfile = c.models.DefaultModel()
		}
		if !changed {
			return saved, nil
		}
		encoded, err := json.Marshal(saved)
		if err != nil {
			return Session{}, fmt.Errorf("encode migrated session %q: %w", snapshot.ID, err)
		}
		updated, err := c.sessions.ReplaceDataIfUnchanged(ctx, snapshot.ID, snapshot.Workspace, snapshot.Data, encoded)
		if err != nil {
			return Session{}, fmt.Errorf("persist session %q migration: %w", snapshot.ID, err)
		}
		if updated {
			return saved, nil
		}
		latest, err := c.sessions.Get(ctx, snapshot.ID, snapshot.Workspace)
		if err != nil {
			return Session{}, fmt.Errorf("reload session %q during migration: %w", snapshot.ID, err)
		}
		snapshot = *latest
	}
	return Session{}, fmt.Errorf("session %q changed repeatedly during data migration", snapshot.ID)
}

func (c *Core) migrateLegacySession(ctx context.Context) error {
	latest, err := c.sessions.Latest(ctx, c.workspace)
	if err != nil || latest != nil {
		return err
	}
	if _, err := c.checkpoints.Load(ctx, legacyRunID); err != nil {
		if errors.Is(err, checkpoint.ErrNotFound) {
			return nil
		}
		return err
	}
	history, err := c.runtime.PendingHistoryFor(ctx, legacyRunID)
	if err != nil {
		return err
	}
	var encoded json.RawMessage = json.RawMessage("null")
	if history != nil {
		items := responseHistoryItems(history)
		cut := len(items)
		for index := len(items) - 1; index >= 0; index-- {
			message, ok := items[index].(model.InputMessage)
			if ok && message.Role == model.RoleUser {
				cut = index
				break
			}
		}
		prior := model.NewResponseInputItems(items[:cut]...)
		encoded, err = json.Marshal(prior)
		if err != nil {
			return err
		}
	}
	saved := Session{
		ID: legacyRunID, Workspace: c.workspace, UpdatedAt: time.Now(),
		Settings: SessionSettings{ModelProfile: c.models.DefaultModel()}, History: encoded,
	}
	return c.saveSession(ctx, saved)
}

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
