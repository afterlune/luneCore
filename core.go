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
	"github.com/afterlune/luneGraph/checkpoint"
	"github.com/afterlune/luneGraph/checkpoint/sqlite"
)

const (
	legacyRunID                 = "lune-cli-active"
	DefaultAutoCompactThreshold = projectconfig.DefaultAutoCompactThreshold
	DefaultAttachmentMaxFiles   = projectconfig.DefaultAttachmentMaxFiles
)

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

const (
	TaskIdle      TaskStatus = "idle"
	TaskRunning   TaskStatus = "running"
	TaskPaused    TaskStatus = "paused"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
)

type EventKind string

const (
	EventTaskStarted              EventKind = "task_started"
	EventTaskFinished             EventKind = "task_finished"
	EventToolStarted              EventKind = "tool_started"
	EventToolFinished             EventKind = "tool_finished"
	EventUsage                    EventKind = "usage"
	EventApprovalRequired         EventKind = "approval_required"
	EventApprovalCancelled        EventKind = "approval_cancelled"
	EventUserInteractionRequired  EventKind = "user_interaction_required"
	EventUserInteractionCancelled EventKind = "user_interaction_cancelled"
	EventAgentStarted             EventKind = "agent_started"
	EventAgentFinished            EventKind = "agent_finished"
	EventModelStream              EventKind = "model_stream"
	EventContextCompaction        EventKind = "context_compaction"
)

// Event is a structured runtime notification. Arguments and response payloads
// are transient; clients should not persist or log them without sanitizing.
type Event struct {
	Kind                 EventKind
	TaskID               string
	SessionID            string
	Status               TaskStatus
	ContextWindow        int64
	EstimatedInputTokens int64
	Tool                 ToolCall
	Usage                Usage
	Approval             *ApprovalRequest
	ApprovalID           string
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
	ID        string
	AgentID   string
	Name      string
	Arguments json.RawMessage
	Duration  time.Duration
	Started   bool
	Failed    bool
	Canceled  bool
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

type userInteractionResult struct {
	Response json.RawMessage
	Err      error
}

type EventHandler func(Event)

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
}

type TaskState struct {
	Status          TaskStatus
	SessionID       string
	TaskID          string
	ContextWindow   int64
	PlanMode        bool
	ModelProfile    string
	Provider        string
	Model           string
	ReasoningEffort string
}

type Config struct {
	Workspace string
	StateDir  string
}

type Core struct {
	workspace        string
	registry         *tool.Registry
	models           *model.ProviderCatalog
	taskDefaults     taskSettings
	attachmentLimits projectconfig.AttachmentLimits
	teamRuns         *teamStore
	team             *teamExecutor
	runtime          *graphResponder
	mcpManager       *mcp.Manager
	checkpoints      *sqlite.Store[agentgraph.State]
	sessions         *session.Store

	mu                       sync.RWMutex
	usageMu                  sync.Mutex
	approvalQueueMu          sync.Mutex
	active                   TaskState
	handler                  EventHandler
	approvals                map[string]chan bool
	interactions             map[string]chan userInteractionResult
	userInteractionAvailable bool
	closed                   bool
}

func New(config Config) (_ *Core, err error) {
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
	registry, err := tool.NewDefaultRegistry(workspace)
	if err != nil {
		return nil, err
	}
	closeRegistry := true
	defer func() {
		if closeRegistry {
			_ = registry.Close()
		}
	}()
	projectConfig, err := projectconfig.Load(filepath.Join(workspace, projectconfig.FileName))
	if err != nil {
		return nil, err
	}
	models, err := model.NewProviderCatalog(projectConfig.ModelConfig())
	if err != nil {
		return nil, err
	}
	checkpoints, err := sqlite.Open[agentgraph.State](context.Background(), filepath.Join(stateDir, "checkpoints.db"), checkpoint.JSON[agentgraph.State]{})
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
	teamRuns, err := openTeamStore(filepath.Join(stateDir, "team_runs.db"))
	if err != nil {
		return nil, err
	}
	closeTeamRuns := true
	defer func() {
		if closeTeamRuns {
			_ = teamRuns.Close()
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
		teamRuns:     teamRuns,
		checkpoints:  checkpoints,
		sessions:     sessions,
		active:       TaskState{Status: TaskIdle},
		approvals:    make(map[string]chan bool),
		interactions: make(map[string]chan userInteractionResult),
	}
	attachmentLimits := c.ProjectAttachmentLimits()
	mcpManager, err := mcp.NewManager(mcp.Options{
		Workspace: workspace, StateDir: filepath.Join(workspace, ".luneagent"), Servers: projectConfig.MCPServers, Registry: registry,
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
	team := &teamExecutor{core: c, models: models, tools: registry, checkpoints: checkpoints, store: teamRuns}
	c.team = team
	c.runtime = &graphResponder{store: checkpoints}
	registry.SetToolEventObserver(c.onToolEvent)
	registry.SetPermissionApprovalHandler(c.requestApproval)
	if err := c.migrateLegacySession(context.Background()); err != nil {
		return nil, err
	}
	closeCheckpoints, closeSessions, closeTeamRuns, closeRegistry, closeMCPManager = false, false, false, false, false
	return c, nil
}

func (c *Core) buildAgent(profileID string) (*agentgraph.Agent, *usageTrackingRequester, model.ModelProfile, error) {
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
		observer: c.onAgentUsage,
	}
	agent, err := agentgraph.NewWithOptions(requester, c.registry, agentgraph.AgentOptions{
		ID: "lead", Name: "Lead", Profile: profileID, Provider: profile.Provider, Model: profile.Model,
		Protocol: providerConfig.Protocol, ServerCompaction: providerConfig.ServerCompaction,
		SupportsInputAudio: profile.SupportsInputAudio,
		Team:               c.team,
		StreamObserver: func(event model.ModelStreamEvent) {
			activeModel := c.State().Model
			if activeModel == "" {
				activeModel = profile.Model
			}
			c.onModelStream("lead", "Lead", profileID, profile.Provider, activeModel, event)
		},
		CompactionObserver: func(event agentgraph.ContextCompactionEvent) {
			active := c.State()
			details := &ContextCompactionDetails{
				Stage: event.Stage, Mode: event.Mode, BeforeTokens: event.BeforeTokens,
				AfterTokens: event.AfterTokens, Threshold: event.Threshold, Err: errorString(event.Error),
			}
			c.emit(Event{Kind: EventContextCompaction, TaskID: active.TaskID, SessionID: active.SessionID,
				Provider: profile.Provider, ModelProfile: profileID, Model: active.Model, Err: errorString(event.Error), Compaction: details})
		},
	})
	if err != nil {
		return nil, nil, model.ModelProfile{}, err
	}
	return agent, requester, profile, nil
}

func (c *Core) configureRuntime(profileID, reasoningEffort string) (model.ModelProfile, error) {
	agent, requester, profile, err := c.buildAgent(profileID)
	if err != nil {
		return model.ModelProfile{}, err
	}
	c.runtime.SetRunModel(agent, requester, profile.Model, reasoningEffort)
	return profile, nil
}

func (c *Core) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if c.active.Status == TaskRunning {
		c.mu.Unlock()
		return errors.New("cannot close core while a task is running")
	}
	c.closed = true
	for id, response := range c.approvals {
		delete(c.approvals, id)
		select {
		case response <- false:
		default:
		}
	}
	for id, response := range c.interactions {
		delete(c.interactions, id)
		select {
		case response <- userInteractionResult{Err: errors.New("core is closing")}:
		default:
		}
	}
	c.mu.Unlock()
	var closeMCP error
	if c.mcpManager != nil {
		closeMCP = c.mcpManager.Close()
	}
	return errors.Join(closeMCP, c.checkpoints.Close(), c.sessions.Close(), c.teamRuns.Close(), c.registry.Close())
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
	if c.State().Status == TaskRunning {
		return errors.New("cannot change MCP servers while a task is running")
	}
	return c.mcpManager.Enable(name)
}

func (c *Core) DisableMCPServer(name string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	if c.State().Status == TaskRunning {
		return errors.New("cannot change MCP servers while a task is running")
	}
	return c.mcpManager.Disable(name)
}

func (c *Core) AuthenticateMCPServer(ctx context.Context, name string) error {
	if c == nil || c.mcpManager == nil {
		return errors.New("MCP manager is unavailable")
	}
	if c.State().Status == TaskRunning {
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

// SetUserInteractionAvailable marks whether the active front end can answer
// generic user-input requests. Headless clients leave this disabled so MCP
// tools fail immediately instead of waiting forever.
func (c *Core) SetUserInteractionAvailable(available bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.userInteractionAvailable = available
	c.mu.Unlock()
}

func (c *Core) ResolveUserInteraction(id string, response json.RawMessage) error {
	if c == nil {
		return errors.New("core is nil")
	}
	if len(response) == 0 || !json.Valid(response) {
		return errors.New("user interaction response must be valid JSON")
	}
	c.mu.Lock()
	channel, ok := c.interactions[id]
	if ok {
		delete(c.interactions, id)
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("user interaction %q is no longer pending", id)
	}
	channel <- userInteractionResult{Response: append(json.RawMessage(nil), response...)}
	return nil
}

func (c *Core) requestUserInteraction(ctx context.Context, agentID, serverName string, request json.RawMessage) (json.RawMessage, error) {
	if agentID != "" && agentID != "lead" {
		return nil, errors.New("MCP user interaction from subagents is not supported")
	}
	c.mu.RLock()
	available, handler := c.userInteractionAvailable, c.handler
	c.mu.RUnlock()
	if !available || handler == nil {
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
	c.interactions[id] = channel
	c.mu.Unlock()
	c.emit(Event{Kind: EventUserInteractionRequired, SessionID: c.State().SessionID,
		Interaction: &UserInteractionRequest{ID: id, AgentID: agentID, ServerName: serverName, Request: append(json.RawMessage(nil), request...)}})
	select {
	case result := <-channel:
		return result.Response, result.Err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.interactions, id)
		c.mu.Unlock()
		c.emit(Event{Kind: EventUserInteractionCancelled, InteractionID: id})
		return nil, ctx.Err()
	}
}

func (c *Core) SetEventHandler(handler EventHandler) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()
}

func (c *Core) State() TaskState {
	if c == nil {
		return TaskState{Status: TaskFailed}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active
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
		PlanMode: state.PlanMode, MaxToolRounds: state.MaxToolRounds, Model: state.ModelOverride, ReasoningEffort: state.ReasoningEffort,
		Goal: state.Goal, ContextWindow: state.ContextWindow, ReserveOutputTokens: state.ReserveOutputTokens,
		AutoCompact: state.AutoCompact, AutoCompactThreshold: state.AutoCompactThreshold,
	}
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

func (c *Core) ResolveApproval(id string, allowed bool) error {
	if c == nil {
		return errors.New("core is nil")
	}
	c.mu.Lock()
	response, ok := c.approvals[id]
	if ok {
		delete(c.approvals, id)
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("approval request %q is no longer pending", id)
	}
	response <- allowed
	return nil
}

func (c *Core) RunTask(ctx context.Context, request TaskRequest) (json.RawMessage, error) {
	if c == nil || ctx == nil {
		return nil, errors.New("core and context are required")
	}
	if len(request.Input.Raw) == 0 || !json.Valid(request.Input.Raw) {
		return nil, errors.New("task input must be valid JSON")
	}
	input, err := model.NewResponseInput(request.Input.Raw)
	if err != nil {
		return nil, err
	}
	if err := validateTaskInputAttachments(input, c.attachmentLimits); err != nil {
		return nil, err
	}
	return c.run(ctx, request.SessionID, request.PlanMode, request.MaxToolRounds, request.Model, false, input)
}

func (c *Core) ResumeTask(ctx context.Context, sessionID string) (json.RawMessage, error) {
	return c.run(ctx, sessionID, false, 0, "", true, nil)
}

func (c *Core) run(ctx context.Context, sessionID string, planMode bool, maxRounds int, modelOverride string, resume bool, input *model.ResponseInput) (json.RawMessage, error) {
	if c == nil || ctx == nil {
		return nil, errors.New("core and context are required")
	}
	if sessionID == "" {
		return nil, errors.New("session id is required")
	}
	if maxRounds < 0 {
		return nil, errors.New("maximum tool rounds must not be negative")
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
		pendingState, ok, err = c.runtime.PendingStateFor(ctx, sessionID)
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
	if _, err := c.beginTask(ctx, sessionID, planMode, settings, profile, effectiveModel, taskOptions.ContextWindow, saved.UpdatedAt); err != nil {
		return nil, err
	}
	defer c.endTask()
	if _, err := c.configureRuntime(settings.ModelProfile, settings.ReasoningEffort); err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return nil, err
	}
	if !resume {
		if err := c.resetTaskUsage(ctx, sessionID); err != nil {
			c.finishTaskState(sessionID, TaskFailed, err)
			return nil, err
		}
	}
	c.runtime.SetSessionState(sessionID, transcript, modelContext, contextKey)
	c.runtime.SetTaskOptions(taskOptions)
	c.runtime.SetHistorySaver(func(id string, transcript, modelContext *model.ResponseInput, key string) error {
		return c.setSessionConversation(context.Background(), id, transcript, modelContext, key)
	})
	defer c.runtime.SetHistorySaver(nil)
	var response *model.Response
	if resume {
		response, err = c.runtime.Resume(ctx)
	} else {
		response, err = c.runtime.Respond(ctx, input)
	}
	if response != nil {
		_ = c.saveHistoryUsage(ctx, sessionID)
	}
	status := TaskCompleted
	if err != nil {
		status = TaskFailed
		if ctx.Err() != nil {
			status = TaskPaused
		}
	}
	if pending, pendingErr := c.HasPending(ctx, sessionID); pendingErr == nil && pending && status != TaskCompleted {
		status = TaskPaused
	}
	c.finishTaskState(sessionID, status, err)
	if status != TaskPaused && c.teamRuns != nil {
		if cleanupErr := c.teamRuns.DeleteParent(c.runtime.runID()); cleanupErr != nil {
			slog.Warn("delete completed team runs", "error", cleanupErr)
		}
	}
	if response == nil {
		return nil, err
	}
	encoded, encodeErr := json.Marshal(response)
	if encodeErr != nil {
		return nil, errors.Join(err, fmt.Errorf("encode task response: %w", encodeErr))
	}
	return encoded, err
}

func (c *Core) beginTask(ctx context.Context, sessionID string, planMode bool, settings SessionSettings, profile model.ModelProfile, modelName string, contextWindow int64, expectedUpdatedAt time.Time) (string, error) {
	if ctx == nil {
		return "", errors.New("context is nil")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", errors.New("core is closed")
	}
	if c.active.Status == TaskRunning {
		c.mu.Unlock()
		return "", errors.New("a task is already running")
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
	c.active = TaskState{
		Status: TaskRunning, SessionID: sessionID, TaskID: id, ContextWindow: contextWindow, PlanMode: planMode,
		ModelProfile: settings.ModelProfile, Provider: profile.Provider, Model: modelName,
		ReasoningEffort: settings.ReasoningEffort,
	}
	c.mu.Unlock()
	c.emit(Event{Kind: EventTaskStarted, TaskID: id, SessionID: sessionID, Status: TaskRunning, ContextWindow: contextWindow,
		ModelProfile: settings.ModelProfile, Provider: profile.Provider, Model: modelName})
	return id, nil
}

func (c *Core) endTask() {
	c.mu.Lock()
	if c.active.Status == TaskRunning {
		c.active.Status = TaskIdle
	}
	c.mu.Unlock()
}

func (c *Core) setTaskStatus(status TaskStatus) {
	c.mu.Lock()
	c.active.Status = status
	c.mu.Unlock()
}

func (c *Core) finishTaskState(sessionID string, status TaskStatus, err error) {
	c.setTaskStatus(status)
	state := c.State()
	c.emit(Event{Kind: EventTaskFinished, TaskID: state.TaskID, SessionID: sessionID, Status: status, ContextWindow: state.ContextWindow, Err: errorString(err),
		ModelProfile: state.ModelProfile, Provider: state.Provider, Model: state.Model})
}

func (c *Core) emit(event Event) {
	c.mu.RLock()
	handler := c.handler
	c.mu.RUnlock()
	if handler == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Warn("core event handler panicked", "panic_type", fmt.Sprintf("%T", recovered))
		}
	}()
	handler(event)
}

func (c *Core) onToolEvent(event tool.ToolEvent) {
	kind := EventToolFinished
	if event.Started {
		kind = EventToolStarted
	}
	c.emit(Event{Kind: kind, SessionID: c.State().SessionID, TaskID: c.State().TaskID, Tool: ToolCall{
		ID: event.CallID, AgentID: event.AgentID, Name: event.Name, Arguments: event.Arguments, Started: event.Started,
		Duration: event.Duration, Failed: event.Failed, Canceled: event.Canceled,
	}})
}

func (c *Core) onModelStream(agentID, agentName, profile, provider, modelID string, event model.ModelStreamEvent) {
	stream := &ModelStreamEvent{
		Type: string(event.Type), ItemID: event.ItemID, OutputIndex: event.OutputIndex,
		Delta: event.Delta,
	}
	if event.ToolCall != nil {
		stream.ToolCallID, stream.ToolName, stream.ToolArguments = event.ToolCall.CallID, event.ToolCall.Name, event.ToolCall.Arguments
	}
	c.emit(Event{
		Kind: EventModelStream, SessionID: c.State().SessionID, TaskID: c.State().TaskID,
		AgentID: agentID, AgentName: agentName, ModelProfile: profile, Provider: provider, Model: modelID,
		Stream: stream,
	})
}

func (c *Core) onUsage(usage model.ResponseUsage) {
	c.onAgentUsage(usage, "", "", "", "", 0)
}

func (c *Core) onAgentUsage(usage model.ResponseUsage, agentID, profile, provider, modelID string, estimatedInputTokens int64) {
	value := Usage{InputTokens: usage.InputTokens, InputTokensDetails: TokenInputDetails{
		CachedTokens: usage.InputTokensDetails.CachedTokens, CacheCreationTokens: usage.InputTokensDetails.CacheCreationTokens,
	},
		OutputTokens: usage.OutputTokens, OutputTokensDetails: TokenOutputDetails{ReasoningTokens: usage.OutputTokensDetails.ReasoningTokens}, TotalTokens: usage.TotalTokens}
	state := c.State()
	contextWindow := state.ContextWindow
	if agentID != "" && agentID != "lead" {
		contextWindow = 0
	}
	if state.SessionID != "" {
		if err := c.recordUsageForAgent(context.Background(), state.SessionID, value, agentID, profile, provider, modelID, contextWindow, estimatedInputTokens); err != nil {
			slog.Warn("persist token usage", "error", err)
		}
	}
	c.emit(Event{Kind: EventUsage, SessionID: state.SessionID, TaskID: state.TaskID, Usage: value,
		AgentID: agentID, ModelProfile: profile, Provider: provider, Model: modelID, ContextWindow: contextWindow,
		EstimatedInputTokens: estimatedInputTokensIfMissing(value.InputTokens, estimatedInputTokens)})
}

func (c *Core) requestApproval(ctx context.Context, request tool.PermissionRequest) bool {
	c.approvalQueueMu.Lock()
	defer c.approvalQueueMu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	id, err := newID()
	if err != nil {
		return false
	}
	response := make(chan bool, 1)
	c.mu.Lock()
	if c.closed || c.handler == nil {
		c.mu.Unlock()
		return false
	}
	c.approvals[id] = response
	c.mu.Unlock()
	c.emit(Event{Kind: EventApprovalRequired, SessionID: c.State().SessionID, TaskID: c.State().TaskID,
		AgentID:  request.AgentID,
		Approval: &ApprovalRequest{ID: id, AgentID: request.AgentID, Tool: request.Tool, Arguments: append(json.RawMessage(nil), request.Arguments...)}})
	select {
	case allowed := <-response:
		return allowed
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.approvals, id)
		c.mu.Unlock()
		c.emit(Event{Kind: EventApprovalCancelled, SessionID: c.State().SessionID, TaskID: c.State().TaskID, ApprovalID: id})
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
	if c.active.Status == TaskRunning && c.active.SessionID == sessionID {
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
	if c.active.Status == TaskRunning && c.active.SessionID == sessionID {
		return errors.New("cannot delete a session while its task is running")
	}
	// Save-based usage/settings updates must not recreate a session after its
	// row has been deleted. The session row is removed last for retry safety.
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return err
	}
	parentID := c.runtime.checkpointID(sessionID)
	childCheckpoints, err := c.teamRuns.CheckpointIDs(parentID)
	if err != nil {
		return fmt.Errorf("list team checkpoints for session %q: %w", sessionID, err)
	}
	if err := c.checkpoints.Delete(ctx, parentID); err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
		return fmt.Errorf("delete task checkpoint for session %q: %w", sessionID, err)
	}
	for _, childID := range childCheckpoints {
		if err := c.checkpoints.Delete(ctx, childID); err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
			return fmt.Errorf("delete team member checkpoint for session %q: %w", sessionID, err)
		}
	}
	if err := c.teamRuns.DeleteParent(parentID); err != nil {
		return err
	}
	deleted, err := c.sessions.Delete(ctx, sessionID, c.workspace)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("session %q does not exist in workspace", sessionID)
	}
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
	if c.active.Status == TaskRunning && c.active.SessionID == sessionID {
		return 0, errors.New("cannot rewind a session while its task is running")
	}
	pending, err := c.HasPending(ctx, sessionID)
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

func (c *Core) HasPending(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, errors.New("session id is required")
	}
	if c == nil || ctx == nil {
		return false, errors.New("core and context are required")
	}
	return c.runtime.HasPendingFor(sessionID, ctx)
}

func (c *Core) PendingHistory(ctx context.Context, sessionID string) (json.RawMessage, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return nil, errors.New("core, context, and session id are required")
	}
	history, err := c.runtime.PendingHistoryFor(ctx, sessionID)
	if err != nil || history == nil {
		return nil, err
	}
	return json.Marshal(history)
}

func (c *Core) PendingPlanMode(ctx context.Context, sessionID string) (bool, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return false, errors.New("core, context, and session id are required")
	}
	return c.runtime.PendingPlanModeFor(ctx, sessionID)
}

func (c *Core) DiscardTask(ctx context.Context, sessionID string) (bool, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return false, errors.New("core, context, and session id are required")
	}
	if c.State().Status == TaskRunning {
		return false, errors.New("cannot discard a task while another task is running")
	}
	err := c.checkpoints.Delete(ctx, c.runtime.checkpointID(sessionID))
	if errors.Is(err, checkpoint.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Core) Compact(ctx context.Context, sessionID string) (int, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return 0, errors.New("core, context, and session id are required")
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
	if _, err := c.beginTask(ctx, sessionID, false, settings, profile, profile.Model, effective.ContextWindow, saved.UpdatedAt); err != nil {
		return 0, err
	}
	defer c.endTask()
	if _, err := c.configureRuntime(settings.ModelProfile, settings.ReasoningEffort); err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return 0, err
	}
	if err := c.resetTaskUsage(ctx, sessionID); err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return 0, err
	}
	c.runtime.SetSessionState(sessionID, transcript, modelContext, contextKey)
	compacted, err := c.runtime.Compact(ctx)
	if err == nil {
		err = c.saveHistoryUsage(ctx, sessionID)
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

func (c *Core) saveHistoryUsage(ctx context.Context, sessionID string) error {
	return c.setSessionConversation(ctx, sessionID, c.runtime.SessionHistory(), c.runtime.ModelContextHistory(), c.runtime.ContextKey())
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
