package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
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
	defer runtime.Close()
	cursor, err := c.sessions.LatestRuntimeEventCursor(ctx, sessionID)
	if err != nil {
		return TaskResult{}, err
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	events, err := runtime.subscribeEvents(streamCtx, cursor, false, false)
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

func (c *Core) run(ctx context.Context, sessionID string, maxRounds int, modelOverride string, executionOptions ExecutionOptions, executionOverride bool, permissionOverride string, planMode bool, authorization publictool.PlanAuthorization, noInteractiveDecisions bool, resume bool, input *model.ResponseInput, queueID, executionID string) (json.RawMessage, error) {
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
	closed := c.closed || c.closing
	c.mu.RUnlock()
	if closed {
		return nil, errors.New("core is closed")
	}
	setup, err := c.prepareTaskRun(ctx, taskRunConfig{
		sessionID: sessionID, maxRounds: maxRounds, modelOverride: modelOverride,
		executionOptions: executionOptions, executionOverride: executionOverride,
		permissionOverride: permissionOverride, planMode: planMode, authorization: authorization,
		noInteractiveDecisions: noInteractiveDecisions, resume: resume, executionID: executionID,
	})
	if err != nil {
		return nil, err
	}
	saved, settings, profile := setup.saved, setup.settings, setup.profile
	effectiveModel, taskOptions := setup.effectiveModel, setup.taskOptions
	transcript, modelContext, contextKey := setup.transcript, setup.modelContext, setup.contextKey
	taskCtx, cancelTask := context.WithCancel(ctx)
	taskID, err := c.beginTask(taskCtx, sessionID, queueID, settings, profile, effectiveModel, taskOptions, saved.UpdatedAt, cancelTask)
	if err != nil {
		cancelTask()
		return nil, err
	}
	defer c.endTask(sessionID, taskID, cancelTask)
	if err := c.commands.OpenScope(taskOptions.ExecutionID, taskCtx); err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return nil, err
	}
	defer c.stopCommands(taskOptions.ExecutionID, "")
	ctx = taskCtx
	taskState, _ := c.StateForSession(sessionID)
	ctx = publictool.WithCallIdentity(ctx, publictool.CallIdentity{
		SessionID: sessionID, TaskID: taskState.TaskID, RunID: c.runtime.CheckpointID(sessionID),
		ExecutionID: taskOptions.ExecutionID, AgentID: "lead",
	})
	ctx = runtimepermission.WithSessionPathGrantStore(ctx, runtimepermission.NewSessionPathGrantStore(saved.PathGrants))
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
	if err := c.sessions.SetExecutionTokenBudget(ctx, sessionID, taskOptions.ExecutionID, taskOptions.Execution.TokenBudget); err != nil {
		c.finishTaskState(sessionID, TaskFailed, err)
		return nil, err
	}
	ctx = agentruntime.WithTokenBudget(ctx, &executionTokenBudget{store: c.sessions, executionID: taskOptions.ExecutionID})
	if !resume {
		if err := c.resetTaskUsage(ctx, sessionID); err != nil {
			c.finishTaskState(sessionID, TaskFailed, err)
			return nil, err
		}
	}
	taskRuntime.SetSessionState(sessionID, transcript, modelContext, contextKey)
	taskRuntime.SetTaskOptions(taskOptions)
	taskRuntime.SetHistorySaver(func(id string, transcript, modelContext *model.ResponseInput, key string, agentContext json.RawMessage) error {
		return c.setSessionConversation(context.Background(), id, transcript, modelContext, key, agentContext)
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
		inputs, err = c.graphResumeInputs(ctx, sessionID, taskRuntime.RunID(), true)
		if err == nil {
			response, err = taskRuntime.Resume(ctx, inputs)
		}
		if reconcileErr := c.reconcileGraphResumeInputs(ctx, sessionID, taskRuntime.RunID()); reconcileErr != nil {
			err = errors.Join(err, reconcileErr)
		}
	} else {
		// A fresh graph starts its wait counter again. Receipts belong to the
		// previous execution and must not collide with this turn.
		err = c.sessions.DeleteGraphResumeInputs(ctx, sessionID, taskRuntime.RunID())
		if err == nil {
			response, err = taskRuntime.Respond(ctx, input)
		}
	}
	return c.finishTaskRun(ctx, sessionID, queueID, executionID, taskOptions.ExecutionID, response, err, taskRuntime)

}
