package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
)

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

func (c *Core) beginTask(ctx context.Context, sessionID, queueID string, settings SessionSettings, profile model.ModelProfile, modelName string, taskOptions agentgraph.TaskOptions, expectedUpdatedAt time.Time, cancel context.CancelFunc) (string, error) {
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
		ModelOverride: taskOptions.Model,
		ModelProfile:  settings.ModelProfile, Provider: profile.Provider, Model: modelName,
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
		// A repeated interrupt can race the paused queue commit. It belongs to
		// this attempt, not a future recovery under the same ExecutionID.
		delete(c.interruptRequests, state.ExecutionID)
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
			artifacts, artifactErr := c.acceptanceArtifacts(context.Background(), sessionID)
			if artifactErr != nil {
				return 0, artifactErr
			}
			return c.sessions.PauseQueuedTurnWithReason(context.Background(), sessionID, state.QueueID, state.ExecutionID, string(TaskPaused), string(event.BudgetReason), payload, artifacts...)
		})
		if persistErr != nil {
			c.logger.Warn("persist paused queued task", "error", persistErr)
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
			c.logger.Warn("persist token usage", "error", err)
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
