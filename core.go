// Package core exposes the agent runtime without terminal presentation details.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/afterlune/luneCore/acceptance"
	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	commandruntime "github.com/afterlune/luneCore/internal/command"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/mcp"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneCore/internal/tool"
	publicmemory "github.com/afterlune/luneCore/memory"
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
	Workspace string
	StateDir  string
	// Logger receives structured logs from this Core and its internal components.
	// When nil, New captures slog.Default() for the Core's lifetime.
	Logger          *slog.Logger
	ModelConfig     *publicmodel.Config
	ModelProviders  map[string]publicmodel.Provider
	Tools           []publictool.Registration
	CommandExecutor publictool.CommandExecutor
	Commands        CommandOptions
	// SandboxHelperPath optionally selects the lune-sandbox companion binary.
	// When empty, Core searches beside the host executable and then PATH.
	SandboxHelperPath string
	// Deprecated: local commands now always run through the kernel sandbox.
	AllowLocalCommand bool
	Execution         ExecutionOptions
	// AcceptanceVerifier optionally overrides the model's acceptance judgment.
	// Implementations should be idempotent, read-only, and honor ctx cancellation.
	AcceptanceVerifier acceptance.Verifier
	// PersonalInstructions supplies user-level guidance shared by this Core's sessions.
	// New tasks snapshot the text for recovery; changes require creating a new Core.
	PersonalInstructions string
	Memory               MemoryConfig
	// MaxConcurrentCallbacks caps callback work across all sessions in this Core.
	MaxConcurrentCallbacks int
}

// MemoryConfig controls automatic workspace memory. Memory is enabled by
// default; set Disabled to turn off extraction, context injection, and search.
// Embedder is optional and enables vector-assisted retrieval.
type MemoryConfig struct {
	Disabled bool
	Embedder publicmemory.Embedder
}

type Core struct {
	commands             *commandruntime.Manager
	logger               *slog.Logger
	workspace            string
	registry             *tool.Registry
	models               *model.ProviderCatalog
	taskDefaults         taskSettings
	attachmentLimits     projectconfig.AttachmentLimits
	team                 *teamExecutor
	runtime              *agentruntime.Responder
	mcpManager           *mcp.Manager
	checkpoints          *sqlite.Store[agentgraph.State]
	sessions             *session.Store
	executionDefaults    ExecutionOptions
	callbackLimiter      *graph.Limiter
	acceptanceVerifier   acceptance.Verifier
	personalInstructions string
	memoryConfig         MemoryConfig
	// liveExecutions keeps warm Graph handles for Advance; checkpoints remain
	// the recovery source after process restart.
	liveExecutions map[string]*graph.Execution[agentgraph.State]
	stateDirLock   *session.DirectoryLock

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
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if config.Commands.Timeout < 0 || config.Commands.MaxConcurrent < 0 {
		return nil, errors.New("command limits must not be negative")
	}
	if err := validateExecutionOptions(config.Execution); err != nil {
		return nil, fmt.Errorf("invalid execution defaults: %w", err)
	}
	if !config.Memory.Disabled && config.Memory.Embedder != nil && strings.TrimSpace(config.Memory.Embedder.ID()) == "" {
		return nil, errors.New("memory embedder ID must not be empty")
	}
	for _, registration := range config.Tools {
		if registration.Tool != nil && (registration.Tool.Definition().Name == publicmemory.SearchToolName || registration.Tool.Definition().Name == "command" || registration.Tool.Definition().Name == "command_status" || registration.Tool.Definition().Name == "command_stop") {
			return nil, fmt.Errorf("tool name is reserved: %s", registration.Tool.Definition().Name)
		}
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
	stateLock, err := session.AcquireStateDirectoryLock(stateDir)
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
	projectConfig, err := projectconfig.LoadForRuntime(workspace, config.ModelConfig, config.ModelProviders)
	if err != nil {
		return nil, err
	}
	hostTools, err := tool.HostToolRegistrations(config.Tools)
	if err != nil {
		return nil, err
	}
	registry, err := tool.NewDefaultRegistry(workspace, tool.RegistryOptions{
		Tools: hostTools, CommandExecutor: config.CommandExecutor, AllowLocalCommands: config.AllowLocalCommand,
		SandboxHelperPath: config.SandboxHelperPath, Logger: logger,
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
	checkpoints, err := sqlite.Open[agentgraph.State](context.Background(), filepath.Join(stateDir, "checkpoints-v10.db"), checkpoint.JSON[agentgraph.State]{})
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
		logger:           logger,
		workspace:        workspace,
		registry:         registry,
		models:           models,
		attachmentLimits: projectConfig.Attachments,
		taskDefaults: taskSettings{
			ContextWindow: projectConfig.ContextWindow, ReserveOutputTokens: projectConfig.ReserveOutputTokens,
			AutoCompact: projectConfig.AutoCompact, AutoCompactThreshold: projectConfig.AutoCompactThreshold,
		},
		checkpoints:          checkpoints,
		sessions:             sessions,
		executionDefaults:    config.Execution,
		callbackLimiter:      callbackLimiter,
		acceptanceVerifier:   config.AcceptanceVerifier,
		personalInstructions: config.PersonalInstructions,
		memoryConfig:         config.Memory,
		liveExecutions:       make(map[string]*graph.Execution[agentgraph.State]),
		active:               TaskState{Status: TaskIdle},
		activeTasks:          make(map[string]TaskState),
		taskCancels:          make(map[string]context.CancelFunc),
		approvals:            make(map[string]pendingApproval),
		interactions:         make(map[string]pendingInteraction),
		interactiveSessions:  make(map[string]bool),
		eventHubs:            make(map[string]*sessionEventHub),
		queueDraining:        make(map[string]bool),
		turnWaiters:          make(map[string]chan turnResult),
		executionRuns:        make(map[string]chan struct{}),
		interruptRequests:    make(map[string]bool),
		stateDirLock:         stateLock,
	}
	if err := c.configureCommands(config); err != nil {
		return nil, err
	}
	if !c.memoryConfig.Disabled {
		registrations, err := tool.HostToolRegistrations([]publictool.Registration{{
			Tool: memorySearchTool{core: c},
			Metadata: publictool.Metadata{
				Category:        publictool.Category{Name: "workspace_memory", Description: "Search saved workspace memories from earlier accepted tasks."},
				PermissionClass: publictool.PermissionWorkspaceRead,
				ReadOnly:        true,
			},
		}})
		if err != nil {
			return nil, fmt.Errorf("prepare workspace memory search tool: %w", err)
		}
		if err := registry.Register(registrations[0]); err != nil {
			return nil, fmt.Errorf("register workspace memory search tool: %w", err)
		}
	}
	attachmentLimits := c.ProjectAttachmentLimits()
	mcpManager, err := mcp.NewManager(mcp.Options{
		Workspace: workspace, StateDir: stateDir, Servers: projectConfig.MCPServers, Registry: registry,
		Limits:             mcp.Limits{MaxBinaryBlocks: attachmentLimits.MaxFiles, MaxFileBytes: attachmentLimits.MaxFileBytes, MaxTotalBytes: attachmentLimits.MaxTotalBytes},
		Logger:             logger,
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
		c.logger.Warn("could not restore enabled MCP server", "error_type", fmt.Sprintf("%T", restoreErr))
	}
	team := &teamExecutor{core: c, models: models, tools: registry}
	c.team = team
	c.runtime = agentruntime.NewResponder(checkpoints, callbackLimiter, c.logger)
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
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 35*time.Second)
	cleanupErr := c.commands.Close(cleanupCtx)
	cleanupCancel()
	if cleanupErr != nil {
		c.mu.Unlock()
		return cleanupErr
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
