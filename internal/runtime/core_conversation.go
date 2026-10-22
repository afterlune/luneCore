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

// CompactionResult describes one manual compaction pass for hosts.
type CompactionResult = agentgraph.CompactionResult

func (c *Core) Compact(ctx context.Context, sessionID string) (agentgraph.CompactionResult, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return agentgraph.CompactionResult{}, errors.New("core, context, and session id are required")
	}
	var compacted agentgraph.CompactionResult
	err := c.withSessionLock(sessionID, func() error {
		var err error
		compacted, err = c.compactOwned(ctx, sessionID)
		return err
	})
	return compacted, err
}

func (c *Core) compactOwned(ctx context.Context, sessionID string) (agentgraph.CompactionResult, error) {
	c.runtimeConfigMu.RLock()
	defer c.runtimeConfigMu.RUnlock()
	c.mu.RLock()
	closed := c.closed || c.closing
	c.mu.RUnlock()
	if closed {
		return agentgraph.CompactionResult{}, errors.New("core is closed")
	}
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return agentgraph.CompactionResult{}, err
	}
	var transcript *model.ResponseInput
	if len(saved.History) > 0 {
		var decoded model.ResponseInput
		if err := json.Unmarshal(saved.History, &decoded); err != nil {
			return agentgraph.CompactionResult{}, err
		}
		transcript = &decoded
	}
	settings, err := c.normalizeSessionSettings(saved.Settings)
	if err != nil {
		return agentgraph.CompactionResult{}, err
	}
	effective, err := c.resolveTaskSettings(settings)
	if err != nil {
		return agentgraph.CompactionResult{}, err
	}
	profile, _ := c.models.Profile(settings.ModelProfile)
	providerConfig, ok := c.models.ProviderConfig(profile.Provider)
	if !ok {
		return agentgraph.CompactionResult{}, fmt.Errorf("provider %q is not configured", profile.Provider)
	}
	contextKey := modelContextKey(profile.Provider, profile.Model, providerConfig)
	modelContext := transcript
	if saved.ModelContextKey == contextKey && hasSessionHistory(saved.ModelContext) {
		var decoded model.ResponseInput
		if err := json.Unmarshal(saved.ModelContext, &decoded); err != nil {
			return agentgraph.CompactionResult{}, fmt.Errorf("decode provider model context: %w", err)
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
		return agentgraph.CompactionResult{}, err
	}
	defer c.endTask(sessionID, taskID, nil)
	taskState, _ := c.StateForSession(sessionID)
	ctx = publictool.WithCallIdentity(ctx, publictool.CallIdentity{
		SessionID: sessionID, TaskID: taskState.TaskID, RunID: c.runtime.CheckpointID(sessionID), AgentID: "lead",
	})
	taskRuntime, _, err := c.configureRuntime(settings.ModelProfile, settings.ReasoningEffort, taskState)
	if err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return agentgraph.CompactionResult{}, err
	}
	if err := c.resetTaskUsage(ctx, sessionID); err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return agentgraph.CompactionResult{}, err
	}
	if len(saved.AgentContext) > 0 {
		if err := json.Unmarshal(saved.AgentContext, &compactOptions.Conversation); err != nil {
			return agentgraph.CompactionResult{}, err
		}
	}
	taskRuntime.SetTaskOptions(compactOptions)
	taskRuntime.SetSessionState(sessionID, transcript, modelContext, contextKey)
	c.emit(Event{Kind: EventContextCompaction, TaskID: taskID, SessionID: sessionID, Compaction: &ContextCompactionDetails{Stage: "started", Mode: "local", Reason: "manual"}})
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
		c.emit(Event{Kind: EventContextCompaction, TaskID: taskID, SessionID: sessionID, Err: err.Error(), Compaction: &ContextCompactionDetails{Stage: "failed", Mode: "local", Reason: "manual", Err: err.Error()}})
		return compacted, err
	}
	if compacted.Compacted {
		c.emit(Event{Kind: EventContextCompaction, TaskID: taskID, SessionID: sessionID, Compaction: &ContextCompactionDetails{
			Stage: "completed", Mode: "local", Reason: "manual", Strategy: compacted.Mode,
			BeforeTokens: compacted.Before, AfterTokens: compacted.After, Threshold: compacted.Threshold, Turns: compacted.Turns,
		}})
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
	return c.setSessionConversation(ctx, sessionID, runtime.SessionHistory(), runtime.ModelContextHistory(), runtime.ContextKey(), runtime.ConversationState())
}
