package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/session"
)

func (c *Core) acceptanceArtifacts(ctx context.Context, sessionID string) ([]json.RawMessage, error) {
	state, found, err := c.runtime.PendingStateFor(ctx, sessionID)
	if err != nil || !found {
		return nil, err
	}
	values := make([]json.RawMessage, 4)
	if len(state.AcceptanceReports) > 0 {
		values[0], err = json.Marshal(state.AcceptanceReports)
	}
	if err == nil && len(state.AcceptanceCriteria) > 0 {
		values[1], err = json.Marshal(state.AcceptanceCriteria)
	}
	if err == nil && len(state.AcceptanceEvidence) > 0 {
		values[2], err = json.Marshal(state.AcceptanceEvidence)
	}
	if err != nil {
		return nil, err
	}
	if state.ModelFailure != nil {
		values[3], err = json.Marshal(state.ModelFailure)
	}
	return values, err
}

func (c *Core) resolveAcceptance(ctx context.Context, sessionID string, resolution AcceptanceResolution) error {
	if c == nil || ctx == nil || sessionID == "" || resolution.RunID == "" || resolution.InvocationID == "" || resolution.CallID == "" {
		return errors.New("session runtime and exact acceptance wait identity are required")
	}
	if resolution.Decision != AcceptanceContinue && resolution.Decision != AcceptanceConfirm {
		return fmt.Errorf("acceptance decision must be %q or %q", AcceptanceContinue, AcceptanceConfirm)
	}
	pending, found, err := c.pendingAcceptance(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("inspect pending acceptance: %w", err)
	}
	if !found || pending.RunID != resolution.RunID || pending.InvocationID != resolution.InvocationID || pending.CallID != resolution.CallID {
		return errors.New("acceptance wait is stale or no longer pending in this session")
	}
	if resolution.Decision == AcceptanceConfirm && (len(pending.Criteria) == 0 || pending.CandidateID == "") {
		return errors.New("cannot confirm acceptance before criteria and a candidate exist")
	}
	payload, err := json.Marshal(struct {
		Decision AcceptanceDecision `json:"decision"`
	}{Decision: resolution.Decision})
	if err != nil {
		return err
	}
	input := session.GraphResumeInput{RunID: resolution.RunID, InvocationID: resolution.InvocationID, CallID: resolution.CallID, Payload: payload}
	if _, err := c.sessions.PutGraphResumeInput(ctx, sessionID, input); err != nil {
		return err
	}
	if state, ok := c.StateForSession(sessionID); ok && state.Status == TaskRunning && state.ExecutionID == pending.ExecutionID {
		return nil
	}
	if _, _, err := c.recoverSession(ctx, sessionID, false); err != nil {
		if errors.Is(err, ErrSessionBusy) {
			return nil
		}
		return fmt.Errorf("resume acceptance decision: %w", err)
	}
	return nil
}

func (c *Core) hasAcceptanceResolutionInput(ctx context.Context, sessionID string) (bool, error) {
	pending, found, err := c.pendingAcceptance(ctx, sessionID)
	if err != nil || !found {
		return false, err
	}
	inputs, err := c.graphResumeInputs(ctx, sessionID, c.runtime.CheckpointID(sessionID))
	if err != nil {
		return false, err
	}
	for _, input := range inputs {
		if input.CallID != pending.CallID || input.InvocationID != pending.InvocationID {
			continue
		}
		var decision struct {
			Decision AcceptanceDecision `json:"decision"`
		}
		if json.Unmarshal(input.Payload, &decision) == nil && (decision.Decision == AcceptanceContinue || decision.Decision == AcceptanceConfirm) {
			return true, nil
		}
	}
	return false, nil
}
