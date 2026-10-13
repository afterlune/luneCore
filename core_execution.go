package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	runtimepermission "github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
)

func (c *Core) RunTask(ctx context.Context, request TaskRequest) (TaskResult, error) {
	if c == nil || ctx == nil {
		return TaskResult{}, errors.New("core and context are required")
	}
	return c.runSynchronous(ctx, request)
}

func (c *Core) ResumeTask(ctx context.Context, sessionID string, options ...ExecutionOptions) (TaskResult, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return TaskResult{}, errors.New("core, context, and session id are required")
	}
	runtime, err := c.OpenSessionRuntime(ctx, sessionID)
	if err != nil {
		return TaskResult{}, err
	}
	cursor, err := c.sessions.LatestRuntimeEventCursor(ctx, sessionID)
	if err != nil {
		return TaskResult{}, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	events, err := runtime.subscribeEvents(streamCtx, cursor, false)
	if err != nil {
		return TaskResult{}, err
	}
	go func() {
		for range events {
		}
	}()
	submission, waiter, err := c.recoverSession(ctx, sessionID, true, options...)
	if err != nil {
		return TaskResult{}, err
	}
	c.scheduleQueueDrain(sessionID)
	return c.waitForTurn(ctx, runtime, submission.ExecutionID, waiter)
}

func (c *Core) run(ctx context.Context, sessionID string, maxRounds int, modelOverride string, executionOptions ExecutionOptions, executionOverride bool, permissionOverride string, authorization publictool.PlanAuthorization, noInteractiveDecisions bool, resume bool, input *model.ResponseInput, queueID, executionID string) (json.RawMessage, error) {
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
	if err := validatePermissionOverride(permissionOverride); err != nil {
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
		maxRounds = taskOptions.MaxToolRounds
	} else {
		effective, resolveErr := c.resolveTaskSettings(settings)
		if resolveErr != nil {
			return nil, resolveErr
		}
		taskOptions = agentgraph.TaskOptions{
			MaxToolRounds: maxRounds, ReasoningEffort: settings.ReasoningEffort,
			Model: modelOverride,
			Goal:  effective.Goal, ContextWindow: effective.ContextWindow,
			ReserveOutputTokens: effective.ReserveOutputTokens, AutoCompact: effective.AutoCompact,
			AutoCompactThreshold:   effective.AutoCompactThreshold,
			Execution:              agentExecutionOptions(mergeExecutionOptions(c.executionDefaults, executionOptions)),
			PermissionOverride:     permissionOverride,
			PlanAuthorization:      authorization,
			NoInteractiveDecisions: noInteractiveDecisions,
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
	if !resume {
		instructions, instructionErr := c.initialProjectInstructions(ctx)
		if instructionErr != nil {
			return nil, fmt.Errorf("load workspace AGENTS.md: %w", instructionErr)
		}
		taskOptions.ProjectInstructionsReady = true
		taskOptions.ProjectInstructions = instructions
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
	taskID, err := c.beginTask(taskCtx, sessionID, queueID, settings, profile, effectiveModel, taskOptions, saved.UpdatedAt, cancelTask)
	if err != nil {
		cancelTask()
		return nil, err
	}
	defer c.endTask(sessionID, taskID, cancelTask)
	ctx = taskCtx
	taskState, _ := c.StateForSession(sessionID)
	ctx = publictool.WithCallIdentity(ctx, publictool.CallIdentity{
		SessionID: sessionID, TaskID: taskState.TaskID, RunID: c.runtime.CheckpointID(sessionID),
		ExecutionID: taskOptions.ExecutionID, AgentID: "lead",
	})
	ctx = runtimepermission.WithPermissionMode(ctx, taskOptions.PermissionOverride)
	ctx = publictool.WithPlanAuthorization(ctx, taskOptions.PlanAuthorization)
	ctx = publictool.WithNoInteractiveDecisions(ctx, taskOptions.NoInteractiveDecisions)
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
		taskRuntime.SetExecution(c.liveExecutions[taskRuntime.RunID()])
		c.mu.RUnlock()
	}
	var response *model.Response
	if resume {
		var inputs []graph.ResumeInput
		inputs, err = c.graphResumeInputs(ctx, sessionID, taskRuntime.RunID())
		if err == nil {
			response, err = taskRuntime.Resume(ctx, inputs)
		}
		if reconcileErr := c.reconcileGraphResumeInputs(ctx, sessionID, taskRuntime.RunID()); reconcileErr != nil {
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
			publicQuestion := publicUserQuestionRequest(question)
			c.emit(Event{Kind: EventUserQuestionRequired, SessionID: sessionID, TaskID: task.TaskID, ExecutionID: question.ExecutionID,
				UserQuestion: &publicQuestion})
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
	if execution := taskRuntime.Execution(); execution != nil && queueID != "" {
		if c.liveExecutions == nil {
			c.liveExecutions = make(map[string]*graph.Execution[agentgraph.State])
		}
		if status == TaskPaused {
			c.liveExecutions[taskRuntime.RunID()] = execution
		} else {
			delete(c.liveExecutions, taskRuntime.RunID())
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
			if invocation.Status == graph.InvocationWaiting && (invocation.Continuation == agentgraph.UserQuestionContinuation || invocation.Continuation == agentgraph.AcceptanceContinuation) && invocation.ID == input.InvocationID && invocation.CallID == input.CallID {
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
			if invocation.Status == graph.InvocationWaiting && (invocation.Continuation == agentgraph.UserQuestionContinuation || invocation.Continuation == agentgraph.AcceptanceContinuation) && invocation.ID == input.InvocationID && invocation.CallID == input.CallID {
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
