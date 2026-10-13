// Package core exposes the agent runtime without terminal presentation details.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
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
	runtime           *agentruntime.Responder
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
	c.runtime = agentruntime.NewResponder(checkpoints, callbackLimiter)
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

func (c *Core) buildAgent(profileID string, taskState TaskState) (*agentgraph.Agent, *agentruntime.UsageTrackingRequester, model.ModelProfile, error) {
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
	requester := agentruntime.NewUsageTrackingRequester(client, "lead", profileID, profile.Provider, profile.Model)
	requester.SetContextAgentUsageObserver(c.onAgentUsageContext)
	agent, err := agentgraph.NewWithOptions(requester, c.registry, agentgraph.AgentOptions{
		ID: "lead", Name: "Lead", Profile: profileID, Provider: profile.Provider, Model: profile.Model,
		Protocol: providerConfig.Protocol, ServerCompaction: providerConfig.ServerCompaction,
		SupportsInputAudio: profile.SupportsInputAudio, Execution: agentExecutionOptions(taskState.Execution),
		Team:        c.team,
		AskUser:     true,
		ToolVisible: c.registry.ToolVisible,
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

func (c *Core) configureRuntime(profileID, reasoningEffort string, taskState TaskState) (*agentruntime.Responder, model.ModelProfile, error) {
	agent, requester, profile, err := c.buildAgent(profileID, taskState)
	if err != nil {
		return nil, model.ModelProfile{}, err
	}
	runtime := agentruntime.NewResponder(c.checkpoints, c.callbackLimiter)
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
