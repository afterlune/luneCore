package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func (c *Core) buildAgent(profileID string, taskState TaskState) (*agentgraph.Agent, *agentruntime.UsageTrackingRequester, model.ModelProfile, error) {
	profile, ok := c.models.Profile(profileID)
	if !ok {
		return nil, nil, model.ModelProfile{}, fmt.Errorf("unknown model profile %q", profileID)
	}
	providerConfig, ok := c.models.ProviderConfig(profile.Provider)
	if !ok {
		return nil, nil, model.ModelProfile{}, fmt.Errorf("unknown provider %q for model profile %q", profile.Provider, profileID)
	}
	client, err := c.models.NewClient(profileID, c.registry, c.logger)
	if err != nil {
		return nil, nil, model.ModelProfile{}, err
	}
	requester := agentruntime.NewUsageTrackingRequester(client, "lead", profileID, profile.Provider, profile.Model)
	requester.SetMaxOutputTokens(profile.MaxOutputTokens)
	memoryRequester := agentruntime.NewUsageTrackingRequester(client, "memory", profileID, profile.Provider, profile.Model)
	memoryRequester.SetMaxOutputTokens(profile.MaxOutputTokens)
	usageObserver := func(ctx context.Context, usage publicmodel.ResponseUsage, agentID, profile, provider, modelName string, estimatedInputTokens int64) {
		state := taskState
		identity := publictool.CallIdentityFromContext(ctx)
		if current, ok := c.StateForSession(identity.SessionID); ok {
			state = current
		}
		c.onAgentUsageForTask(state, usage, agentID, profile, provider, modelName, estimatedInputTokens, model.UsagePurposeFromContext(ctx), model.RequestTraceFromContext(ctx))
	}
	requester.SetContextAgentUsageObserver(usageObserver)
	memoryRequester.SetContextAgentUsageObserver(usageObserver)
	activeCommands, stopCommands := c.commandHooks()
	agent, err := agentgraph.NewWithOptions(requester, c.registry, agentgraph.AgentOptions{
		ID: "lead", Name: "Lead", Profile: profileID, Provider: profile.Provider, Model: profile.Model,
		Logger: c.logger, ActiveCommands: activeCommands, StopCommands: stopCommands,
		Protocol: providerConfig.Protocol, ServerCompaction: providerConfig.ServerCompaction,
		SupportsInputAudio: profile.SupportsInputAudio, Execution: agentExecutionOptions(taskState.Execution),
		Team:    c.team,
		AskUser: true,
		ModelObserver: func(state agentgraph.State, stage string, failure publicmodel.Failure) {
			c.onModelRecoveryEvent(taskState.SessionID, state, stage, failure)
		},
		MemoryEnabled:              !c.memoryConfig.Disabled,
		MemoryRequester:            memoryRequester,
		MemorySummaryFor:           c.memorySummaryForTask,
		MemoryWriter:               c.saveExtractedMemories,
		MemoryObserver:             c.onMemoryEvent,
		ProjectInstructionsForTask: c.projectInstructionsForTask,
		ProjectInstructionsForTool: c.projectInstructionsForTool,
		ToolVisibleForMode:         c.registry.ToolVisibleForMode,
		ToolReadOnly: func(name string) bool {
			metadata, ok := c.registry.ToolMetadata(name)
			return ok && metadata.ReadOnly
		},
		CurrentPermissionMode: func() string { return string(c.PermissionMode()) },
		StreamObserverFor: func(state agentgraph.State, event model.ModelStreamEvent) {
			currentTask := taskState
			if current, ok := c.StateForSession(taskState.SessionID); ok {
				currentTask = current
			}
			activeModel := state.Model
			if state.ModelOverride != "" {
				activeModel = state.ModelOverride
			}
			c.onModelStreamForTask(currentTask, state.AgentID, state.AgentName, state.ModelProfile, state.Provider, activeModel, event)
		},
		OnBranchStarted: func(ctx context.Context, state agentgraph.State) {
			c.commands.OpenAgent(state.ExecutionID, state.AgentID)
			identity := publictool.CallIdentityFromContext(ctx)
			c.emit(Event{Kind: EventAgentStarted, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID,
				AgentID: state.AgentID, AgentName: state.AgentName, AgentRole: "worker", TeamRunID: state.TeamRunID,
				AgentTask: state.TeamTask, ContextWindow: state.ContextWindow,
				ModelProfile: state.ModelProfile, Provider: state.Provider, Model: state.Model, Status: TaskRunning})
		},
		OnBranchFinished: func(ctx context.Context, state agentgraph.State, branchErr error) {
			identity := publictool.CallIdentityFromContext(ctx)
			status := TaskCompleted
			if branchErr != nil {
				status = TaskFailed
				if errors.Is(branchErr, context.Canceled) {
					status = TaskCancelled
				}
			}
			c.emit(Event{Kind: EventAgentFinished, SessionID: identity.SessionID, TaskID: identity.TaskID, ExecutionID: identity.ExecutionID,
				AgentID: state.AgentID, AgentName: state.AgentName, AgentRole: "worker", TeamRunID: state.TeamRunID,
				AgentTask: state.TeamTask, ContextWindow: state.ContextWindow,
				ModelProfile: state.ModelProfile, Provider: state.Provider, Model: state.Model, Status: status, Err: errorString(branchErr)})
		},
		CompactionObserver: func(event agentgraph.ContextCompactionEvent) {
			currentTask := taskState
			if current, ok := c.StateForSession(taskState.SessionID); ok {
				currentTask = current
			}
			details := &ContextCompactionDetails{
				Stage: event.Stage, Mode: event.Mode, Reason: event.Reason, Strategy: event.Strategy,
				BeforeTokens: event.BeforeTokens, AfterTokens: event.AfterTokens, Threshold: event.Threshold,
				Turns: event.Turns, Err: errorString(event.Error),
			}
			c.emit(Event{Kind: EventContextCompaction, TaskID: currentTask.TaskID, ExecutionID: currentTask.ExecutionID, SessionID: currentTask.SessionID,
				Provider: profile.Provider, ModelProfile: profileID, Model: currentTask.Model, Err: errorString(event.Error), Compaction: details})
		},
	})
	if err != nil {
		return nil, nil, model.ModelProfile{}, err
	}
	return agent, requester, profile, nil
}

func (c *Core) onExecutionBlocker(ctx context.Context, state agentgraph.State, blockerID, toolName string) {
	identity := publictool.CallIdentityFromContext(ctx)
	c.emit(Event{
		Kind: EventExecutionBlocked, SessionID: identity.SessionID, TaskID: identity.TaskID,
		ExecutionID: state.ExecutionID, BlockerID: blockerID, BlockerTool: toolName,
		Status: TaskPaused,
	})
}

func (c *Core) configureRuntime(profileID, reasoningEffort string, taskState TaskState) (*agentruntime.Responder, model.ModelProfile, error) {
	agent, requester, profile, err := c.buildAgent(profileID, taskState)
	if err != nil {
		return nil, model.ModelProfile{}, err
	}
	runtime := agentruntime.NewResponder(c.checkpoints, c.callbackLimiter, c.logger)
	runtime.SetRunModel(agent, requester, profile.Model, reasoningEffort)
	return runtime, profile, nil
}
