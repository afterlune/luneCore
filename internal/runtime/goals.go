package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/session"
)

type Goal = session.Goal
type GoalStatus = session.GoalStatus
const (
	GoalActive = session.GoalActive
	GoalPaused = session.GoalPaused
	GoalBlocked = session.GoalBlocked
	GoalBudgetLimited = session.GoalBudgetLimited
	GoalComplete = session.GoalComplete
)

type GoalCreateOptions struct {
	Objective string
	TokenBudget *int64
	NoInteractiveDecisions bool
}

type GoalResumeOptions struct { NoInteractiveDecisions bool }

type goalExecution struct {
	ID string
	Revision int64
	Automatic bool
	ToolProgress bool
	NoInteractiveDecisions bool
}

var errStaleGoal = errors.New("goal changed or is no longer active")

func (r *SessionRuntime) GetGoal(ctx context.Context) (*Goal, error) {
	if r == nil || r.core == nil || ctx == nil { return nil, errors.New("session runtime and context are required") }
	if _, err := r.core.GetSession(ctx, r.sessionID); err != nil { return nil, err }
	return r.core.sessions.Goal(ctx, r.sessionID)
}

// changeGoal serializes all goal mutations with automatic turn admission. Never
// wait for a running task while holding goalMu: usage and tools also take it.
func (c *Core) changeGoal(ctx context.Context, sessionID, receipt string, mutate func(*Goal) (*Goal, error)) (*Goal, error) {
	c.goalMu.Lock()
	defer c.goalMu.Unlock()
	return c.changeGoalLocked(ctx, sessionID, receipt, mutate)
}

func (c *Core) changeGoalLocked(ctx context.Context, sessionID, receipt string, mutate func(*Goal) (*Goal, error)) (*Goal, error) {
	goal, changed, err := c.sessions.ModifyGoal(ctx, sessionID, receipt, mutate, func(next *Goal) json.RawMessage {
		kind := EventGoalUpdated
		if next == nil { kind = EventGoalCleared }
		return mustMarshalEvent(Event{Kind: kind, SessionID: sessionID, Goal: next})
	})
	if changed { c.notifyEventHub(sessionID) }
	return goal, err
}

func validateGoalBudget(budget *int64) error {
	if budget != nil && *budget <= 0 { return errors.New("goal token budget must be a positive integer, or unset") }
	return nil
}

func (r *SessionRuntime) CreateGoal(ctx context.Context, options GoalCreateOptions) (*Goal, error) {
	if _, err := r.GetGoal(ctx); err != nil { return nil, err }
	objective := strings.TrimSpace(options.Objective)
	if objective == "" { return nil, errors.New("goal objective must not be empty") }
	if err := validateGoalBudget(options.TokenBudget); err != nil { return nil, err }
	state, err := r.State(ctx)
	if err != nil { return nil, err }
	if state.Task.Status == TaskRunning || state.Task.Status == TaskQueued || state.Task.Status == TaskPaused || len(state.Queue) > 0 { return nil, ErrSessionBusy }
	id, err := newID()
	if err != nil { return nil, err }
	goal, err := r.core.changeGoal(ctx, r.sessionID, "", func(previous *Goal) (*Goal, error) {
		if previous != nil && previous.Status != GoalComplete { return nil, errors.New("an unfinished goal exists; pause and edit it, or clear it first") }
		now := time.Now().UTC()
		var budget *int64
		if options.TokenBudget != nil { n := *options.TokenBudget; budget = &n }
		goal := &Goal{ID: id, Revision: 1, Objective: objective, Status: GoalActive, TokenBudget: budget, CreatedAt: now, UpdatedAt: now}
		if r.core.goalArmed == nil { r.core.goalArmed = make(map[string]bool) }
		r.core.goalArmed[r.sessionID] = true
		if r.core.goalRuns == nil { r.core.goalRuns = make(map[string]goalExecution) }
		r.core.goalRuns["session:"+r.sessionID] = goalExecution{NoInteractiveDecisions:options.NoInteractiveDecisions}
		return goal, nil
	})
	if err == nil {
		r.core.scheduleQueueDrain(r.sessionID)
	}
	return goal, err
}

func (r *SessionRuntime) PauseGoal(ctx context.Context) (*Goal, error) {
	return r.pauseGoal(ctx, true)
}

func (r *SessionRuntime) pauseGoal(ctx context.Context, wait bool) (*Goal, error) {
	if _, err := r.GetGoal(ctx); err != nil { return nil, err }
	goal, err := r.core.changeGoal(ctx, r.sessionID, "", func(goal *Goal) (*Goal, error) {
		if goal == nil { return nil, errors.New("no goal is set") }
		delete(r.core.goalArmed, r.sessionID)
		if goal.Status != GoalComplete && goal.Status != GoalBudgetLimited {
			goal.Status, goal.StopReason, goal.UpdatedAt = GoalPaused, "Paused by user", time.Now().UTC()
		}
		return goal, nil
	})
	if err != nil { return nil, err }
	// Interrupt queued automatic work as well as the current goal execution.
	queue, err := r.core.sessions.QueuedTurns(ctx, r.sessionID)
	if err != nil { return goal, err }
	for _, item := range queue {
		var request storedTurnRequest
		if json.Unmarshal(item.Request, &request) == nil && request.GoalID == goal.ID {
			if item.Status == session.QueuedTurnQueued && !request.AutomaticGoal {
				if err := r.core.sessions.SetQueuedTurnStatus(ctx, r.sessionID, item.QueueID, session.QueuedTurnPaused); err != nil { return goal, err }
				continue
			}
			r.core.interruptExecution(r.sessionID, item.ExecutionID)
			if wait && item.Status == session.QueuedTurnActive {
				if err := r.core.waitExecutionStopped(ctx, item.ExecutionID); err != nil { return goal, err }
			}
		}
	}
	return goal, nil
}

func (c *Core) waitExecutionStopped(ctx context.Context, executionID string) error {
	c.mu.RLock()
	done := c.executionRuns[executionID]
	c.mu.RUnlock()
	if done != nil { select { case <-done: case <-ctx.Done(): return ctx.Err() } }
	return nil
}

func (r *SessionRuntime) ResumeGoal(ctx context.Context, options ...GoalResumeOptions) (*Goal, error) {
	if len(options)>1 { return nil, errors.New("at most one goal resume options value is allowed") }
	previous, err := r.GetGoal(ctx)
	if err != nil { return nil, err }
	state, err := r.State(ctx)
	if err != nil { return nil, err }
	if state.Task.Status==TaskRunning || state.Task.Status==TaskQueued { return nil, ErrSessionBusy }
	pending, found, err := r.core.pendingState(ctx,r.sessionID)
	if err != nil { return nil, err }
	if found && (previous==nil || pending.GoalID!=previous.ID || pending.GoalRevision!=previous.Revision) { return nil, errors.New("a different task is paused; resolve or discard it first") }
	goal, err := r.core.changeGoal(ctx, r.sessionID, "", func(goal *Goal) (*Goal, error) {
		if goal == nil { return nil, errors.New("no goal is set") }
		if goal.Status == GoalComplete { return nil, errors.New("goal is complete; create a new goal") }
		if goal.TokenBudget != nil && goal.TokensUsed >= *goal.TokenBudget { return nil, errors.New("goal budget exhausted; increase it or use /goal budget off before resuming") }
		goal.Status, goal.StopReason, goal.EmptyTurns, goal.UpdatedAt = GoalActive, "", 0, time.Now().UTC()
		if r.core.goalArmed == nil { r.core.goalArmed = make(map[string]bool) }
		r.core.goalArmed[r.sessionID] = true
		if len(options)>0 {
			if r.core.goalRuns==nil { r.core.goalRuns=make(map[string]goalExecution) }
			r.core.goalRuns["session:"+r.sessionID]=goalExecution{NoInteractiveDecisions:options[0].NoInteractiveDecisions}
		}
		return goal, nil
	})
	if err != nil { return nil, err }
	if found {
		if _, _, err := r.core.recoverSession(ctx, r.sessionID, false); err != nil {
			_, _ = r.core.changeGoal(ctx, r.sessionID, "", func(current *Goal) (*Goal, error) {
				if current == nil || current.ID != goal.ID { return nil, errStaleGoal }
				current.Status, current.StopReason = GoalPaused, err.Error()
				return current, nil
			})
			return nil, err
		}
	}
	queued, err := r.core.sessions.QueuedTurns(ctx, r.sessionID)
	if err != nil { return goal, err }
	for _, item := range queued {
		var request storedTurnRequest
		if item.Status != session.QueuedTurnPaused || json.Unmarshal(item.Request, &request) != nil || request.GoalID != goal.ID || (found && item.ExecutionID == pending.ExecutionID) { continue }
		request.GoalRevision = goal.Revision
		encoded, _ := json.Marshal(request)
		_, err := r.core.sessions.SetQueuedTurnWithEvent(ctx, r.sessionID, item.QueueID, session.QueuedTurnQueued, encoded, mustMarshalEvent(Event{Kind: EventTaskQueued, SessionID: r.sessionID, ExecutionID: item.ExecutionID, QueueID: item.QueueID, GoalID: goal.ID, GoalRevision: goal.Revision, AutomaticGoal: request.AutomaticGoal, Status: TaskQueued}))
		if err != nil { return goal, err }
	}
	r.core.scheduleQueueDrain(r.sessionID)
	return goal, nil
}

func (r *SessionRuntime) EditGoal(ctx context.Context, objective string) (*Goal, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" { return nil, errors.New("goal objective must not be empty") }
	goal, err := r.GetGoal(ctx)
	if err != nil { return nil, err }
	if goal == nil { return nil, errors.New("no goal is set") }
	if goal.Status == GoalActive { return nil, errors.New("pause the goal before editing it") }
	if err := r.discardGoalCheckpoint(ctx, goal.ID); err != nil { return nil, err }
	return r.core.changeGoal(ctx, r.sessionID, "", func(current *Goal) (*Goal, error) {
		if current == nil || current.ID != goal.ID || current.Status == GoalActive { return nil, errStaleGoal }
		current.Objective, current.Status, current.StopReason, current.UpdatedAt = objective, GoalPaused, "Objective edited", time.Now().UTC()
		current.Revision++
		return current, nil
	})
}

func (r *SessionRuntime) SetGoalBudget(ctx context.Context, budget *int64) (*Goal, error) {
	if _, err := r.GetGoal(ctx); err != nil { return nil, err }
	if err := validateGoalBudget(budget); err != nil { return nil, err }
	return r.core.changeGoal(ctx, r.sessionID, "", func(goal *Goal) (*Goal, error) {
		if goal == nil { return nil, errors.New("no goal is set") }
		goal.TokenBudget = nil
		if budget != nil { n := *budget; goal.TokenBudget = &n }
		if goal.Status == GoalActive && budget != nil && goal.TokensUsed >= *budget { goal.Status, goal.StopReason = GoalBudgetLimited, "Goal token budget exhausted" }
		goal.UpdatedAt = time.Now().UTC()
		return goal, nil
	})
}

func (r *SessionRuntime) discardGoalCheckpoint(ctx context.Context, goalID string) error {
	pending, found, err := r.core.pendingState(ctx, r.sessionID)
	if err != nil { return err }
	if found && pending.GoalID == goalID { _, err = r.core.discardPaused(ctx, r.sessionID) }
	return err
}

func (r *SessionRuntime) ClearGoal(ctx context.Context) error {
	goal, err := r.GetGoal(ctx)
	if err != nil || goal == nil { return err }
	if _, err := r.PauseGoal(ctx); err != nil { return err }
	if err := r.discardGoalCheckpoint(ctx, goal.ID); err != nil { return err }
	_, err = r.core.changeGoal(ctx, r.sessionID, "", func(current *Goal) (*Goal, error) {
		if current != nil && current.ID != goal.ID { return nil, errStaleGoal }
		return nil, nil
	})
	return err
}

func (c *Core) restoreGoal(ctx context.Context, sessionID string) error {
	c.goalMu.Lock()
	defer c.goalMu.Unlock()
	if c.goalArmed[sessionID] { return nil }
	goal, err := c.sessions.Goal(ctx, sessionID)
	if err != nil || goal == nil || goal.Status != GoalActive { return err }
	_, err = c.changeGoalLocked(ctx, sessionID, "", func(goal *Goal) (*Goal, error) {
		goal.Status, goal.StopReason, goal.UpdatedAt = GoalPaused, "Session reopened; use /goal resume", time.Now().UTC()
		return goal, nil
	})
	if err != nil { return err }
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil { return err }
	for _, item := range queued {
		var request storedTurnRequest
		if item.Status == session.QueuedTurnQueued && json.Unmarshal(item.Request, &request) == nil && request.GoalID == goal.ID {
			if err := c.sessions.SetQueuedTurnStatus(ctx, sessionID, item.QueueID, session.QueuedTurnPaused); err != nil { return err }
		}
	}
	return err
}

func goalInstructions(goal *Goal) string {
	budget := "unlimited"
	if goal.TokenBudget != nil { budget = fmt.Sprint(*goal.TokenBudget) }
	return fmt.Sprintf("%s\n\nThis is a persistent user goal. Use the language of the objective and actual user messages; automatic continuation input is host context. Make concrete progress, verify the complete objective, and call update_goal with status complete only when all required work is finished. If genuinely blocked without any productive action, use status blocked and explain why. Otherwise leave the goal active so the host can continue. Goal token usage: %d; budget: %s (uncached input plus output).", goal.Objective, goal.TokensUsed, budget)
}

// copyGoal is called while the existing fork operation owns session mutation.
// It writes a distinct paused identity and never arms automatic work.
func (c *Core) copyGoal(ctx context.Context,sourceID,targetID string)(*Goal,error){
	goal,err:=c.sessions.Goal(ctx,sourceID)
	if err!=nil || goal==nil { return nil,err }
	id,err:=newID()
	if err!=nil { return nil,err }
	goal.ID,goal.Revision,goal.Status=id,1,GoalPaused
	goal.TokensUsed,goal.CachedTokens,goal.RunMilliseconds,goal.EmptyTurns=0,0,0,0
	goal.Estimated=false
	goal.StopReason="Forked goal; use /goal resume"
	goal.CreatedAt,goal.UpdatedAt=time.Now().UTC(),time.Now().UTC()
	next,_,err:=c.sessions.ModifyGoal(ctx,targetID,"",func(*Goal)(*Goal,error){return goal,nil},func(next *Goal)json.RawMessage{return mustMarshalEvent(Event{Kind:EventGoalUpdated,SessionID:targetID,Goal:next})})
	return next,err
}
