package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	graph "github.com/afterlune/luneGraph"
)

func (c *Core) finishTaskRun(ctx context.Context, sessionID, queueID, executionID, executionScopeID string, response *model.Response, runErr error, taskRuntime *agentruntime.Responder) (json.RawMessage, error) {
	err := runErr
	cleanupErr := c.stopCommands(executionScopeID, "")
	if cleanupErr != nil {
		err = errors.Join(err, ErrTaskPaused, cleanupErr)
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
			c.logger.Warn("delete completed tool invocation records", "error", cleanupErr)
		}
	}
	if response == nil {
		return nil, err
	}
	return encoded, err
}
