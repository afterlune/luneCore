package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type taskRunConfig struct {
	sessionID              string
	maxRounds              int
	modelOverride          string
	executionOptions       ExecutionOptions
	executionOverride      bool
	permissionOverride     string
	planMode               bool
	authorization          publictool.PlanAuthorization
	noInteractiveDecisions bool
	resume                 bool
	executionID            string
}

type taskRunSetup struct {
	saved          Session
	settings       SessionSettings
	profile        model.ModelProfile
	effectiveModel string
	taskOptions    agentgraph.TaskOptions
	transcript     *model.ResponseInput
	modelContext   *model.ResponseInput
	contextKey     string
}

func (c *Core) prepareTaskRun(ctx context.Context, request taskRunConfig) (taskRunSetup, error) {
	request.modelOverride = strings.TrimSpace(request.modelOverride)
	saved, err := c.GetSession(ctx, request.sessionID)
	if err != nil {
		return taskRunSetup{}, err
	}
	settings := saved.Settings
	var pendingState agentgraph.State
	if request.resume {
		var ok bool
		pendingState, ok, err = c.pendingState(ctx, request.sessionID)
		if err != nil {
			return taskRunSetup{}, fmt.Errorf("inspect paused task settings: %w", err)
		}
		if !ok {
			return taskRunSetup{}, errors.New("no paused task exists in this session")
		}
		if pendingState.TaskSettingsVersion != agentgraph.CurrentTaskSettingsVersion {
			return taskRunSetup{}, checkpointSettingsVersionError(pendingState.TaskSettingsVersion)
		}
		if pendingState.ModelProfile != "" {
			settings.ModelProfile = pendingState.ModelProfile
		}
		settings.ReasoningEffort = pendingState.ReasoningEffort
	}
	profile, ok := c.models.Profile(settings.ModelProfile)
	if !ok {
		return taskRunSetup{}, fmt.Errorf("session model profile %q is not configured", settings.ModelProfile)
	}
	providerConfig, ok := c.models.ProviderConfig(profile.Provider)
	if !ok {
		return taskRunSetup{}, fmt.Errorf("provider %q is not configured", profile.Provider)
	}
	if request.resume && ((pendingState.Provider != "" && pendingState.Provider != profile.Provider) ||
		(pendingState.Model != "" && pendingState.Model != profile.Model)) {
		return taskRunSetup{}, fmt.Errorf("paused task model profile %q changed in config; it remains pinned to provider %q and model %q", settings.ModelProfile, pendingState.Provider, pendingState.Model)
	}
	if request.resume && pendingState.Protocol != "" && (pendingState.Protocol != providerConfig.Protocol || pendingState.ServerCompaction != providerConfig.ServerCompaction) {
		return taskRunSetup{}, fmt.Errorf("paused task provider protocol or compaction capability changed; it remains pinned to %q with serverCompaction=%t", pendingState.Protocol, pendingState.ServerCompaction)
	}
	if request.resume {
		settings, err = c.normalizeSessionModelSettings(settings)
	} else {
		settings, err = c.normalizeSessionSettings(settings)
	}
	if err != nil {
		return taskRunSetup{}, err
	}
	profile, _ = c.models.Profile(settings.ModelProfile)
	var taskOptions agentgraph.TaskOptions
	if request.resume {
		taskOptions = taskOptionsFromState(pendingState)
		if request.executionOverride {
			taskOptions.Execution = agentExecutionOptions(mergeTeamExecutionDefaults(c.executionDefaults, request.executionOptions))
		}
		request.maxRounds = taskOptions.MaxToolRounds
	} else {
		effective, resolveErr := c.resolveTaskSettings(settings)
		if resolveErr != nil {
			return taskRunSetup{}, resolveErr
		}
		if effective.ContextWindow <= 0 && profile.ContextWindow > 0 {
			effective.ContextWindow = profile.ContextWindow
			if effective.ReserveOutputTokens >= effective.ContextWindow {
				return taskRunSetup{}, fmt.Errorf("reserveOutputTokens must be smaller than model profile %q contextWindow", settings.ModelProfile)
			}
		}
		taskOptions = agentgraph.TaskOptions{
			MaxToolRounds: request.maxRounds, ReasoningEffort: settings.ReasoningEffort,
			PlanMode: request.planMode,
			Model:    request.modelOverride, PersonalInstructions: c.personalInstructions,
			Goal: effective.Goal, ContextWindow: effective.ContextWindow,
			ReserveOutputTokens: effective.ReserveOutputTokens, AutoCompact: effective.AutoCompact,
			AutoCompactThreshold:   effective.AutoCompactThreshold,
			Execution:              agentExecutionOptions(mergeExecutionOptions(c.executionDefaults, request.executionOptions)),
			PermissionOverride:     request.permissionOverride,
			PlanAuthorization:      request.authorization,
			NoInteractiveDecisions: request.noInteractiveDecisions,
		}
	}
	if request.resume {
		if taskOptions.ExecutionID == "" {
			return taskRunSetup{}, errors.New("paused task checkpoint is missing its execution identity; discard it and start a new task")
		}
		if request.executionID != "" && request.executionID != taskOptions.ExecutionID {
			return taskRunSetup{}, errors.New("queued recovery execution id does not match the paused checkpoint")
		}
	} else {
		if request.executionID != "" {
			taskOptions.ExecutionID = request.executionID
		} else {
			taskOptions.ExecutionID, err = newID()
			if err != nil {
				return taskRunSetup{}, fmt.Errorf("create task execution ID: %w", err)
			}
		}
	}
	if !request.resume {
		instructions, instructionErr := c.initialProjectInstructions(ctx)
		if instructionErr != nil {
			return taskRunSetup{}, fmt.Errorf("load workspace AGENTS.md: %w", instructionErr)
		}
		taskOptions.ProjectInstructionsReady = true
		taskOptions.ProjectInstructions = instructions
	}
	var transcript *model.ResponseInput
	if len(saved.History) > 0 && string(saved.History) != "null" {
		var decoded model.ResponseInput
		if err := json.Unmarshal(saved.History, &decoded); err != nil {
			return taskRunSetup{}, fmt.Errorf("decode session history: %w", err)
		}
		transcript = &decoded
	}
	effectiveModel := profile.Model
	if taskOptions.Model != "" {
		effectiveModel = taskOptions.Model
	}
	contextKey := modelContextKey(profile.Provider, effectiveModel, providerConfig)
	modelContext := transcript
	if saved.ModelContextKey == contextKey && hasSessionHistory(saved.ModelContext) {
		var decoded model.ResponseInput
		if err := json.Unmarshal(saved.ModelContext, &decoded); err != nil {
			return taskRunSetup{}, fmt.Errorf("decode provider model context: %w", err)
		}
		modelContext = &decoded
	}
	return taskRunSetup{saved: saved, settings: settings, profile: profile, effectiveModel: effectiveModel, taskOptions: taskOptions, transcript: transcript, modelContext: modelContext, contextKey: contextKey}, nil
}
