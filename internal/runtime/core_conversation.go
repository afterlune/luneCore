package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

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
		Execution:       agentExecutionOptions(c.executionDefaults),
		ReasoningEffort: settings.ReasoningEffort, Goal: effective.Goal,
		ContextWindow: effective.ContextWindow, ReserveOutputTokens: effective.ReserveOutputTokens,
		AutoCompact: effective.AutoCompact, AutoCompactThreshold: effective.AutoCompactThreshold,
	}
	taskID, err := c.beginTask(ctx, sessionID, "", settings, profile, profile.Model, compactOptions, saved.UpdatedAt, nil)
	if err != nil {
		return 0, err
	}
	defer c.endTask(sessionID, taskID, nil)
	taskState, _ := c.StateForSession(sessionID)
	ctx = publictool.WithCallIdentity(ctx, publictool.CallIdentity{
		SessionID: sessionID, TaskID: taskState.TaskID, RunID: c.runtime.CheckpointID(sessionID), AgentID: "lead",
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

func (c *Core) saveHistoryUsage(ctx context.Context, sessionID string, runtime *agentruntime.Responder) error {
	return c.setSessionConversation(ctx, sessionID, runtime.SessionHistory(), runtime.ModelContextHistory(), runtime.ContextKey())
}
