package runtime

import (
	"context"
	"errors"
	"reflect"

	"github.com/afterlune/luneCore/internal/agentgraph"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/mcp"
	"github.com/afterlune/luneCore/internal/model"
)

// ConfigReloadResult reports what ReloadProjectConfig applied immediately and
// whether MCP server updates must wait for active tasks to finish.
type ConfigReloadResult struct {
	Reloaded    bool     `json:"reloaded"`
	MCPPending  bool     `json:"mcpPending,omitempty"`
	MCPWarnings []string `json:"mcpWarnings,omitempty"`
}

// ReloadProjectConfig validates and atomically swaps model, runtime,
// attachment, and instruction settings without closing sessions or RPC
// connections. Running and paused turns retain their checkpointed snapshot.
func (c *Core) ReloadProjectConfig(ctx context.Context, projectConfig projectconfig.ProjectConfig, personalInstructions string) (ConfigReloadResult, error) {
	if c == nil || ctx == nil {
		return ConfigReloadResult{}, errors.New("core and context are required")
	}
	if err := ctx.Err(); err != nil {
		return ConfigReloadResult{}, err
	}
	c.mu.RLock()
	closed := c.closed || c.closing
	c.mu.RUnlock()
	if closed {
		return ConfigReloadResult{}, errors.New("core is closed")
	}

	c.configMu.Lock()
	defer c.configMu.Unlock()

	c.mu.RLock()
	closed = c.closed || c.closing
	c.mu.RUnlock()
	if closed {
		return ConfigReloadResult{}, errors.New("core is closed")
	}

	prev := c.currentConfig()
	if prev == nil {
		return ConfigReloadResult{}, errors.New("core configuration snapshot is unavailable")
	}

	resolvedProject, err := projectconfig.LoadForRuntime(c.workspace, prev.modelConfig, prev.modelProviders, &projectConfig, prev.maxParallelAgents)
	if err != nil {
		return ConfigReloadResult{}, err
	}
	nextModelConfig := resolvedProject.ModelConfig()
	models, err := model.NewProviderCatalogWithHostProviders(nextModelConfig, prev.modelProviders)
	if err != nil {
		return ConfigReloadResult{}, err
	}

	executionDefaults := prev.hostExecution
	if executionDefaults.MaxTeamChildrenPerAgent == 0 {
		executionDefaults.MaxTeamChildrenPerAgent = resolvedProject.Runtime.MaxTeamChildrenPerAgent
	}
	if executionDefaults.MaxTeamChildrenPerAgent == 0 {
		executionDefaults.MaxTeamChildrenPerAgent = agentgraph.DefaultMaxTeamChildrenPerAgent
	}
	if executionDefaults.MaxTeamDepth == 0 {
		executionDefaults.MaxTeamDepth = resolvedProject.Runtime.MaxTeamDepth
	}
	if executionDefaults.MaxTeamDepth == 0 {
		executionDefaults.MaxTeamDepth = agentgraph.DefaultMaxTeamDepth
	}

	nextMCP := resolveMCPServers(resolvedProject.MCPServers, prev.memoryDisabled)
	nextTaskDefaults := taskSettings{
		ContextWindow:        resolvedProject.Runtime.ContextWindow,
		ReserveOutputTokens:  resolvedProject.Runtime.ReserveOutputTokens,
		AutoCompact:          resolvedProject.Runtime.AutoCompact,
		AutoCompactThreshold: resolvedProject.Runtime.AutoCompactThreshold,
	}
	attachmentsChanged := prev.attachmentLimits != resolvedProject.Attachments
	mcpChanged := !mcpServerMapsEqual(prev.mcpServers, nextMCP)
	configChanged := !reflect.DeepEqual(prev.resolvedModelConfig, nextModelConfig) ||
		prev.taskDefaults != nextTaskDefaults ||
		prev.executionDefaults != executionDefaults ||
		prev.personalInstructions != personalInstructions ||
		attachmentsChanged

	next := &runtimeSnapshot{
		models:               models,
		team:                 &teamExecutor{core: c, models: models, tools: c.registry},
		attachmentLimits:     resolvedProject.Attachments,
		taskDefaults:         nextTaskDefaults,
		executionDefaults:    executionDefaults,
		personalInstructions: personalInstructions,
		mcpServers:           cloneMCPServerMap(prev.mcpServers),
		memoryDisabled:       prev.memoryDisabled,
		modelConfig:          prev.modelConfig,
		resolvedModelConfig:  nextModelConfig,
		modelProviders:       prev.modelProviders,
		maxParallelAgents:    prev.maxParallelAgents,
		hostExecution:        prev.hostExecution,
	}

	var result ConfigReloadResult
	appliedMCP := false
	mcpLimits := mcpLimitsFromAttachments(resolvedProject.Attachments)
	if mcpChanged && c.mcpManager != nil {
		if c.runtimeConfigMu.TryLock() {
			c.mu.RLock()
			idle := !c.closed && !c.closing && !c.hasRunningTasksLocked()
			c.mu.RUnlock()
			if idle {
				for _, mcpErr := range c.mcpManager.UpdateServers(nextMCP, mcpLimits) {
					c.logger.Warn("update MCP server after config reload", "error_type", errStringType(mcpErr), "error", mcpErr)
					if mcpErr != nil {
						result.MCPWarnings = append(result.MCPWarnings, mcpErr.Error())
					}
				}
				next.mcpServers = cloneMCPServerMap(nextMCP)
				appliedMCP = true
			}
			c.runtimeConfigMu.Unlock()
		}
		result.MCPPending = !appliedMCP
	} else {
		next.mcpServers = cloneMCPServerMap(nextMCP)
	}

	if attachmentsChanged {
		if c.registry != nil {
			c.registry.SetImageMaxBytes(resolvedProject.Attachments.ImageMaxBytes())
		}
		// Update MCP attachment/binary limits without tearing down connected servers.
		if c.mcpManager != nil && !appliedMCP {
			c.mcpManager.SetLimits(mcpLimits)
		}
	}

	if configChanged || appliedMCP {
		c.configState.Store(next)
		c.publishLiveAll(Event{Kind: EventConfigReloaded})
	}
	result.Reloaded = true
	return result, nil
}

func mcpLimitsFromAttachments(attachments projectconfig.AttachmentLimits) mcp.Limits {
	resolved := attachments.Resolved()
	return mcp.Limits{
		MaxBinaryBlocks: resolved.MaxFiles,
		MaxFileBytes:    resolved.MaxFileBytes,
		MaxTotalBytes:   resolved.MaxTotalBytes,
	}
}

func mcpServerMapsEqual(a, b map[string]projectconfig.MCPServerConfig) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func errStringType(err error) string {
	if err == nil {
		return ""
	}
	return reflect.TypeOf(err).String()
}

func (c *Core) setExecutionDefaultsForTest(options ExecutionOptions) {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	prev := c.currentConfig()
	if prev == nil {
		return
	}
	cloned := *prev
	cloned.executionDefaults = options
	c.configState.Store(&cloned)
}

func (c *Core) setAttachmentLimitsForTest(limits projectconfig.AttachmentLimits) {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	prev := c.currentConfig()
	if prev == nil {
		c.configState.Store(&runtimeSnapshot{attachmentLimits: limits})
		return
	}
	cloned := *prev
	cloned.attachmentLimits = limits
	c.configState.Store(&cloned)
}
