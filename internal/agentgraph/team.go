package agentgraph

import (
	"context"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

func (a *Agent) mergeTeam(ctx context.Context, _ graph.CallInfo, parent State, branches []graph.BranchResult[State]) (State, error) {
	results := make([]TeamMemberResult, len(branches))
	for index, branch := range branches {
		if a.options.StopCommands != nil {
			state := branch.State
			if state.AgentID == "" && index < len(parent.TeamAssignments) {
				state.ExecutionID = parent.ExecutionID
				state.AgentID = parent.AgentID + "/" + parent.TeamAssignments[index].ID
			}
			if err := a.options.StopCommands(state); err != nil {
				return State{}, err
			}
		}
		assignment := TeamAssignment{}
		if index < len(parent.TeamAssignments) {
			assignment = parent.TeamAssignments[index]
		}
		contextMode := branch.State.TeamDispatchMode
		if contextMode == "" && index < len(parent.TeamBranchModes) {
			contextMode = parent.TeamBranchModes[index]
		}
		member := TeamMemberResult{ID: assignment.ID, Model: assignment.Model, ContextMode: contextMode}
		if branch.OK() {
			member = TeamMemberResult{ID: branch.State.TeamTaskID, Model: branch.State.TeamModel, Provider: branch.State.TeamProvider, ContextMode: contextMode, Text: branch.State.TeamText, Error: branch.State.TeamError}
			if a.options.OnBranchFinished != nil {
				var branchErr error
				if member.Error != "" {
					branchErr = errors.New(member.Error)
				}
				a.options.OnBranchFinished(ctx, branch.State, branchErr)
			}
		} else if branch.Pending() {
			member = TeamMemberResult{ID: assignment.ID, Model: assignment.Model, ContextMode: contextMode, Error: "cancelled after the minimum success threshold was reached"}
			if profile, ok := a.options.Team.Profiles()[assignment.Model]; ok {
				member.Provider, member.Model = profile.Provider, profile.Model
			}
			if a.options.OnBranchFinished != nil {
				pending := branch.State
				pending.TeamRunID = parent.TeamDispatchID
				a.options.OnBranchFinished(ctx, pending, context.Canceled)
			}
		} else {
			member.Error = branch.Error
			if member.Error == "" {
				member.Error = "team branch ended without a result"
			}
			if profile, ok := a.options.Team.Profiles()[assignment.Model]; ok {
				member.Provider = profile.Provider
				member.Model = profile.Model
			}
			if a.options.OnBranchFinished != nil {
				failed := State{
					RunID: parent.RunID, ExecutionID: parent.ExecutionID, AgentID: parent.AgentID + "/" + assignment.ID,
					AgentName: assignment.ID, TeamTaskID: assignment.ID, TeamBranch: true, TeamStarted: true, TeamRunID: parent.TeamDispatchID,
					TeamModel: member.Model, TeamProvider: member.Provider, Model: member.Model, ModelProfile: assignment.Model,
				}
				a.options.OnBranchFinished(ctx, failed, errors.New(member.Error))
			}
		}
		results[index] = member
	}
	if parent.PendingIndex < 0 || parent.PendingIndex >= len(parent.PendingCalls) {
		return State{}, errors.New("team join lost its originating tool call")
	}
	call := parent.PendingCalls[parent.PendingIndex]
	output := model.InputFunctionCallOutput{CallID: call.CallID, Output: model.NewToolOutputText(fmt.Sprintf("%s\nDelegation remaining for this agent: %d; maximum depth: %d.", formatTeamResults(results), parent.Execution.MaxTeamChildrenPerAgent-parent.TeamChildrenCreated, parent.Execution.MaxTeamDepth))}
	parent = appendToolOutput(parent, output)
	parent.TeamAssignments, parent.TeamDispatchID = nil, ""
	parent.TeamBranchModes = nil
	parent.TeamMinimumSuccesses = 0
	parent.TeamDispatchMode = ""
	parent.TeamForkBaseInput = nil
	return parent, nil
}

func (a *Agent) teamQuorumReached(parent State, branches []graph.BranchResult[State]) bool {
	if parent.TeamMinimumSuccesses <= 0 {
		return false
	}
	var successful int
	for _, branch := range branches {
		if branch.OK() && branch.State.TeamError == "" {
			successful++
		}
	}
	return successful >= parent.TeamMinimumSuccesses
}
