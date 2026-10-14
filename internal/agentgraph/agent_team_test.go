package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

func TestAgentDispatchesTeamProfilesAndSynthesizesPartialResults(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	team := &scriptedTeamExecutor{
		limit: 3,
		profiles: map[string]model.ModelProfile{
			"lead":     {Provider: model.ProviderDeepSeek, Model: "deepseek-test"},
			"research": {Provider: model.ProviderAnthropic, Model: "claude-test", MaxOutputTokens: 1024},
		},
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"team-1","name":"delegate_team","arguments":"{\"tasks\":[{\"id\":\"research\",\"model\":\"research\",\"task\":\"inspect input handling\"},{\"id\":\"review\",\"model\":\"lead\",\"task\":\"review permission boundaries\"}]}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Combined result."}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderDeepSeek,
		Model: "deepseek-test", Team: team,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	input, err := model.NewResponseInput("inspect the project")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "team-agent-run", nil, input, TaskOptions{
		MaxToolRounds: 7, ReasoningEffort: model.ReasoningEffortHigh,
		Goal: "parent goal", PersonalInstructions: "personal preferences",
		ContextWindow: 64000, ReserveOutputTokens: 2048,
		AutoCompact: true, AutoCompactThreshold: 0.75,
	}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v", result)
	}
	if len(team.assignments) != 2 || team.assignments[0].Model != "research" || team.assignments[1].Model != "lead" {
		t.Fatalf("team assignments = %#v", team.assignments)
	}
	if len(team.options) != 2 || team.options[0].MaxToolRounds != 7 || team.options[0].ReasoningEffort != model.ReasoningEffortHigh {
		t.Fatalf("worker options = %#v", team.options)
	}
	for _, options := range team.options {
		if options.Goal != "" || options.ContextWindow != 0 || options.ReserveOutputTokens != 0 || options.AutoCompact || options.AutoCompactThreshold != 0 {
			t.Fatalf("new task settings unexpectedly inherited by team member: %#v", options)
		}
		if options.PersonalInstructions != "personal preferences" {
			t.Fatalf("personal instructions not inherited by team member: %#v", options)
		}
	}
	for _, instructions := range team.branchInstructions {
		if instructions != "personal preferences" {
			t.Fatalf("branch state personal instructions = %q", instructions)
		}
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want dispatch plus synthesis", len(requester.requests))
	}
	if requester.requests[0].Reasoning == nil || requester.requests[0].Reasoning.Effort != model.ReasoningEffortHigh {
		t.Fatalf("main request reasoning = %#v, want high effort", requester.requests[0].Reasoning)
	}
	var teamTool bool
	for _, definition := range requester.requests[0].Tools {
		if function, ok := definition.(model.FunctionTool); ok && function.Name == teamDispatchToolName {
			teamTool = true
			if !strings.Contains(string(function.Parameters), `"enum":["lead","research"]`) {
				t.Errorf("team tool profile choices = %s", function.Parameters)
			}
		}
	}
	if !teamTool {
		t.Fatal("lead request does not include delegate_team")
	}
	if !strings.Contains(teamResponseHistoryText(requester.requests[1].Input), "Found the relevant behavior") ||
		!strings.Contains(teamResponseHistoryText(requester.requests[1].Input), "review provider unavailable") {
		t.Fatalf("synthesis history lacks successful and failed member results: %#v", requester.requests[1].Input)
	}
}

func TestAgentTeamQuorumMergesEarlyAndMarksPendingMembers(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	team := &quorumTeamExecutor{started: make(chan struct{}, 1), profiles: map[string]model.ModelProfile{
		"worker": {Provider: model.ProviderDeepSeek, Model: "worker-model"},
	}}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"team-1","name":"delegate_team","arguments":"{\"tasks\":[{\"id\":\"fast\",\"model\":\"worker\",\"task\":\"finish quickly\"},{\"id\":\"slow\",\"model\":\"worker\",\"task\":\"wait for cancellation\"}],\"minimum_successes\":null}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthesized"}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderDeepSeek,
		Model: "lead-model", Team: team, Execution: ExecutionOptions{TeamQuorum: 1},
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("delegate two independent tasks")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, result, err := agent.Start(ctx, "team-quorum-run", nil, input, TaskOptions{Execution: ExecutionOptions{TeamQuorum: 1}}, store, graph.Options{MaxConcurrency: 3})
	if err != nil || (result.Status != graph.StatusCompleted && result.Status != graph.StatusCompletedWithFailures) || result.Final == nil {
		t.Fatalf("quorum result = status %q final=%t error=%v", result.Status, result.Final != nil, err)
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want dispatch and synthesis", len(requester.requests))
	}
	if history := teamResponseHistoryText(requester.requests[1].Input); !strings.Contains(history, "fast member finished") || !strings.Contains(history, "cancelled after the minimum success threshold was reached") {
		t.Fatalf("synthesis input does not show the completed and cancelled members: %s", history)
	}
}

func TestAgentDelegationCanOverrideHostQuorumToWaitForAll(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	team := &quorumTeamExecutor{
		profiles: map[string]model.ModelProfile{"worker": {Provider: model.ProviderDeepSeek, Model: "worker-model"}},
		started:  make(chan struct{}, 1), release: make(chan struct{}),
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"team-1","name":"delegate_team","arguments":"{\"tasks\":[{\"id\":\"fast\",\"model\":\"worker\",\"task\":\"finish quickly\"},{\"id\":\"slow\",\"model\":\"worker\",\"task\":\"finish after release\"}],\"minimum_successes\":0}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthesized"}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderDeepSeek,
		Model: "lead-model", Team: team, Execution: ExecutionOptions{TeamQuorum: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("wait for all team members")
	if err != nil {
		t.Fatal(err)
	}
	type runResult struct {
		result graph.Result[State]
		err    error
	}
	done := make(chan runResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_, result, err := agent.Start(ctx, "team-wait-all-run", nil, input, TaskOptions{Execution: ExecutionOptions{TeamQuorum: 1}}, store, graph.Options{MaxConcurrency: 3})
		done <- runResult{result: result, err: err}
	}()
	select {
	case <-team.started:
	case result := <-done:
		t.Fatalf("run ended before slow member started: status=%q error=%v", result.result.Status, result.err)
	case <-ctx.Done():
		t.Fatal("slow member did not start")
	}
	team.release <- struct{}{}
	select {
	case result := <-done:
		if result.err != nil || result.result.Final == nil || result.result.Status != graph.StatusCompleted {
			t.Fatalf("wait-all result = status %q error=%v", result.result.Status, result.err)
		}
	case <-ctx.Done():
		t.Fatal("wait-all run did not finish after releasing the slow member")
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want dispatch and synthesis", len(requester.requests))
	}
	if history := teamResponseHistoryText(requester.requests[1].Input); !strings.Contains(history, "slow member finished") || strings.Contains(history, "cancelled after the minimum success threshold was reached") {
		t.Fatalf("per-dispatch zero did not wait for all results: %s", history)
	}
}

func TestParseTeamAssignmentsAcceptsPerDispatchQuorum(t *testing.T) {
	team := &scriptedTeamExecutor{limit: 2, profiles: map[string]model.ModelProfile{
		"worker": {Provider: model.ProviderDeepSeek, Model: "worker-model"},
	}}
	assignments := `"tasks":[{"id":"one","model":"worker","task":"work"}]`
	_, inherited, err := parseTeamAssignments("{"+assignments+"}", team)
	if err != nil || inherited != nil {
		t.Fatalf("inherited quorum = %v, error=%v", inherited, err)
	}
	_, waitAll, err := parseTeamAssignments("{"+assignments+`,"minimum_successes":0}`, team)
	if err != nil || waitAll == nil || *waitAll != 0 {
		t.Fatalf("zero quorum override = %v, error=%v", waitAll, err)
	}
	if _, _, err := parseTeamAssignments("{"+assignments+`,"minimum_successes":2}`, team); err == nil {
		t.Fatal("quorum larger than assignment count should be rejected")
	}
}

func TestAgentGraphJoinsNestedTeamBranches(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]model.ModelProfile{
		"worker": {Provider: model.ProviderDeepSeek, Model: "worker-model"},
	}
	team := &nestedTeamExecutor{profiles: profiles, requesters: map[string]Requester{
		"lead/outer-a": &scriptedRequester{responses: []*model.Response{
			mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"nested-team","name":"delegate_team","arguments":"{\"tasks\":[{\"id\":\"inner\",\"model\":\"worker\",\"task\":\"inspect a focused detail\"}]}"}]}`),
			mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"outer-a completed"}]}]}`),
		}},
		"lead/outer-b": &scriptedRequester{responses: []*model.Response{
			mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"outer-b completed"}]}]}`),
		}},
		"lead/outer-a/inner": &scriptedRequester{responses: []*model.Response{
			mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"inner completed"}]}]}`),
		}},
	}}
	lead := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"outer-team","name":"delegate_team","arguments":"{\"tasks\":[{\"id\":\"outer-a\",\"model\":\"worker\",\"task\":\"perform a nested workflow\"},{\"id\":\"outer-b\",\"model\":\"worker\",\"task\":\"perform independent work\"}]}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final synthesis"}]}]}`),
	}}
	agent, err := NewWithOptions(lead, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderDeepSeek, Model: "lead-model", Team: team,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("root request must stay out of worker history")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "nested-team-run", nil, input, TaskOptions{}, store, graph.Options{MaxConcurrency: 3})
	if err != nil || result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("nested team run = status %q, error %v", result.Status, err)
	}
	encodedHistory, err := json.Marshal(result.Final.History)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"outer-a completed", "outer-b completed", "Team results:"} {
		if !strings.Contains(string(encodedHistory), expected) {
			t.Errorf("nested team history is missing %q: %s", expected, encodedHistory)
		}
	}
	outerA := team.requesters["lead/outer-a"].(*scriptedRequester)
	workerInput, err := json.Marshal(outerA.requests[0].Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workerInput), "perform a nested workflow") || strings.Contains(string(workerInput), "root request must stay out of worker history") {
		t.Fatalf("team branch did not have isolated input history: %s", workerInput)
	}
	outerAResume, err := json.Marshal(outerA.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(outerAResume), "inner completed") {
		t.Fatalf("nested result did not return to its parent branch: %s", outerAResume)
	}
}

type scriptedTeamExecutor struct {
	profiles           map[string]model.ModelProfile
	limit              int
	assignments        []TeamAssignment
	options            []TaskOptions
	branchInstructions []string
}

type scriptedBranchRequester struct {
	text string
	err  error
}

func (r scriptedBranchRequester) CreateResponse(context.Context, model.ModelRequest) (*model.ModelResponse, error) {
	if r.err != nil {
		return nil, r.err
	}
	return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{
		model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: r.text}}},
	}}, nil
}

func (t *scriptedTeamExecutor) Profiles() map[string]model.ModelProfile { return t.profiles }

func (t *scriptedTeamExecutor) MaxParallelAgents() int { return t.limit }

func (t *scriptedTeamExecutor) PrepareBranch(_ context.Context, state State, assignment TeamAssignment, options TaskOptions) (State, error) {
	t.assignments = append(t.assignments, assignment)
	t.options = append(t.options, options)
	t.branchInstructions = append(t.branchInstructions, state.PersonalInstructions)
	profile := t.profiles[assignment.Model]
	state.ModelProfile, state.Model, state.Provider = assignment.Model, profile.Model, profile.Provider
	state.Protocol, state.TeamModel, state.TeamProvider = model.ProtocolResponses, profile.Model, profile.Provider
	return state, nil
}

func (t *scriptedTeamExecutor) Requester(_ context.Context, state State) (Requester, error) {
	if state.TeamTaskID == "research" {
		return scriptedBranchRequester{text: "Found the relevant behavior."}, nil
	}
	return scriptedBranchRequester{err: errors.New("review provider unavailable")}, nil
}

type quorumTeamExecutor struct {
	profiles map[string]model.ModelProfile
	started  chan struct{}
	release  chan struct{}
}

func (t *quorumTeamExecutor) Profiles() map[string]model.ModelProfile { return t.profiles }

func (t *quorumTeamExecutor) MaxParallelAgents() int { return 2 }

func (t *quorumTeamExecutor) PrepareBranch(_ context.Context, state State, assignment TeamAssignment, _ TaskOptions) (State, error) {
	profile := t.profiles[assignment.Model]
	state.ModelProfile, state.Model, state.Provider = assignment.Model, profile.Model, profile.Provider
	state.TeamModel, state.TeamProvider = profile.Model, profile.Provider
	state.Protocol = model.ProtocolResponses
	return state, nil
}

func (t *quorumTeamExecutor) Requester(_ context.Context, state State) (Requester, error) {
	if state.TeamTaskID == "fast" {
		return scriptedBranchRequester{text: "fast member finished"}, nil
	}
	return blockingBranchRequester{started: t.started, release: t.release}, nil
}

type blockingBranchRequester struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (r blockingBranchRequester) CreateResponse(ctx context.Context, _ model.ModelRequest) (*model.ModelResponse, error) {
	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	if r.release == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-r.release:
		return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{
			model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "slow member finished"}}},
		}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func teamResponseHistoryText(input *model.ResponseInput) string {
	encoded, _ := json.Marshal(input)
	return string(encoded)
}

type nestedTeamExecutor struct {
	profiles   map[string]model.ModelProfile
	requesters map[string]Requester
}

func (t *nestedTeamExecutor) Profiles() map[string]model.ModelProfile { return t.profiles }

func (t *nestedTeamExecutor) MaxParallelAgents() int { return 3 }

func (t *nestedTeamExecutor) PrepareBranch(_ context.Context, state State, assignment TeamAssignment, _ TaskOptions) (State, error) {
	profile := t.profiles[assignment.Model]
	state.ModelProfile, state.Provider, state.Model = assignment.Model, profile.Provider, profile.Model
	state.Protocol = model.ProtocolResponses
	state.TeamModel, state.TeamProvider = profile.Model, profile.Provider
	return state, nil
}

func (t *nestedTeamExecutor) Requester(_ context.Context, state State) (Requester, error) {
	requester := t.requesters[state.AgentID]
	if requester == nil {
		return nil, fmt.Errorf("no requester for team branch %q", state.AgentID)
	}
	return requester, nil
}
