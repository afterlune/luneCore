package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) loadSteeringInputs(ctx context.Context, sessionID, parentExecutionID string, finalizing bool) ([]agentgraph.SteeringInput, error) {
	if c == nil || ctx == nil || sessionID == "" || parentExecutionID == "" {
		return nil, errors.New("core, context, session id, and parent execution id are required")
	}
	c.queueMutationMu.Lock()
	defer c.queueMutationMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.steeringExecutions[sessionID] != parentExecutionID {
		return nil, nil
	}
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list steering inputs: %w", err)
	}
	inputs := make([]agentgraph.SteeringInput, 0)
	for _, item := range queued {
		if item.Status != session.QueuedTurnQueued {
			continue
		}
		var request storedTurnRequest
		if err := json.Unmarshal(item.Request, &request); err != nil {
			return nil, fmt.Errorf("decode steering input %s: %w", item.ExecutionID, err)
		}
		if !request.SteerInput || request.SteerParentExecutionID != parentExecutionID {
			continue
		}
		input, err := model.NewResponseInput(request.Input)
		if err != nil {
			return nil, fmt.Errorf("decode steering input %s payload: %w", item.ExecutionID, err)
		}
		inputs = append(inputs, agentgraph.SteeringInput{QueueID: item.QueueID, ExecutionID: item.ExecutionID, Input: *input})
	}
	if finalizing && len(inputs) == 0 {
		delete(c.steeringExecutions, sessionID)
	}
	return inputs, nil
}

func (c *Core) acknowledgeSteeringInputs(ctx context.Context, sessionID, parentExecutionID string, inputs []agentgraph.SteeringInput) ([]agentgraph.SteeringInput, error) {
	if c == nil || ctx == nil || sessionID == "" || parentExecutionID == "" {
		return nil, errors.New("core, context, session id, and parent execution id are required")
	}
	c.queueMutationMu.Lock()
	defer c.queueMutationMu.Unlock()
	accepted := make([]agentgraph.SteeringInput, 0, len(inputs))
	for _, input := range inputs {
		queued, found, err := c.sessions.QueuedTurnByExecutionID(ctx, sessionID, input.ExecutionID)
		if err != nil {
			return accepted, err
		}
		if !found {
			result, completed, resultErr := c.sessions.TurnResult(ctx, sessionID, input.ExecutionID)
			if resultErr != nil {
				return accepted, resultErr
			}
			if completed && result.Status == string(TaskCompleted) {
				accepted = append(accepted, input)
			}
			continue
		}
		if queued.Status != session.QueuedTurnQueued && queued.Status != session.QueuedTurnActive {
			continue
		}
		var request storedTurnRequest
		if err := json.Unmarshal(queued.Request, &request); err != nil {
			return accepted, fmt.Errorf("decode claimed steering input: %w", err)
		}
		if !request.SteerInput || request.SteerParentExecutionID != parentExecutionID {
			continue
		}
		event := Event{
			Kind: EventTaskInputInjected, SessionID: sessionID, QueueID: input.QueueID,
			ExecutionID: parentExecutionID, InputExecutionID: input.ExecutionID, Status: TaskCompleted,
		}
		if _, err := c.sessions.CompleteQueuedTurn(ctx, sessionID, input.QueueID, input.ExecutionID, string(TaskCompleted), nil, nil, nil, "", mustMarshalEvent(event), nil); err != nil {
			result, completed, resultErr := c.sessions.TurnResult(ctx, sessionID, input.ExecutionID)
			if resultErr == nil && completed && result.Status == string(TaskCompleted) {
				accepted = append(accepted, input)
				continue
			}
			return accepted, fmt.Errorf("acknowledge steering input %s: %w", input.ExecutionID, err)
		}
		c.resolveTurnWaiter(input.ExecutionID, turnResult{})
		accepted = append(accepted, input)
		c.eventHub(sessionID).notifyDurable()
	}
	return accepted, nil
}
