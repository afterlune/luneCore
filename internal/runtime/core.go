// Package runtime implements the embedded agent runtime exposed by Core.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	commandruntime "github.com/afterlune/luneCore/internal/command"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/mcp"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneCore/internal/tool"
	"github.com/afterlune/luneCore/internal/workspacememory"
	publicmemory "github.com/afterlune/luneCore/memory"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
	"github.com/afterlune/luneGraph/checkpoint/sqlite"
)

// Config constructs an embedded runtime. ProjectConfig, when supplied, is the
// complete project configuration and prevents config.json from being read.
// Without it, project settings are loaded from config.json. ModelConfig and
// MaxParallelAgents overlay the matching catalog and runtime values.
type Config struct {
	Workspace string
	StateDir  string
	// Logger receives structured logs from this Core and its internal components.
	// When nil, New captures slog.Default() for the Core's lifetime.
	Logger            *slog.Logger
	ProjectConfig     *projectconfig.ProjectConfig
	ModelConfig       *publicmodel.Config
	MaxParallelAgents int
	// RestoreEnabledMCP restores and persists enabled MCP servers across Core
	// instances. Leave disabled when workspace configuration or state is
	// controlled by the project rather than the host.
	RestoreEnabledMCP bool
	ModelProviders    map[string]publicmodel.Provider
	Tools             []publictool.Registration
	CommandExecutor   publictool.CommandExecutor
	Commands          CommandOptions
	// CommandToolchainPaths grants command sandboxes read and execute access to
	// host-owned toolchain directories. It is intentionally empty by default and
	// is not loaded from the shared ProjectConfig.
	CommandToolchainPaths []string
	// SandboxHelperPath optionally selects the lune-sandbox companion binary.
	// When empty, Core searches beside the host executable and then PATH.
	SandboxHelperPath string
	// Deprecated: local commands now always run through the kernel sandbox.
	AllowLocalCommand bool
	Execution         ExecutionOptions
	// PersonalInstructions supplies user-level guidance shared by this Core's sessions.
	// New tasks snapshot the text for recovery; changes require creating a new Core.
	PersonalInstructions string
	Memory               MemoryConfig
	// MaxConcurrentCallbacks caps callback work across all sessions in this Core.
	MaxConcurrentCallbacks int
	// BrowserOpener customizes how authorization and interaction URLs are opened.
	// When nil, Core defaults to the host operating system browser.
	BrowserOpener mcp.BrowserOpener
	// CredentialStorage customizes where OAuth credentials are saved and loaded.
	// When nil, Core defaults to the operating system keyring.
	CredentialStorage mcp.CredentialStorage
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
	stateDir             string
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
	personalInstructions string
	memoryConfig         MemoryConfig
	memory               *workspacememory.Service
	// liveExecutions keeps warm Graph handles for Advance; checkpoints remain
	// the recovery source after process restart.
	liveExecutions map[string]*graph.Execution[agentgraph.State]
	sessionLocks   map[string]*sessionLockEntry
	coord          *sessionCoordinator

	mu                  sync.RWMutex
	eventHubMu          sync.Mutex
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
	queueMutationMu     sync.Mutex
	turnWaiters         map[string]chan turnResult
	executionRuns       map[string]chan struct{}
	interruptRequests   map[string]bool
	queueDrainWG        sync.WaitGroup
	sessionOps          sync.WaitGroup
	closed              bool
	closing             bool
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
	config.CommandToolchainPaths, err = normalizeCommandToolchainPaths(config.CommandToolchainPaths)
	if err != nil {
		return nil, err
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
	projectConfig, err := projectconfig.LoadForRuntime(workspace, config.ModelConfig, config.ModelProviders, config.ProjectConfig, config.MaxParallelAgents)
	if err != nil {
		return nil, err
	}
	hostTools, err := tool.HostToolRegistrations(config.Tools)
	if err != nil {
		return nil, err
	}
	registry, err := tool.NewDefaultRegistry(workspace, tool.RegistryOptions{
		Tools: hostTools, CommandExecutor: config.CommandExecutor, AllowLocalCommands: config.AllowLocalCommand,
		SandboxHelperPath: config.SandboxHelperPath, CommandToolchainPaths: config.CommandToolchainPaths,
		ImageMaxBytes: projectConfig.Attachments.ImageMaxBytes(), Logger: logger,
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
	executionDefaults := config.Execution
	if executionDefaults.MaxTeamChildrenPerAgent == 0 {
		executionDefaults.MaxTeamChildrenPerAgent = projectConfig.Runtime.MaxTeamChildrenPerAgent
	}
	if executionDefaults.MaxTeamChildrenPerAgent == 0 {
		executionDefaults.MaxTeamChildrenPerAgent = agentgraph.DefaultMaxTeamChildrenPerAgent
	}
	if executionDefaults.MaxTeamDepth == 0 {
		executionDefaults.MaxTeamDepth = projectConfig.Runtime.MaxTeamDepth
	}
	if executionDefaults.MaxTeamDepth == 0 {
		executionDefaults.MaxTeamDepth = agentgraph.DefaultMaxTeamDepth
	}
	coord := newSessionCoordinator()
	c := &Core{
		logger:           logger,
		workspace:        workspace,
		stateDir:         stateDir,
		registry:         registry,
		models:           models,
		attachmentLimits: projectConfig.Attachments,
		taskDefaults: taskSettings{
			ContextWindow: projectConfig.Runtime.ContextWindow, ReserveOutputTokens: projectConfig.Runtime.ReserveOutputTokens,
			AutoCompact: projectConfig.Runtime.AutoCompact, AutoCompactThreshold: projectConfig.Runtime.AutoCompactThreshold,
		},
		checkpoints:          checkpoints,
		sessions:             sessions,
		executionDefaults:    executionDefaults,
		callbackLimiter:      callbackLimiter,
		personalInstructions: config.PersonalInstructions,
		memoryConfig:         config.Memory,
		memory:               workspacememory.New(sessions, workspace, config.Memory.Embedder, logger),
		liveExecutions:       make(map[string]*graph.Execution[agentgraph.State]),
		coord:                coord,
		active:               TaskState{Status: TaskIdle},
		activeTasks:          coord.activeTasks,
		taskCancels:          coord.taskCancels,
		approvals:            coord.approvals,
		interactions:         coord.interactions,
		interactiveSessions:  coord.interactiveSessions,
		eventHubs:            make(map[string]*sessionEventHub),
		queueDraining:        coord.queueDraining,
		turnWaiters:          coord.turnWaiters,
		executionRuns:        coord.executionRuns,
		interruptRequests:    coord.interruptRequests,
		sessionLocks:         make(map[string]*sessionLockEntry),
	}
	if err := c.configureCommands(config); err != nil {
		return nil, err
	}
	if !c.memoryConfig.Disabled {
		registrations, err := tool.HostToolRegistrations([]publictool.Registration{{
			Tool: workspacememory.NewSearchTool(c.memory),
			Metadata: publictool.Metadata{
				Category:        publictool.Category{Name: "workspace_memory", Description: "Search saved workspace memories from earlier tasks."},
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
		RestoreEnabled:     config.RestoreEnabledMCP,
		Browser:            config.BrowserOpener,
		Credentials:        config.CredentialStorage,
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
	if config.RestoreEnabledMCP {
		for _, restoreErr := range mcpManager.RestoreEnabled() {
			c.logger.Warn("could not restore enabled MCP server", "error_type", fmt.Sprintf("%T", restoreErr))
		}
	}
	team := &teamExecutor{core: c, models: models, tools: registry}
	c.team = team
	c.runtime = agentruntime.NewResponder(checkpoints, callbackLimiter, c.logger)
	registry.SetToolEventObserver(c.onToolEvent)
	registry.SetToolCallController(c.executeToolCall)
	registry.SetPermissionApprovalHandler(c.requestApproval)
	closeCheckpoints, closeSessions, closeRegistry, closeMCPManager = false, false, false, false
	return c, nil
}

// MaxParallelAgents returns the maximum parallel agent limit for team delegations.
func (c *Core) MaxParallelAgents() int {
	if c.team == nil {
		return 0
	}
	return c.team.MaxParallelAgents()
}

// ExecutionDefaults returns the resolved host and project execution limits.
// A running or paused task keeps its own effective limits in TaskState.Execution.
func (c *Core) ExecutionDefaults() ExecutionOptions {
	if c == nil {
		return ExecutionOptions{}
	}
	return c.executionDefaults
}
