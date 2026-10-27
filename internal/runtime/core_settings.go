package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

type PermissionMode string

func agentExecutionOptions(options ExecutionOptions) agentgraph.ExecutionOptions {
	return agentgraph.ExecutionOptions{
		ModelMaxAttempts: options.ModelMaxAttempts, MaxSteps: options.MaxSteps, MaxInvocations: options.MaxInvocations, MaxConcurrency: options.MaxConcurrency,
		ModelTimeout: options.ModelTimeout, ToolTimeout: options.ToolTimeout, JoinTimeout: options.JoinTimeout,
		TeamEntryTimeout: options.TeamEntryTimeout, TeamQuorum: options.TeamQuorum,
		MaxTeamChildrenPerAgent: options.MaxTeamChildrenPerAgent, MaxTeamDepth: options.MaxTeamDepth, TokenBudget: options.TokenBudget,
	}
}

func coreExecutionOptions(options agentgraph.ExecutionOptions) ExecutionOptions {
	return ExecutionOptions{
		ModelMaxAttempts: options.ModelMaxAttempts, MaxSteps: options.MaxSteps, MaxInvocations: options.MaxInvocations, MaxConcurrency: options.MaxConcurrency,
		ModelTimeout: options.ModelTimeout, ToolTimeout: options.ToolTimeout, JoinTimeout: options.JoinTimeout,
		TeamEntryTimeout: options.TeamEntryTimeout, TeamQuorum: options.TeamQuorum,
		MaxTeamChildrenPerAgent: options.MaxTeamChildrenPerAgent, MaxTeamDepth: options.MaxTeamDepth, TokenBudget: options.TokenBudget,
	}
}

func (c *Core) Workspace() string {
	if c == nil {
		return ""
	}
	return c.workspace
}

// ListModelProfiles returns configured profiles in stable name order.
func (c *Core) ListModelProfiles() []ModelOption {
	if c == nil || c.models == nil {
		return nil
	}
	profiles := c.models.Profiles()
	options := make([]ModelOption, 0, len(profiles))
	for id, profile := range profiles {
		m := profile.Model
		if m == "" {
			m = profile.Model
		}
		options = append(options, ModelOption{
			Profile:       id,
			Provider:      profile.Provider,
			Model:         m,
			DisplayName:   profile.DisplayName,
			ContextWindow: profile.ContextWindow,
		})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Profile < options[j].Profile })
	return options
}

// DefaultModel returns the default configured model name or profile.
func (c *Core) DefaultModel() string {
	if c == nil || c.models == nil {
		return ""
	}
	defaultProfile := c.models.DefaultModel()
	if profile, ok := c.models.Profile(defaultProfile); ok && profile.Model != "" {
		return profile.Model
	}
	return defaultProfile
}

// ModelForSession returns the configured model name for the session,
// or the default model if none is explicitly configured on the session.
func (c *Core) ModelForSession(ctx context.Context, sessionID string) string {
	if c == nil || c.models == nil {
		return ""
	}
	profileName := ""
	if sessionID != "" && ctx != nil {
		if sess, err := c.GetSession(ctx, sessionID); err == nil && sess.Settings.ModelProfile != "" {
			profileName = sess.Settings.ModelProfile
		}
	}
	if profileName == "" {
		profileName = c.models.DefaultModel()
	}
	if profile, ok := c.models.Profile(profileName); ok && profile.Model != "" {
		return profile.Model
	}
	return profileName
}

// SetSessionSettings replaces the per-session overrides used by future tasks.
// A paused task continues with the settings saved in its checkpoint.
func (c *Core) SetSessionSettings(ctx context.Context, sessionID string, settings SessionSettings) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	settings, err := c.normalizeSessionSettings(settings)
	if err != nil {
		return err
	}
	return c.withSessionLock(sessionID, func() error {
		return c.setSessionSettingsOwned(ctx, sessionID, settings)
	})
}

func (c *Core) setSessionSettingsOwned(ctx context.Context, sessionID string, settings SessionSettings) error {
	// Usage writes and settings writes both replace the JSON session snapshot.
	// Serialize them so neither update can overwrite the other's fields.
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	saved.Settings = settings
	saved.UpdatedAt = time.Now()
	return c.saveSession(ctx, saved)
}

func (c *Core) ProjectContextDefaults() ContextDefaults {
	if c == nil {
		return ContextDefaults{}
	}
	return ContextDefaults{
		ContextWindow: c.taskDefaults.ContextWindow, ReserveOutputTokens: c.taskDefaults.ReserveOutputTokens,
		AutoCompact: c.taskDefaults.AutoCompact, AutoCompactThreshold: c.taskDefaults.AutoCompactThreshold,
	}
}

func (c *Core) ProjectAttachmentLimits() AttachmentLimits {
	limits := projectconfig.AttachmentLimits{}
	if c != nil {
		limits = c.attachmentLimits
	}
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = DefaultAttachmentMaxFiles
	}
	if limits.MaxFileBytes <= 0 {
		limits.MaxFileBytes = DefaultAttachmentMaxFileBytes
	}
	if limits.MaxTotalBytes <= 0 {
		limits.MaxTotalBytes = DefaultAttachmentMaxTotalBytes
	}
	return AttachmentLimits{
		MaxFiles: limits.MaxFiles, MaxFileBytes: limits.MaxFileBytes, MaxTotalBytes: limits.MaxTotalBytes,
	}
}

func (c *Core) normalizeSessionModelSettings(settings SessionSettings) (SessionSettings, error) {
	if settings.ModelProfile == "" {
		settings.ModelProfile = c.models.DefaultModel()
	}
	if _, ok := c.models.Profile(settings.ModelProfile); !ok {
		return SessionSettings{}, fmt.Errorf("unknown model profile %q", settings.ModelProfile)
	}
	effort := strings.TrimSpace(settings.ReasoningEffort)
	switch effort {
	case "", model.ReasoningEffortNone, model.ReasoningEffortMinimal, model.ReasoningEffortLow,
		model.ReasoningEffortMedium, model.ReasoningEffortHigh, model.ReasoningEffortXHigh, model.ReasoningEffortMax:
	default:
		return SessionSettings{}, fmt.Errorf("unsupported reasoning effort %q", settings.ReasoningEffort)
	}
	settings.ReasoningEffort = effort
	return settings, nil
}

func (c *Core) normalizeSessionSettings(settings SessionSettings) (SessionSettings, error) {
	settings, err := c.normalizeSessionModelSettings(settings)
	if err != nil {
		return SessionSettings{}, err
	}
	if settings.ContextWindow != nil && *settings.ContextWindow < 0 {
		return SessionSettings{}, errors.New("contextWindow override must not be negative")
	}
	if settings.ReserveOutputTokens != nil && *settings.ReserveOutputTokens < 0 {
		return SessionSettings{}, errors.New("reserveOutputTokens override must not be negative")
	}
	if settings.AutoCompactThreshold != nil && (math.IsNaN(*settings.AutoCompactThreshold) || math.IsInf(*settings.AutoCompactThreshold, 0) ||
		*settings.AutoCompactThreshold <= 0 || *settings.AutoCompactThreshold > 1) {
		return SessionSettings{}, errors.New("autoCompactThreshold override must be greater than 0 and at most 1")
	}
	if _, err := c.resolveTaskSettings(settings); err != nil {
		return SessionSettings{}, err
	}
	return settings, nil
}

func (c *Core) resolveTaskSettings(settings SessionSettings) (taskSettings, error) {
	effective := c.taskDefaults
	if settings.ContextWindow != nil {
		effective.ContextWindow = *settings.ContextWindow
	}
	if settings.ReserveOutputTokens != nil {
		effective.ReserveOutputTokens = *settings.ReserveOutputTokens
	}
	if settings.AutoCompact != nil {
		effective.AutoCompact = *settings.AutoCompact
	}
	if settings.AutoCompactThreshold != nil {
		effective.AutoCompactThreshold = *settings.AutoCompactThreshold
	}
	if effective.AutoCompact && effective.AutoCompactThreshold == 0 {
		effective.AutoCompactThreshold = projectconfig.DefaultAutoCompactThreshold
	}
	if effective.ContextWindow < 0 {
		return taskSettings{}, errors.New("contextWindow must not be negative")
	}
	if effective.ReserveOutputTokens < 0 {
		return taskSettings{}, errors.New("reserveOutputTokens must not be negative")
	}
	if effective.ContextWindow > 0 && effective.ReserveOutputTokens >= effective.ContextWindow {
		return taskSettings{}, errors.New("reserveOutputTokens must be smaller than contextWindow")
	}
	if math.IsNaN(effective.AutoCompactThreshold) || math.IsInf(effective.AutoCompactThreshold, 0) ||
		effective.AutoCompactThreshold < 0 || effective.AutoCompactThreshold > 1 {
		return taskSettings{}, errors.New("autoCompactThreshold must be between 0 and 1")
	}
	return effective, nil
}

func taskOptionsFromState(state agentgraph.State) agentgraph.TaskOptions {
	return agentgraph.TaskOptions{
		Conversation:  state.ConversationState(),
		ExecutionID:   state.ExecutionID,
		MaxToolRounds: state.MaxToolRounds, Model: state.ModelOverride, ReasoningEffort: state.ReasoningEffort,
		Goal: state.Goal, PersonalInstructions: state.PersonalInstructions,
		GoalID: state.GoalID, GoalRevision: state.GoalRevision, AutomaticGoal: state.AutomaticGoal,
		ProjectInstructionsReady: state.ProjectInstructionsReady,
		ProjectInstructions:      append([]agentgraph.ProjectInstruction(nil), state.ProjectInstructions...),
		ContextWindow:            state.ContextWindow, ReserveOutputTokens: state.ReserveOutputTokens,
		AutoCompact: state.AutoCompact, AutoCompactThreshold: state.AutoCompactThreshold,
		LastInputTokens:    state.LastInputTokens,
		Execution:          state.Execution,
		PermissionOverride: state.PermissionOverride,
		PlanMode:           state.PlanMode,
		PlanAuthorization:  state.PlanAuthorization, NoInteractiveDecisions: state.NoInteractiveDecisions,
	}
}

func validateExecutionOptions(options ExecutionOptions) error {
	if options.ModelMaxAttempts < 0 || options.MaxSteps < 0 || options.MaxInvocations < 0 || options.MaxConcurrency < 0 || options.TeamQuorum < 0 || options.MaxTeamChildrenPerAgent < 0 || options.MaxTeamDepth < 0 || options.TokenBudget < 0 {
		return errors.New("execution limits must not be negative")
	}
	if options.ModelTimeout < 0 || options.ToolTimeout < 0 || options.JoinTimeout < 0 || options.TeamEntryTimeout < 0 {
		return errors.New("execution timeouts must not be negative")
	}
	return nil
}

func validatePermissionOverride(mode string) error {
	if mode == "" || PermissionMode(mode) == PermissionReadOnly || PermissionMode(mode) == PermissionWorkspace || PermissionMode(mode) == PermissionAuto || PermissionMode(mode) == PermissionAsk || PermissionMode(mode) == PermissionFullAccess {
		return nil
	}
	return fmt.Errorf("unsupported task permission override %q", mode)
}

func mergeExecutionOptions(defaults, overrides ExecutionOptions) ExecutionOptions {
	if overrides.ModelMaxAttempts != 0 {
		defaults.ModelMaxAttempts = overrides.ModelMaxAttempts
	}
	if overrides.MaxSteps != 0 {
		defaults.MaxSteps = overrides.MaxSteps
	}
	if overrides.MaxInvocations != 0 {
		defaults.MaxInvocations = overrides.MaxInvocations
	}
	if overrides.MaxConcurrency != 0 {
		defaults.MaxConcurrency = overrides.MaxConcurrency
	}
	if overrides.ModelTimeout != 0 {
		defaults.ModelTimeout = overrides.ModelTimeout
	}
	if overrides.ToolTimeout != 0 {
		defaults.ToolTimeout = overrides.ToolTimeout
	}
	if overrides.JoinTimeout != 0 {
		defaults.JoinTimeout = overrides.JoinTimeout
	}
	if overrides.TeamEntryTimeout != 0 {
		defaults.TeamEntryTimeout = overrides.TeamEntryTimeout
	}
	if overrides.TeamQuorum != 0 {
		defaults.TeamQuorum = overrides.TeamQuorum
	}
	if overrides.MaxTeamChildrenPerAgent != 0 {
		defaults.MaxTeamChildrenPerAgent = overrides.MaxTeamChildrenPerAgent
	}
	if overrides.MaxTeamDepth != 0 {
		defaults.MaxTeamDepth = overrides.MaxTeamDepth
	}
	if overrides.TokenBudget != 0 {
		defaults.TokenBudget = overrides.TokenBudget
	}
	return defaults
}

func mergeTeamExecutionDefaults(defaults, options ExecutionOptions) ExecutionOptions {
	if options.MaxTeamChildrenPerAgent == 0 {
		options.MaxTeamChildrenPerAgent = defaults.MaxTeamChildrenPerAgent
	}
	if options.MaxTeamDepth == 0 {
		options.MaxTeamDepth = defaults.MaxTeamDepth
	}
	return options
}

func checkpointSettingsVersionError(version int) error {
	return fmt.Errorf("cannot resume task checkpoint with task settings version %d (current version %d); discard it and start a new task", version, agentgraph.CurrentTaskSettingsVersion)
}

func (c *Core) PermissionMode() PermissionMode {
	if c == nil {
		return ""
	}
	return PermissionMode(c.registry.PermissionMode())
}

func (c *Core) SetPermissionMode(mode PermissionMode) error {
	if c == nil {
		return errors.New("core is nil")
	}
	return c.registry.SetPermissionMode(tool.PermissionMode(mode))
}
