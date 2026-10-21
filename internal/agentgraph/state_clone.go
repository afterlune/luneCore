package agentgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func cloneState(state State) (State, error) {
	if math.IsNaN(state.AutoCompactThreshold) || math.IsInf(state.AutoCompactThreshold, 0) {
		return State{}, errors.New("clone state: auto compact threshold must be finite")
	}
	clone := state
	if state.ModelFailure != nil {
		failure := *state.ModelFailure
		clone.ModelFailure = &failure
	}
	clone.PlanAuthorization = clonePlanAuthorization(state.PlanAuthorization)
	clone.ProjectInstructions = append([]ProjectInstruction(nil), state.ProjectInstructions...)
	var err error
	if clone.History, err = model.CloneResponseInput(state.History); err != nil {
		return State{}, fmt.Errorf("clone history: %w", err)
	}
	if clone.Transcript, err = model.CloneResponseInput(state.Transcript); err != nil {
		return State{}, fmt.Errorf("clone transcript: %w", err)
	}
	clone.LastRequest.Changes = append([]string(nil), state.LastRequest.Changes...)
	clone.SelectedCategories = append([]string(nil), state.SelectedCategories...)
	clone.PendingCalls = append([]model.OutputFunctionCall(nil), state.PendingCalls...)
	clone.VisibleTools = append([]string(nil), state.VisibleTools...)
	if clone.FinalResponse, err = model.CloneModelResponse(state.FinalResponse); err != nil {
		return State{}, fmt.Errorf("clone final response: %w", err)
	}
	clone.TeamAssignments = append([]TeamAssignment(nil), state.TeamAssignments...)
	clone.TeamBranchModes = append([]string(nil), state.TeamBranchModes...)
	if state.TeamForkBaseInput != nil && (state.TeamForkBaseInput.Text != nil || state.TeamForkBaseInput.Items != nil) {
		forkInput, err := model.CloneResponseInput(*state.TeamForkBaseInput)
		if err != nil {
			return State{}, fmt.Errorf("clone team fork input: %w", err)
		}
		clone.TeamForkBaseInput = &forkInput
	} else {
		clone.TeamForkBaseInput = nil
	}
	if state.UserQuestions != nil {
		clone.UserQuestions = make([]model.UserQuestionItem, len(state.UserQuestions))
		for index, question := range state.UserQuestions {
			clone.UserQuestions[index] = question
			clone.UserQuestions[index].Options = append([]model.UserQuestionOption(nil), question.Options...)
		}
	}
	clone.UserAnswers = append([]model.UserQuestionResponse(nil), state.UserAnswers...)
	if len(state.TaskInput) > 0 {
		if !json.Valid(state.TaskInput) {
			return State{}, errors.New("clone state: task input is invalid JSON")
		}
		clone.TaskInput = append(json.RawMessage(nil), state.TaskInput...)
	} else {
		clone.TaskInput = nil
	}
	return clone, nil
}

func clonePlanAuthorization(authorization publictool.PlanAuthorization) publictool.PlanAuthorization {
	authorization.Paths = append([]publictool.PathGrant(nil), authorization.Paths...)
	authorization.AllowedTools = append([]string(nil), authorization.AllowedTools...)
	authorization.ApprovedHighRiskCommands = append([]string(nil), authorization.ApprovedHighRiskCommands...)
	return authorization
}
