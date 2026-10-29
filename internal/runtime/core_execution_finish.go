package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

// runOutcome is produced by execution and committed exclusively by its queue worker.
type runOutcome struct {
	Status       TaskStatus
	Response     json.RawMessage
	History      json.RawMessage
	ModelContext json.RawMessage
	ContextKey   string
	AgentContext json.RawMessage
	BudgetReason BudgetReason
	Question     *UserQuestionRequest
	Err          error
}

func failedRun(err error) runOutcome {
	status := TaskFailed
	if errors.Is(err, errExecutionInterrupted) {
		status = TaskCancelled
	}
	return runOutcome{Status: status, Err: err}
}

func (c *Core) finishTaskRun(ctx context.Context, sessionID, executionID, executionScopeID string, response *model.Response, runErr error, taskRuntime *agentruntime.Responder) runOutcome {
	explicitlyInterrupted := c.beginTaskFinish(executionID)

	err := runErr
	cleanupErr := c.stopCommands(executionScopeID, "")
	if cleanupErr != nil {
		err = errors.Join(err, ErrTaskPaused, cleanupErr)
	}
	pausedByGraph := errors.Is(err, ErrTaskPaused)
	graphCancelled := errors.Is(err, agentruntime.ErrExecutionCancelled)
	var question *UserQuestionRequest
	if pausedByGraph && !explicitlyInterrupted {
		pending, found, questionErr := taskRuntime.PendingUserQuestion(ctx, sessionID)
		if questionErr != nil {
			err = errors.Join(err, questionErr)
		} else if found {
			q := publicUserQuestionRequest(pending)
			question = &q
		}
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
	if explicitlyInterrupted {
		status = TaskCancelled
	} else if pausedByGraph || graphCancelled {
		status = TaskPaused
	} else if err != nil {
		status = TaskFailed
		if ctx.Err() != nil {
			status = TaskPaused
		}
	} else if response == nil && ctx.Err() != nil {
		// A cancelled graph can return no response and no error. Preserve its
		// checkpoint instead of reporting a completed turn with no response.
		status = TaskPaused
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
	}
	c.mu.Lock()
	if execution := taskRuntime.Execution(); execution != nil {
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
	outcome := runOutcome{Status: status, Response: encoded, Err: err, Question: question, BudgetReason: budgetReasonFromError(err)}
	if status != TaskCompleted {
		outcome.Response = nil
	}
	transcript := taskRuntime.SessionHistory()
	if transcript == nil {
		transcript = model.NewResponseInputItems()
	}
	var snapshotErr error
	outcome.History, snapshotErr = json.Marshal(transcript)
	if snapshotErr == nil {
		outcome.ModelContext, snapshotErr = json.Marshal(taskRuntime.ModelContextHistory())
	}
	outcome.ContextKey = taskRuntime.ContextKey()
	outcome.AgentContext = taskRuntime.ConversationState()
	if snapshotErr != nil {
		outcome.Status = TaskFailed
		outcome.Response = nil
		outcome.Err = errors.Join(outcome.Err, fmt.Errorf("snapshot task conversation: %w", snapshotErr))
	}
	return outcome
}
