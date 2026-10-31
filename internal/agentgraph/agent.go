// Package agentgraph runs the existing model and tool interfaces on luneGraph.
package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

const (
	modelNode          = "model_request"
	toolNode           = "execute_tool"
	teamModelNode      = "team_model_request"
	teamToolNode       = "team_execute_tool"
	teamJoinNode       = "team_join"
	teamBranchJoinNode = "team_branch_join"
	machineID          = "lune-agent-v4"
)

// Agent runs one model request or one tool call per graph step.
type Agent struct {
	runner      *graph.Runner[State]
	requester   Requester
	tools       model.ToolExecutor
	categorized model.CategorizedToolExecutor
	categories  []model.ToolCategory
	options     AgentOptions
}

var _ Requester = (*model.Client)(nil)

func New(requester Requester, tools model.ToolExecutor) (*Agent, error) {
	return NewWithOptions(requester, tools, AgentOptions{
		ID: "lead", Name: "Lead", Profile: model.DeepSeekFlashModel,
		Provider: model.ProviderDeepSeek, Model: model.DeepSeekFlashModel,
	})
}

func NewWithOptions(requester Requester, tools model.ToolExecutor, options AgentOptions) (*Agent, error) {
	if options.Execution.ModelMaxAttempts < 0 {
		return nil, errors.New("model maximum attempts must not be negative")
	}
	if options.Execution.MaxTeamChildrenPerAgent < 0 || options.Execution.MaxTeamDepth < 0 {
		return nil, errors.New("team delegation limits must not be negative")
	}
	if requester == nil {
		return nil, errors.New("requester is required")
	}
	if tools == nil {
		return nil, errors.New("tool executor is required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.ID == "" {
		options.ID = "lead"
	}
	if options.Name == "" {
		options.Name = options.ID
	}
	if options.Model == "" {
		options.Model = model.DeepSeekFlashModel
	}
	if options.Profile == "" {
		options.Profile = options.Model
	}

	agent := &Agent{requester: requester, tools: tools, options: options}
	if categorized, ok := tools.(model.CategorizedToolExecutor); ok {
		agent.categories = categorized.Categories()
		if len(agent.categories) > 0 {
			agent.categorized = categorized
		}
	}

	workflow := graph.New[State](modelNode)
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: modelNode, Run: agent.requestLead, OnError: graph.FailExecution, Timeout: options.Execution.ModelTimeout}); err != nil {
		return nil, fmt.Errorf("add model node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: toolNode, Run: agent.executeLeadTool, OnError: graph.FailExecution, Timeout: options.Execution.ToolTimeout}); err != nil {
		return nil, fmt.Errorf("add tool node: %w", err)
	}
	
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: teamModelNode, Run: agent.requestTeam, OnError: graph.FailInvocation, Timeout: options.Execution.ModelTimeout}); err != nil {
		return nil, fmt.Errorf("add team model node: %w", err)
	}
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: teamToolNode, Run: agent.executeTeamTool, OnError: graph.FailInvocation, Timeout: options.Execution.ToolTimeout}); err != nil {
		return nil, fmt.Errorf("add team tool node: %w", err)
	}
	if err := workflow.AddJoin(graph.JoinSpec[State]{Name: teamJoinNode, From: toolNode, Merge: agent.mergeTeam, OnError: graph.FailExecution, Timeout: options.Execution.JoinTimeout, CanMerge: agent.teamQuorumReached}); err != nil {
		return nil, fmt.Errorf("add team join: %w", err)
	}
	if err := workflow.AddJoin(graph.JoinSpec[State]{Name: teamBranchJoinNode, From: teamToolNode, Merge: agent.mergeTeam, OnError: graph.FailInvocation, Timeout: options.Execution.JoinTimeout, CanMerge: agent.teamQuorumReached}); err != nil {
		return nil, fmt.Errorf("add team branch join: %w", err)
	}
	if err := graph.RegisterJSONContinuation[State, []model.UserQuestionResponse](workflow, UserQuestionContinuation, func(_ context.Context, _ graph.CallInfo, state State, answer []model.UserQuestionResponse) (State, error) {
		state.UserAnswers, state.UserAnswerReady = answer, true
		return state, nil
	}); err != nil {
		return nil, fmt.Errorf("register user question continuation: %w", err)
	}
	if err := graph.RegisterJSONContinuation[State, string](workflow, ModelRecoveryContinuation, func(_ context.Context, _ graph.CallInfo, state State, _ string) (State, error) {
		if state.ModelFailure != nil && time.Now().Before(state.ModelFailure.RetryNotBefore) {
			return state, errors.New("model retry is not ready")
		}
		if options.ModelObserver != nil && state.ModelFailure != nil {
			options.ModelObserver(state, "resumed", *state.ModelFailure)
		}
		state.ModelFailure = nil
		return state, nil
	}); err != nil {
		return nil, err
	}
	if err := graph.RegisterJSONContinuation[State, string](workflow, ExecutionBlockerContinuation, func(_ context.Context, _ graph.CallInfo, state State, _ string) (State, error) {
		if IsCommandCompletionWait(state) {
			state.BlockerID, state.BlockerTool, state.BlockerReason = "", "", ""
			state.CommandFinalizeAttempts = 0
			return state, nil
		}
		return state, errors.New("an execution blocker requires an updated plan")
	}); err != nil {
		return nil, fmt.Errorf("register execution blocker continuation: %w", err)
	}
	if err := graph.RegisterJSONContinuation[State, string](workflow, TokenBudgetContinuation, func(_ context.Context, _ graph.CallInfo, state State, _ string) (State, error) {
		return state, nil
	}); err != nil {
		return nil, fmt.Errorf("register token budget continuation: %w", err)
	}
	if err := workflow.AddEdge(modelNode, modelNode); err != nil {
		return nil, fmt.Errorf("add model-to-model edge: %w", err)
	}
	if err := workflow.AddEdge(modelNode, toolNode); err != nil {
		return nil, fmt.Errorf("add model-to-tool edge: %w", err)
	}
	if err := workflow.AddEdge(toolNode, toolNode); err != nil {
		return nil, fmt.Errorf("add tool-to-tool edge: %w", err)
	}
	if err := workflow.AddEdge(toolNode, modelNode); err != nil {
		return nil, fmt.Errorf("add tool-to-model edge: %w", err)
	}
	for _, edge := range [][2]string{
		{toolNode, teamModelNode}, {teamModelNode, teamToolNode}, {teamToolNode, teamToolNode}, {teamToolNode, teamModelNode},
		{teamModelNode, teamModelNode}, {teamJoinNode, toolNode}, {teamBranchJoinNode, teamToolNode},
	} {
		if err := workflow.AddEdge(edge[0], edge[1]); err != nil {
			return nil, fmt.Errorf("add %s-to-%s edge: %w", edge[0], edge[1], err)
		}
	}
	runner, err := workflow.Compile(graph.Config[State]{MachineID: machineID, Clone: cloneState})
	if err != nil {
		return nil, fmt.Errorf("compile agent graph: %w", err)
	}
	agent.runner = runner
	return agent, nil
}

func (a *Agent) Start(ctx context.Context, runID string, history, input *model.ResponseInput, taskOptions TaskOptions, store graph.Store[State], options graph.Options) (*graph.Execution[State], graph.Result[State], error) {
	return a.StartWithTranscript(ctx, runID, history, history, input, taskOptions, store, options)
}

// StartWithTranscript starts an agent with a provider-specific request context
// and a portable conversation transcript kept separately for display and
// rebuilding context after a provider change.
func (a *Agent) StartWithTranscript(ctx context.Context, runID string, history, transcript, input *model.ResponseInput, taskOptions TaskOptions, store graph.Store[State], options graph.Options) (*graph.Execution[State], graph.Result[State], error) {
	if taskOptions.MaxToolRounds < 0 {
		return nil, graph.Result[State]{Status: graph.StatusFailed}, errors.New("MaxToolRounds must not be negative")
	}
	if taskOptions.Execution.MaxTeamChildrenPerAgent < 0 || taskOptions.Execution.MaxTeamDepth < 0 {
		return nil, graph.Result[State]{Status: graph.StatusFailed}, errors.New("team delegation limits must not be negative")
	}
	state, err := initialState(history, transcript, input)
	if err != nil {
		return nil, graph.Result[State]{Status: graph.StatusFailed}, err
	}
	state.SelectedCategories = append([]string(nil), taskOptions.Conversation.Categories...)
	state.HostContext = taskOptions.Conversation.HostContext
	state.LastRequest = taskOptions.Conversation.LastRequest
	state.ContextAtTaskStart = true
	inputItems, _ := responseInputItems(input)
	if taskOptions.AutomaticGoal {
		// Continuation inputs guide the model but are not user-authored messages.
		state.Transcript.Items = state.Transcript.Items[:len(state.Transcript.Items)-len(inputItems)]
	}
	state.ContextInsertIndex = len(state.History.Items) - len(inputItems)
	state.RunID = runID
	state.ExecutionID = taskOptions.ExecutionID
	if state.ExecutionID == "" {
		state.ExecutionID = runID
	}
	state.AgentID = a.options.ID
	state.AgentName = a.options.Name
	state.Provider = a.options.Provider
	state.Protocol = a.options.Protocol
	state.ServerCompaction = a.options.ServerCompaction
	state.SupportsInputAudio = a.options.SupportsInputAudio
	state.ModelProfile = a.options.Profile
	state.Model = a.options.Model
	state.ModelOverride = strings.TrimSpace(taskOptions.Model)
	state.ReasoningEffort = taskOptions.ReasoningEffort
	state.MaxToolRounds = taskOptions.MaxToolRounds
	state.TaskSettingsVersion = CurrentTaskSettingsVersion
	state.Goal = taskOptions.Goal
	state.GoalID, state.GoalRevision, state.AutomaticGoal = taskOptions.GoalID, taskOptions.GoalRevision, taskOptions.AutomaticGoal
	state.PersonalInstructions = taskOptions.PersonalInstructions
	state.ProjectInstructionsReady = taskOptions.ProjectInstructionsReady
	state.ProjectInstructions = cloneProjectInstructions(taskOptions.ProjectInstructions)
	state.ContextWindow = taskOptions.ContextWindow
	state.ReserveOutputTokens = taskOptions.ReserveOutputTokens
	state.AutoCompact = taskOptions.AutoCompact
	state.AutoCompactThreshold = taskOptions.AutoCompactThreshold
	state.LastInputTokens = taskOptions.LastInputTokens
	state.Execution = effectiveTeamExecution(a.options.Execution, taskOptions.Execution)
	state.PermissionOverride = taskOptions.PermissionOverride
	state.PlanMode = taskOptions.PlanMode
	state.PlanAuthorization = taskOptions.PlanAuthorization
	state.NoInteractiveDecisions = taskOptions.NoInteractiveDecisions
	if input != nil {
		state.TaskInput, err = json.Marshal(input)
		if err != nil {
			return nil, graph.Result[State]{Status: graph.StatusFailed}, fmt.Errorf("encode task input: %w", err)
		}
	}
	return a.runner.Start(ctx, runID, state, store, options)
}

func effectiveTeamExecution(defaults, options ExecutionOptions) ExecutionOptions {
	if options.MaxTeamChildrenPerAgent == 0 {
		options.MaxTeamChildrenPerAgent = defaults.MaxTeamChildrenPerAgent
	}
	if options.MaxTeamDepth == 0 {
		options.MaxTeamDepth = defaults.MaxTeamDepth
	}
	if options.MaxTeamChildrenPerAgent <= 0 {
		options.MaxTeamChildrenPerAgent = DefaultMaxTeamChildrenPerAgent
	}
	if options.MaxTeamDepth <= 0 {
		options.MaxTeamDepth = DefaultMaxTeamDepth
	}
	return options
}

func (a *Agent) Resume(ctx context.Context, checkpoint graph.Checkpoint[State], inputs []graph.ResumeInput, store graph.Store[State], options graph.Options) (*graph.Execution[State], graph.Result[State], error) {
	return a.runner.Resume(ctx, checkpoint, inputs, store, options)
}

func (a *Agent) Recover(ctx context.Context, runID string, inputs []graph.ResumeInput, store graph.Store[State], options graph.Options) (*graph.Execution[State], graph.Result[State], error) {
	return a.runner.Recover(ctx, runID, inputs, store, options)
}

func (a *Agent) Fork(ctx context.Context, newRunID string, source graph.Checkpoint[State]) (graph.Checkpoint[State], error) {
	return a.runner.Fork(ctx, newRunID, source, nil)
}

func RekeyForkState(state State, runID, executionID string) State {
	oldRunID, oldExecutionID := state.RunID, state.ExecutionID
	state.RunID, state.ExecutionID = runID, executionID
	state.TeamDispatchID = rekeyTeamRunID(state.TeamDispatchID, oldRunID, oldExecutionID, runID, executionID)
	state.TeamRunID = rekeyTeamRunID(state.TeamRunID, oldRunID, oldExecutionID, runID, executionID)
	return state
}

func rekeyTeamRunID(value, oldRunID, oldExecutionID, runID, executionID string) string {
	oldPrefix := oldRunID + ":" + oldExecutionID + ":"
	if strings.HasPrefix(value, oldPrefix) {
		return runID + ":" + executionID + ":" + strings.TrimPrefix(value, oldPrefix)
	}
	return value
}

func (a *Agent) MaxConcurrency() int {
	if a == nil || a.options.Team == nil {
		return 0
	}
	return a.options.Team.MaxParallelAgents()
}
