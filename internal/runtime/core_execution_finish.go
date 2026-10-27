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
	"github.com/afterlune/luneCore/internal/session"
	graph "github.com/afterlune/luneGraph"
)

func (c *Core) finishTaskRun(ctx context.Context, sessionID, queueID, executionID, executionScopeID string, response *model.Response, runErr error, taskRuntime *agentruntime.Responder) (json.RawMessage, error) {
	explicitlyInterrupted := c.beginTaskFinish(executionID)
	defer c.endTaskFinish(executionID)

	err := runErr
	cleanupErr := c.stopCommands(executionScopeID, "")
	if cleanupErr != nil {
		err = errors.Join(err, ErrTaskPaused, cleanupErr)
	}
	pausedByGraph := errors.Is(err, ErrTaskPaused)
	graphCancelled := errors.Is(err, agentruntime.ErrExecutionCancelled)
	if pausedByGraph && !explicitlyInterrupted {
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
	if explicitlyInterrupted {
		status = TaskCancelled
	} else if pausedByGraph || graphCancelled {
		status, taskErr = TaskPaused, err
	} else if err != nil {
		status = TaskFailed
		if ctx.Err() != nil {
			status = TaskPaused
		}
	} else if response == nil && ctx.Err() != nil {
		// A cancelled graph can return no response and no error. Preserve its
		// checkpoint instead of reporting a completed turn with no response.
		status = TaskPaused
		taskErr = ctx.Err()
		err = ctx.Err()
	}
	if !explicitlyInterrupted {
		if pending, pendingErr := c.hasPending(ctx, sessionID); pendingErr == nil && pending && status != TaskCompleted {
			status = TaskPaused
		}
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
	persistCtx := ctx
	var cancelPersist context.CancelFunc
	if explicitlyInterrupted {
		persistCtx, cancelPersist = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancelPersist()
	}
	if queueID != "" && status != TaskPaused {
		queued := session.QueuedTurn{SessionID: sessionID, QueueID: queueID, ExecutionID: executionID}
		if persistErr := c.completeQueuedTurnFromRuntime(persistCtx, queued, status, encoded, taskRuntime, taskErr); persistErr != nil {
			err = errors.Join(err, fmt.Errorf("persist completed turn: %w", persistErr))
		}
	}
	if explicitlyInterrupted {
		checkpointID := c.runtime.CheckpointID(sessionID)
		if queueID == "" {
			c.mu.Lock()
			delete(c.liveExecutions, checkpointID)
			c.mu.Unlock()
			if cleanupErr := c.runtime.DeleteCompletedRunForSession(sessionID); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("delete interrupted task checkpoint: %w", cleanupErr))
				taskErr = err
			}
		}
		if cleanupErr := c.sessions.DeleteGraphResumeInputs(persistCtx, sessionID, checkpointID); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("delete interrupted task resume inputs: %w", cleanupErr))
			taskErr = err
		}
	}
	c.finishTaskState(sessionID, status, taskErr)
	if queueID == "" && status != TaskPaused {
		if cleanupErr := c.sessions.DeleteToolInvocationsForSession(context.Background(), sessionID); cleanupErr != nil {
			c.logger.Warn("delete completed tool invocation records", "error", cleanupErr)
		}
		if cleanupErr := c.sessions.DeleteExecutionTokenBudget(context.Background(), executionScopeID); cleanupErr != nil {
			c.logger.Warn("delete completed execution token budget", "error", cleanupErr)
		}
	}
	if response == nil {
		return nil, err
	}
	return encoded, err
}
