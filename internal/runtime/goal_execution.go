package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/afterlune/luneCore/internal/model"
)

func (c *Core) goalRun(executionID string) goalExecution {
	c.goalMu.Lock()
	defer c.goalMu.Unlock()
	return c.goalRuns[executionID]
}

func (c *Core) enqueueGoalContinuation(ctx context.Context, sessionID string) (bool, error) {
	if err := lockContext(ctx, &c.queueMutationMu); err != nil {
		return false, err
	}
	defer c.queueMutationMu.Unlock()
	c.goalMu.Lock()
	defer c.goalMu.Unlock()
	if !c.goalSessions[sessionID].Armed {
		return false, nil
	}
	goal, err := c.sessions.Goal(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if goal == nil || goal.Status != GoalActive {
		return false, nil
	}
	options := c.goalSessions[sessionID]
	input, _ := ParseTaskInput("Continue making concrete progress toward the active goal. Verify the full objective before marking it complete. The objective and budget are supplied in runtime context.")
	_, _, err = c.submitTurnAdmitted(ctx, sessionID, TurnRequest{Input: input, automaticGoal: true, NoInteractiveDecisions: options.NoInteractiveDecisions}, StartIfIdle, false)
	return err == nil, err
}

func (c *Core) admitGoalExecution(ctx context.Context, sessionID, executionID string, request storedTurnRequest) error {
	if request.GoalID == "" {
		return nil
	}
	c.goalMu.Lock()
	defer c.goalMu.Unlock()
	goal, err := c.sessions.Goal(ctx, sessionID)
	if err != nil {
		return err
	}
	// A user message remains valid after its associated goal finishes or is
	// replaced. Automatic continuations must never escape goal ownership.
	if !request.AutomaticGoal && (goal == nil || goal.ID != request.GoalID || goal.Status == GoalComplete) {
		return nil
	}
	if goal == nil || goal.ID != request.GoalID || goal.Revision != request.GoalRevision || goal.Status != GoalActive || !c.goalSessions[sessionID].Armed {
		return errStaleGoal
	}
	if c.goalRuns == nil {
		c.goalRuns = make(map[string]goalExecution)
	}
	c.goalRuns[executionID] = goalExecution{ID: goal.ID, Revision: goal.Revision, Automatic: request.AutomaticGoal, NoInteractiveDecisions: request.NoInteractiveDecisions}
	return nil
}

func (c *Core) goalBeforeRequest(ctx context.Context, sessionID, executionID string) error {
	ref := c.goalRun(executionID)
	if ref.ID == "" {
		return nil
	}
	goal, err := c.sessions.Goal(ctx, sessionID)
	if err != nil {
		return err
	}
	if goal == nil || goal.ID != ref.ID || goal.Revision != ref.Revision {
		return errStaleGoal
	}
	if goal.Status == GoalPaused || goal.Status == GoalBudgetLimited {
		return model.ErrTokenBudgetExceeded
	}
	return nil
}

func (c *Core) accountGoalUsage(state TaskState, usage Usage, agentID string, estimatedInput int64, trace model.RequestTrace) {
	ref := c.goalRun(state.ExecutionID)
	if ref.ID == "" {
		return
	}
	key := fmt.Sprintf("%s:%s:%s:%d", state.ExecutionID, agentID, trace.ID, trace.Attempt)
	if trace.ID == "" {
		id, _ := newID()
		key += ":" + id
	}
	_, err := c.changeGoal(context.Background(), state.SessionID, key, func(goal *Goal) (*Goal, error) {
		if goal == nil || goal.ID != ref.ID {
			return nil, errStaleGoal
		}
		input, cached := usage.InputTokens, usage.InputTokensDetails.CachedTokens
		if input <= 0 && estimatedInput > 0 {
			input, goal.Estimated = estimatedInput, true
		}
		goal.TokensUsed += max(input-cached, 0) + max(usage.OutputTokens, 0)
		goal.CachedTokens += max(cached, 0)
		goal.UpdatedAt = time.Now().UTC()
		if goal.Status == GoalActive && goal.TokenBudget != nil && goal.TokensUsed >= *goal.TokenBudget {
			goal.Status, goal.StopReason = GoalBudgetLimited, "Goal token budget exhausted"
		}
		return goal, nil
	})
	if err != nil && !errors.Is(err, errStaleGoal) {
		c.logger.Warn("account goal usage", "error", err)
	}
}

func (c *Core) finishGoalExecution(sessionID, executionID string, response json.RawMessage, runErr error, elapsed time.Duration) {
	ref := c.goalRun(executionID)
	if ref.ID == "" {
		return
	}
	state, _ := c.StateForSession(sessionID)
	// Inspect pending requests before taking goalMu; recovery reads Core state.
	pending, _ := (&SessionRuntime{core: c, sessionID: sessionID}).PendingRequests(context.Background())
	_, err := c.changeGoal(context.Background(), sessionID, "", func(goal *Goal) (*Goal, error) {
		if goal == nil || goal.ID != ref.ID {
			return nil, errStaleGoal
		}
		goal.RunMilliseconds += elapsed.Milliseconds()
		goal.UpdatedAt = time.Now().UTC()
		if goal.Status != GoalActive {
			return goal, nil
		}
		if runErr != nil && state.Status != TaskPaused {
			goal.Status, goal.StopReason = GoalBlocked, "Task failed: "+runErr.Error()
		} else if state.Status == TaskPaused {
			if len(pending) == 0 {
				goal.Status, goal.StopReason = GoalPaused, "Task paused; use /goal resume"
			}
		} else {
			var parsed model.Response
			_ = json.Unmarshal(response, &parsed)
			hasOutput := false
			for _, item := range parsed.Output {
				if message, ok := item.(model.OutputMessage); ok && len(message.Content) > 0 {
					hasOutput = true
				}
			}
			if hasOutput || ref.ToolProgress {
				goal.EmptyTurns = 0
			} else {
				goal.EmptyTurns++
			}
			if goal.EmptyTurns >= 3 {
				goal.Status, goal.StopReason = GoalBlocked, "Three consecutive turns produced no text or tool progress"
			}
		}
		return goal, nil
	})
	if err != nil && !errors.Is(err, errStaleGoal) {
		c.logger.Warn("finish goal execution", "error", err)
	}
}

func (c *Core) goalMayContinue(ctx context.Context, sessionID string) bool {
	c.goalMu.Lock()
	defer c.goalMu.Unlock()
	if !c.goalSessions[sessionID].Armed {
		return false
	}
	goal, err := c.sessions.Goal(ctx, sessionID)
	return err == nil && goal != nil && goal.Status == GoalActive
}
