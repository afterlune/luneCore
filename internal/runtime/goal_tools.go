package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
)

type goalTool struct { core *Core; update bool }

func (t goalTool) Definition() publictool.Definition {
	if !t.update { return publictool.Definition{Name:"get_goal", Description:"Read the current session goal, status, uncached token usage, optional token budget and stop reason.", Parameters:json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)} }
	return publictool.Definition{Name:"update_goal", Description:"Mark the active user goal complete only after verifying the entire objective, or blocked when no meaningful progress is possible without user input or an external change. Give a concrete reason. Never narrow the objective to claim completion. Only the lead agent may update a goal. Creation, pausing, resuming and budget changes belong to the user.", Parameters:json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["complete","blocked"]},"reason":{"type":"string"}},"required":["status","reason"],"additionalProperties":false}`)}
}

func (t goalTool) Run(ctx context.Context, arguments json.RawMessage) publictool.Result {
	identity := publictool.CallIdentityFromContext(ctx)
	if identity.SessionID == "" { return publictool.Result{Err:errors.New("goal tools require a session identity")} }
	var goal *Goal
	var err error
	if !t.update { goal, err = t.core.sessions.Goal(ctx, identity.SessionID) } else {
		if identity.AgentID != "lead" { return publictool.Result{Err:errors.New("only the lead agent may update the goal")} }
		var input struct { Status GoalStatus `json:"status"`; Reason string `json:"reason"` }
		if err := json.Unmarshal(arguments, &input); err != nil { return publictool.Result{Err:err} }
		if (input.Status != GoalComplete && input.Status != GoalBlocked) || strings.TrimSpace(input.Reason) == "" { return publictool.Result{Err:errors.New("status complete or blocked and a concrete reason are required")} }
		ref := t.core.goalRun(identity.ExecutionID)
		goal, err = t.core.changeGoal(ctx, identity.SessionID, "", func(goal *Goal) (*Goal, error) {
			if goal == nil || ref.ID == "" || goal.ID != ref.ID || goal.Revision != ref.Revision || goal.Status != GoalActive { return nil, errStaleGoal }
			goal.Status, goal.StopReason, goal.UpdatedAt = input.Status, input.Reason, time.Now().UTC()
			return goal, nil
		})
	}
	if err != nil { return publictool.Result{Err:err} }
	data, err := json.Marshal(struct{ Goal *Goal `json:"goal"` }{goal})
	return publictool.Result{Output:string(data), Err:err}
}

func (c *Core) registerGoalTools() error {
	registrations, err := tool.HostToolRegistrations([]publictool.Registration{
		{Tool:goalTool{core:c}, Metadata:publictool.Metadata{Category:publictool.Category{Name:"goal", Description:"Inspect and complete the current persistent user goal."}, PermissionClass:publictool.PermissionSessionControl, ReadOnly:true, AlwaysVisible:true}},
		{Tool:goalTool{core:c, update:true}, Metadata:publictool.Metadata{Category:publictool.Category{Name:"goal", Description:"Inspect and complete the current persistent user goal."}, PermissionClass:publictool.PermissionSessionControl, ReadOnly:false, AlwaysVisible:true}},
	})
	if err != nil { return err }
	for _, registration := range registrations { if err := c.registry.Register(registration); err != nil { return err } }
	return nil
}
