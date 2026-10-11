package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

type teamExecutor struct {
	core        *Core
	models      *model.ProviderCatalog
	tools       *tool.Registry
	checkpoints graph.Store[agentgraph.State]
	store       *teamStore
}

func (t *teamExecutor) Profiles() map[string]model.ModelProfile {
	return t.models.Profiles()
}

func (t *teamExecutor) MaxParallelAgents() int {
	return t.models.ParallelLimit()
}

func (t *teamExecutor) Run(ctx context.Context, runID string, assignments []agentgraph.TeamAssignment, options agentgraph.TaskOptions) ([]agentgraph.TeamMemberResult, error) {
	if len(assignments) == 0 || len(assignments) > t.MaxParallelAgents() {
		return nil, fmt.Errorf("team task count must be between 1 and %d", t.MaxParallelAgents())
	}
	parentID, callID, ok := strings.Cut(runID, ":")
	if !ok || parentID == "" || callID == "" {
		return nil, errors.New("team run ID must include a parent run and tool call ID")
	}
	record, _, err := t.store.GetOrCreate(parentID, callID, assignments, t.models.Profiles())
	if err != nil {
		return nil, err
	}
	results := make([]agentgraph.TeamMemberResult, len(record.Members))
	errs := make([]error, len(record.Members))
	var wait sync.WaitGroup
	for index, member := range record.Members {
		index, member := index, member
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index], errs[index] = t.runMember(ctx, record.ID, member, options)
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (t *teamExecutor) runMember(ctx context.Context, teamRunID string, member teamMemberRecord, taskOptions agentgraph.TaskOptions) (agentgraph.TeamMemberResult, error) {
	result := agentgraph.TeamMemberResult{
		ID: member.ID, Model: member.ModelID, Provider: member.Provider,
	}
	if member.Status == "completed" || member.Status == "failed" {
		result.Text, result.Error = member.Text, member.Error
		return result, nil
	}
	profile, ok := t.models.Profile(member.Model)
	if !ok {
		return result, fmt.Errorf("team model profile %q is no longer configured", member.Model)
	}
	if member.Provider != profile.Provider || member.ModelID != profile.Model {
		return result, fmt.Errorf("team member %q model profile changed; the paused member remains pinned to its original provider and model", member.ID)
	}
	providerConfig, ok := t.models.ProviderConfig(profile.Provider)
	if !ok {
		return result, fmt.Errorf("team member %q provider %q is no longer configured", member.ID, profile.Provider)
	}
	client, err := t.models.NewClient(member.Model, t.tools)
	if err != nil {
		return result, err
	}
	requester := &usageTrackingRequester{
		inner: client, agentID: member.ID, profile: member.Model,
		providerName: profile.Provider, modelName: profile.Model,
		observer: t.core.onAgentUsage,
	}
	agentOptions := agentgraph.AgentOptions{
		ID: member.ID, Name: member.ID, Profile: member.Model,
		Provider: profile.Provider, Protocol: providerConfig.Protocol,
		ServerCompaction: providerConfig.ServerCompaction, Model: profile.Model,
		SupportsInputAudio: profile.SupportsInputAudio,
		StreamObserver: func(event model.ModelStreamEvent) {
			t.core.onModelStream(member.ID, member.ID, member.Model, profile.Provider, profile.Model, event)
		},
	}
	agent, err := agentgraph.NewWithOptions(requester, t.tools, agentOptions)
	if err != nil {
		return result, err
	}
	childRunID := teamRunID + ":" + member.ID
	if member.Status == "paused" || member.Status == "running" {
		state, found, stateErr := pendingStateForRunID(ctx, t.checkpoints, childRunID)
		if stateErr != nil && !errors.Is(stateErr, checkpoint.ErrNotFound) {
			return result, fmt.Errorf("inspect team member checkpoint: %w", stateErr)
		}
		if stateErr == nil {
			if !found {
				return result, checkpointSettingsVersionError(0)
			}
			if state.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
				return result, checkpointSettingsVersionError(state.TaskSettingsVersion)
			}
		}
	}
	graphOptions := graph.Options[agentgraph.State]{Store: t.checkpoints, Observer: graph.ObserverFunc(logGraphEvent)}
	updated := member
	updated.Status = "running"
	updated.Error = ""
	if err := t.store.UpdateMember(teamRunID, updated); err != nil {
		return result, err
	}
	t.core.emit(Event{
		Kind: EventAgentStarted, SessionID: t.core.State().SessionID, TaskID: t.core.State().TaskID,
		AgentID: member.ID, AgentName: member.ID, AgentRole: "worker", TeamRunID: teamRunID,
		ModelProfile: member.Model, Provider: profile.Provider, Model: profile.Model, Status: TaskRunning,
	})
	var graphResult graph.Result[agentgraph.State]
	if member.Status == "paused" || member.Status == "running" {
		graphResult, err = agent.Recover(ctx, childRunID, nil, graphOptions)
		if errors.Is(err, checkpoint.ErrNotFound) {
			err = nil
		}
	}
	if member.Status == "queued" || errors.Is(err, checkpoint.ErrNotFound) || graphResult.Status == "" {
		input, inputErr := model.NewResponseInput(member.Task)
		if inputErr != nil {
			return result, inputErr
		}
		graphResult, err = agent.Start(ctx, childRunID, nil, input, taskOptions, graphOptions)
	}
	if err != nil {
		if ctx.Err() != nil {
			updated.Status = "paused"
			_ = t.store.UpdateMember(teamRunID, updated)
			t.emitMemberFinished(teamRunID, member, profile, TaskPaused, err.Error())
			return result, ctx.Err()
		}
		return t.failMember(teamRunID, member, profile, result, err)
	}
	if ctx.Err() != nil || graphResult.Status == graph.StatusCancelled || graphResult.Status == graph.StatusWaiting {
		updated.Status = "paused"
		_ = t.store.UpdateMember(teamRunID, updated)
		t.emitMemberFinished(teamRunID, member, profile, TaskPaused, "task interrupted")
		return result, ctx.Err()
	}
	if graphResult.Checkpoint.Final == nil {
		return t.failMember(teamRunID, member, profile, result, fmt.Errorf("team member ended with status %s and no final state", graphResult.Status))
	}
	state := graphResult.Checkpoint.Final
	if state.TaskError != "" && state.FinalResponse == nil {
		return t.failMember(teamRunID, member, profile, result, errors.New(state.TaskError))
	}
	if state.FinalResponse == nil {
		return t.failMember(teamRunID, member, profile, result, errors.New("team member completed without a response"))
	}
	result.Text = teamResponseText(state.FinalResponse)
	updated.Status = "completed"
	updated.Text = result.Text
	updated.Error = ""
	if err := t.store.UpdateMember(teamRunID, updated); err != nil {
		return result, err
	}
	_ = t.checkpoints.Delete(context.Background(), childRunID)
	t.emitMemberFinished(teamRunID, member, profile, TaskCompleted, "")
	return result, nil
}

func (t *teamExecutor) failMember(teamRunID string, member teamMemberRecord, profile model.ModelProfile, result agentgraph.TeamMemberResult, err error) (agentgraph.TeamMemberResult, error) {
	result.Error = err.Error()
	updated := member
	updated.Status = "failed"
	updated.Error = result.Error
	if saveErr := t.store.UpdateMember(teamRunID, updated); saveErr != nil {
		return result, errors.Join(err, saveErr)
	}
	_ = t.checkpoints.Delete(context.Background(), teamRunID+":"+member.ID)
	t.emitMemberFinished(teamRunID, member, profile, TaskFailed, result.Error)
	return result, nil
}

func (t *teamExecutor) emitMemberFinished(teamRunID string, member teamMemberRecord, profile model.ModelProfile, status TaskStatus, message string) {
	t.core.emit(Event{
		Kind: EventAgentFinished, SessionID: t.core.State().SessionID, TaskID: t.core.State().TaskID,
		AgentID: member.ID, AgentName: member.ID, AgentRole: "worker", TeamRunID: teamRunID,
		ModelProfile: member.Model, Provider: profile.Provider, Model: profile.Model, Status: status, Err: message,
	})
}

func teamResponseText(response *model.Response) string {
	if response == nil {
		return ""
	}
	var output strings.Builder
	for _, item := range response.Output {
		message, ok := item.(model.OutputMessage)
		if !ok {
			continue
		}
		for _, part := range message.Content {
			if text, ok := part.(model.OutputText); ok {
				output.WriteString(text.Text)
			}
		}
	}
	return strings.TrimSpace(output.String())
}
