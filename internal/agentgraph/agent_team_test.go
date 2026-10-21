package agentgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
		Model: "deepseek-test", Team: team, Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 2},
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
			if !strings.Contains(string(function.Parameters), `"enum":["task","fork"]`) || !strings.Contains(string(function.Parameters), `"enum":["lead","research"]`) {
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
		Model: "lead-model", Team: team, Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 2, TeamQuorum: 1},
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
		Model: "lead-model", Team: team, Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 2, TeamQuorum: 1},
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
	_, mode, inherited, err := parseTeamAssignments("{"+assignments+"}", team, 2)
	if err != nil || mode != TeamDispatchTask || inherited != nil {
		t.Fatalf("default dispatch mode/quorum = %q/%v, error=%v", mode, inherited, err)
	}
	_, mode, waitAll, err := parseTeamAssignments("{"+assignments+`,"mode":"fork","minimum_successes":0}`, team, 2)
	if err != nil || mode != TeamDispatchFork || waitAll == nil || *waitAll != 0 {
		t.Fatalf("fork mode and zero quorum override = %q/%v, error=%v", mode, waitAll, err)
	}
	if _, _, _, err := parseTeamAssignments("{"+assignments+`,"minimum_successes":2}`, team, 2); err == nil {
		t.Fatal("quorum larger than assignment count should be rejected")
	}
	if _, _, _, err := parseTeamAssignments("{"+assignments+"}", team, 0); err == nil {
		t.Fatal("delegation with no remaining quota should be rejected")
	}
	for _, mode := range []string{`"unknown"`, `""`, `null`} {
		if _, _, _, err := parseTeamAssignments("{"+assignments+`,"mode":`+mode+`}`, team, 2); err == nil {
			t.Errorf("invalid dispatch mode %s should be rejected", mode)
		}
	}
}

func TestForkDispatchCopiesPreDispatchHistoryAndScopesCacheByModel(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	team := &forkRecordingTeamExecutor{
		profiles: map[string]model.ModelProfile{
			"same-a": {Provider: model.ProviderOpenAI, Model: "shared-model"},
			"same-b": {Provider: model.ProviderOpenAI, Model: "shared-model"},
			"other":  {Provider: model.ProviderAnthropic, Model: "different-model"},
		},
		requests: make(map[string]model.ModelRequest),
		states:   make(map[string]State),
	}
	requester := &scriptedRequester{responses: []*model.Response{
		teamDispatchResponseForAssignments("dispatch-call", TeamDispatchFork, []TeamAssignment{
			{ID: "fork-0", Model: "same-a", Task: "task zero"},
			{ID: "fork-1", Model: "same-b", Task: "task one"},
			{ID: "fork-2", Model: "other", Task: "task two"},
		}),
		teamTextResponse("synthesized fork results"),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderOpenAI,
		Protocol: model.ProtocolResponses, Model: "shared-model", Team: team, Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("initial user request")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText("prior assistant context")},
		model.InputFunctionCall{CallID: "prior-call", Name: "read_file", Arguments: `{"path":"a.go"}`},
		model.InputFunctionCallOutput{CallID: "prior-call", Output: model.NewToolOutputText("previous tool result")},
	)
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "fork-context-run", nil, input, TaskOptions{}, store, graph.Options{MaxConcurrency: 4})
	if err != nil || result.Status != graph.StatusCompleted {
		t.Fatalf("fork run status=%q error=%v", result.Status, err)
	}
	if len(requester.requests) < 2 || requester.requests[0].Input == nil {
		t.Fatalf("lead model requests = %d, want dispatch and synthesis", len(requester.requests))
	}
	parentCache := requester.requests[0].PromptCache
	if parentCache == nil {
		t.Fatal("delegatable parent request did not receive a cache hint")
	}
	prefixCount := len(requester.requests[0].Input.Items)
	team.mu.Lock()
	branchRequests := make(map[string]model.ModelRequest, len(team.requests))
	branchStates := make(map[string]State, len(team.states))
	for id, request := range team.requests {
		branchRequests[id] = request
	}
	for id, state := range team.states {
		branchStates[id] = state
	}
	team.mu.Unlock()
	if len(branchRequests) != 3 {
		t.Fatalf("fork branch requests = %d, want 3", len(branchRequests))
	}
	for _, id := range []string{"lead/fork-0", "lead/fork-1"} {
		request := branchRequests[id]
		inputCount := -1
		if request.Input != nil {
			inputCount = len(request.Input.Items)
		}
		if inputCount != prefixCount+2 {
			t.Errorf("branch %q input item count = %d, want assignment plus updated worker context (%d)", id, inputCount, prefixCount+2)
			continue
		}
		parentPrefix, _ := json.Marshal(requester.requests[0].Input)
		childPrefix, _ := json.Marshal(model.NewResponseInputItems(request.Input.Items[:prefixCount]...))
		if string(parentPrefix) != string(childPrefix) {
			t.Fatal("worker context update rewrote shared fork prefix")
		}
		encoded, err := json.Marshal(request.Input)
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range []string{"initial user request", "prior assistant context", "previous tool result"} {
			if !strings.Contains(string(encoded), expected) {
				t.Errorf("branch %q fork input is missing %q: %s", id, expected, encoded)
			}
		}
		for _, excluded := range []string{"dispatch-call", "delegate_team"} {
			if strings.Contains(string(encoded), excluded) {
				t.Errorf("branch %q copied the pending parent call %q: %s", id, excluded, encoded)
			}
		}
		if request.PromptCache == nil || request.PromptCache.Key != parentCache.Key || request.PromptCache.PrefixItems != prefixCount {
			t.Errorf("branch %q prompt cache hint = %#v, want parent's key and prefix count %d", id, request.PromptCache, prefixCount)
		}
		encodedState, err := json.Marshal(branchStates[id])
		if err != nil {
			t.Fatal(err)
		}
		var restored State
		if err := json.Unmarshal(encodedState, &restored); err != nil {
			t.Fatal(err)
		}
		if !restored.TeamForked || restored.TeamForkPrefixItems != prefixCount || restored.TeamDispatchMode != TeamDispatchFork || restored.TeamForkSourceCache.Key != parentCache.Key || restored.TaskSettingsVersion != CurrentTaskSettingsVersion {
			t.Errorf("fork checkpoint state = %#v", restored)
		}
	}
	differentRequest := branchRequests["lead/fork-2"]
	differentInput, err := json.Marshal(differentRequest.Input)
	if err != nil {
		t.Fatal(err)
	}
	if differentRequest.Input == nil || strings.Contains(string(differentInput), "initial user request") || !strings.Contains(string(differentInput), "task two") {
		t.Fatalf("different-model member inherited parent history instead of receiving a fresh task: %#v", differentRequest.Input)
	}
	if differentRequest.PromptCache == nil || differentRequest.PromptCache.Key == parentCache.Key {
		t.Errorf("different-model member should have an isolated cache key, got %#v (parent %#v)", differentRequest.PromptCache, parentCache)
	}
	differentState := branchStates["lead/fork-2"]
	if differentState.TeamForked || differentState.TeamDispatchMode != TeamDispatchTask || differentState.TeamForkPrefixItems != 0 {
		t.Errorf("different-model member should use fresh task context: %#v", differentState)
	}
	synthesizedInput, err := json.Marshal(requester.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Context: forked", "Context: task-only"} {
		if !strings.Contains(string(synthesizedInput), expected) {
			t.Errorf("parent synthesis input is missing context mode %q: %s", expected, synthesizedInput)
		}
	}
	first, second := branchRequests["lead/fork-0"].PromptCache, branchRequests["lead/fork-1"].PromptCache
	if first != nil && second != nil && first.Key != second.Key {
		t.Error("same-provider, same-model fork siblings should use the parent's cache key")
	}
}

func TestAgentTeamDelegationIsDisabledByDefault(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	team := &treeTeamExecutor{
		profiles: map[string]model.ModelProfile{"worker": {Provider: model.ProviderDeepSeek, Model: "worker-model"}},
		calls:    make(map[string]int),
		requests: make(map[string][]model.ModelRequest),
	}
	root := &scriptedRequester{responses: []*model.Response{
		teamDispatchResponse("default-disabled", "child", 1),
		teamTextResponse("completed without delegating"),
	}}
	agent, err := NewWithOptions(root, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderDeepSeek,
		Model: "lead-model", Team: team,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("delegation is disabled without a positive limit")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "default-team-disabled", nil, input, TaskOptions{}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("default team status=%q error=%v", result.Status, err)
	}
	if hasTeamTool(root.requests[0].Tools) || len(team.prepared) != 0 || result.Final.TeamChildrenCreated != 0 || result.Final.Execution.MaxTeamChildrenPerAgent != 0 {
		t.Fatalf("delegation was not disabled by default: requests=%#v prepared=%d state=%#v", root.requests[0].Tools, len(team.prepared), result.Final)
	}
}

func TestAgentTeamTreeUsesConfiguredChildAndDepthLimits(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	team := &treeTeamExecutor{
		profiles: map[string]model.ModelProfile{"worker": {Provider: model.ProviderDeepSeek, Model: "worker-model"}},
		calls:    make(map[string]int),
		requests: make(map[string][]model.ModelRequest),
	}
	root := &scriptedRequester{responses: []*model.Response{
		teamDispatchResponse("root-dispatch", "outer", 4),
		teamTextResponse("root synthesized all results"),
	}}
	agent, err := NewWithOptions(root, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderDeepSeek,
		Model: "lead-model", Team: team, Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 4, MaxTeamDepth: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("delegate four subtasks with one recursive level")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "default-team-tree", nil, input, TaskOptions{}, store, graph.Options{MaxConcurrency: 4})
	if err != nil || result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("team tree = status %q, error %v", result.Status, err)
	}
	if result.Final.TeamDepth != 0 || result.Final.TeamChildrenCreated != 4 {
		t.Fatalf("root quota state = depth %d, children %d; want 0 and 4", result.Final.TeamDepth, result.Final.TeamChildrenCreated)
	}
	team.mu.Lock()
	prepared := append([]State(nil), team.prepared...)
	requests := make(map[string][]model.ModelRequest, len(team.requests))
	for id, values := range team.requests {
		requests[id] = append([]model.ModelRequest(nil), values...)
	}
	team.mu.Unlock()
	if len(prepared) != 20 {
		t.Fatalf("prepared branches = %d, want 20 (4 children plus 16 grandchildren)", len(prepared))
	}
	depthCounts := map[int]int{}
	for _, state := range prepared {
		depthCounts[state.TeamDepth]++
		if state.TeamDepth == 1 && state.TeamChildrenCreated != 0 {
			t.Errorf("new depth-one branch inherited child count %d", state.TeamChildrenCreated)
		}
	}
	if depthCounts[1] != 4 || depthCounts[2] != 16 {
		t.Fatalf("branch depths = %#v, want 4 at depth one and 16 at depth two", depthCounts)
	}
	if got := teamToolMaxItems(t, root.requests[0].Tools); got != 0 {
		t.Errorf("root delegate_team maxItems = %d, want absent", got)
	}
	for id, agentRequests := range requests {
		if len(agentRequests) == 0 {
			t.Errorf("branch %q made no model request", id)
			continue
		}
		depth := strings.Count(id, "/")
		if depth == 1 {
			if got := teamToolMaxItems(t, agentRequests[0].Tools); got != 0 {
				t.Errorf("depth-one agent %q maxItems = %d, want absent", id, got)
			}
		} else if hasTeamTool(agentRequests[0].Tools) {
			t.Errorf("depth-two agent %q still sees delegate_team", id)
		}
	}
	encoded, err := json.Marshal(result.Final)
	if err != nil {
		t.Fatal(err)
	}
	var restored State
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.TeamDepth != 0 || restored.TeamChildrenCreated != 4 || restored.Execution.MaxTeamDepth != 2 || restored.Execution.MaxTeamChildrenPerAgent != 4 {
		t.Fatalf("team checkpoint fields did not round-trip: %#v", restored)
	}
}

func TestAgentTeamChildQuotaIsCumulativeAcrossDispatches(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	team := &scriptedTeamExecutor{limit: 2, profiles: map[string]model.ModelProfile{
		"worker": {Provider: model.ProviderDeepSeek, Model: "worker-model"},
	}}
	root := &scriptedRequester{responses: []*model.Response{
		teamDispatchResponse("first-dispatch", "first", 3),
		teamDispatchResponse("second-dispatch", "second", 1),
		teamDispatchResponse("stale-dispatch", "stale", 1),
		teamTextResponse("finished after the quota was reached"),
	}}
	agent, err := NewWithOptions(root, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderDeepSeek,
		Model: "lead-model", Team: team, Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("use the full per-agent child quota")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "cumulative-team-quota", nil, input, TaskOptions{}, store, graph.Options{MaxConcurrency: 4})
	if err != nil || result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("cumulative quota run = status %q, error %v", result.Status, err)
	}
	if len(team.assignments) != 4 || result.Final.TeamChildrenCreated != 4 {
		t.Fatalf("accepted children = %d, checkpoint count = %d; want 4 and 4", len(team.assignments), result.Final.TeamChildrenCreated)
	}
	if got := teamToolMaxItems(t, root.requests[0].Tools); got != 0 {
		t.Fatalf("initial maxItems = %d, want absent", got)
	}
	if got := teamToolMaxItems(t, root.requests[1].Tools); got != 0 {
		t.Fatalf("maxItems after first dispatch = %d, want absent", got)
	}
	initialSchema, _ := json.Marshal(root.requests[0].Tools)
	for _, request := range root.requests[1:] {
		schema, _ := json.Marshal(request.Tools)
		if string(schema) != string(initialSchema) {
			t.Fatal("delegation quota mutated tool declarations")
		}
	}
	if !hasTeamTool(root.requests[2].Tools) {
		t.Fatal("delegate_team declaration changed after the cumulative quota was exhausted")
	}
	if history := teamResponseHistoryText(root.requests[3].Input); !strings.Contains(history, "reached its delegation limit") {
		t.Fatalf("stale delegation call was not rejected: %s", history)
	}
}

func TestAgentTaskTeamLimitsOverrideHostDefaults(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	team := &scriptedTeamExecutor{limit: 2, profiles: map[string]model.ModelProfile{
		"worker": {Provider: model.ProviderDeepSeek, Model: "worker-model"},
	}}
	root := &scriptedRequester{responses: []*model.Response{teamDispatchResponse("override-dispatch", "child", 2), teamTextResponse("done")}}
	agent, err := NewWithOptions(root, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: model.ProviderDeepSeek,
		Model: "lead-model", Team: team,
		Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 8, MaxTeamDepth: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("use this task's team limits")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "task-team-overrides", nil, input, TaskOptions{
		Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 2, MaxTeamDepth: 1},
	}, store, graph.Options{MaxConcurrency: 2})
	if err != nil || result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("task override run = status %q, error %v", result.Status, err)
	}
	if result.Final.Execution.MaxTeamChildrenPerAgent != 2 || result.Final.Execution.MaxTeamDepth != 1 {
		t.Fatalf("effective task limits = %#v, want 2 children and depth 1", result.Final.Execution)
	}
	if got := teamToolMaxItems(t, root.requests[0].Tools); got != 0 {
		t.Fatalf("task override schema maxItems = %d, want absent", got)
	}
	childDefinitions, _, err := agent.visibleTools(State{Execution: result.Final.Execution, TeamDepth: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	if hasTeamTool(childDefinitions) {
		t.Fatal("task depth override did not hide delegate_team from a depth-one agent")
	}
}

type treeTeamExecutor struct {
	profiles map[string]model.ModelProfile
	mu       sync.Mutex
	calls    map[string]int
	requests map[string][]model.ModelRequest
	prepared []State
}

func (t *treeTeamExecutor) Profiles() map[string]model.ModelProfile { return t.profiles }

func (t *treeTeamExecutor) MaxParallelAgents() int { return 4 }

func (t *treeTeamExecutor) PrepareBranch(_ context.Context, state State, assignment TeamAssignment, _ TaskOptions) (State, error) {
	t.mu.Lock()
	t.prepared = append(t.prepared, state)
	t.mu.Unlock()
	profile := t.profiles[assignment.Model]
	state.ModelProfile, state.Model, state.Provider = assignment.Model, profile.Model, profile.Provider
	state.Protocol = model.ProtocolResponses
	if profile.Provider == model.ProviderAnthropic {
		state.Protocol = model.ProtocolAnthropicMessages
	}
	state.TeamModel, state.TeamProvider = profile.Model, profile.Provider
	return state, nil
}

func (t *treeTeamExecutor) Requester(_ context.Context, state State) (Requester, error) {
	t.mu.Lock()
	call := t.calls[state.AgentID]
	t.calls[state.AgentID]++
	t.mu.Unlock()
	return treeTeamRequester{team: t, agentID: state.AgentID, depth: state.TeamDepth, call: call}, nil
}

type treeTeamRequester struct {
	team    *treeTeamExecutor
	agentID string
	depth   int
	call    int
}

func (r treeTeamRequester) CreateResponse(_ context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	r.team.mu.Lock()
	r.team.requests[r.agentID] = append(r.team.requests[r.agentID], request)
	r.team.mu.Unlock()
	if r.depth == 1 && r.call == 0 {
		return teamDispatchResponse("tree-"+strings.ReplaceAll(r.agentID, "/", "-"), "leaf", 4), nil
	}
	return teamTextResponse("branch completed"), nil
}

func teamDispatchResponse(callID, prefix string, count int) *model.Response {
	return teamDispatchResponseWithMode(callID, prefix, count, TeamDispatchTask)
}

func teamDispatchResponseWithMode(callID, prefix string, count int, mode string) *model.Response {
	if count < 1 {
		panic("team dispatch test response requires at least one task")
	}
	tasks := make([]TeamAssignment, count)
	for index := range tasks {
		tasks[index] = TeamAssignment{ID: fmt.Sprintf("%s-%d", prefix, index), Model: "worker", Task: fmt.Sprintf("%s task %d", prefix, index)}
	}
	return teamDispatchResponseForAssignments(callID, mode, tasks)
}

func teamDispatchResponseForAssignments(callID, mode string, tasks []TeamAssignment) *model.Response {
	arguments, err := json.Marshal(map[string]any{"mode": mode, "tasks": tasks, "minimum_successes": nil})
	if err != nil {
		panic(err)
	}
	return &model.Response{Status: "completed", Output: []model.ResponseOutputItem{
		model.OutputFunctionCall{CallID: callID, Name: teamDispatchToolName, Arguments: string(arguments)},
	}}
}

func teamTextResponse(text string) *model.Response {
	return &model.Response{Status: "completed", Output: []model.ResponseOutputItem{
		model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: text}}},
	}}
}

func teamToolMaxItems(t *testing.T, definitions []model.ResponseTool) int {
	t.Helper()
	for _, definition := range definitions {
		function, ok := definition.(model.FunctionTool)
		if !ok || function.Name != teamDispatchToolName {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				MaxItems int `json:"maxItems"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(function.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		return schema.Properties["tasks"].MaxItems
	}
	t.Fatal("delegate_team not found")
	return 0
}

func hasTeamTool(definitions []model.ResponseTool) bool {
	for _, definition := range definitions {
		if function, ok := definition.(model.FunctionTool); ok && function.Name == teamDispatchToolName {
			return true
		}
	}
	return false
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
		Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 2, MaxTeamDepth: 2},
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

type forkRecordingTeamExecutor struct {
	profiles map[string]model.ModelProfile
	mu       sync.Mutex
	requests map[string]model.ModelRequest
	states   map[string]State
}

func (t *forkRecordingTeamExecutor) Profiles() map[string]model.ModelProfile { return t.profiles }

func (t *forkRecordingTeamExecutor) MaxParallelAgents() int { return 4 }

func (t *forkRecordingTeamExecutor) PrepareBranch(_ context.Context, state State, assignment TeamAssignment, _ TaskOptions) (State, error) {
	t.mu.Lock()
	t.states[state.AgentID] = state
	t.mu.Unlock()
	profile := t.profiles[assignment.Model]
	state.ModelProfile, state.Model, state.Provider = assignment.Model, profile.Model, profile.Provider
	state.Protocol = model.ProtocolResponses
	state.TeamModel, state.TeamProvider = profile.Model, profile.Provider
	return state, nil
}

func (t *forkRecordingTeamExecutor) Requester(_ context.Context, state State) (Requester, error) {
	return forkRecordingRequester{team: t, agentID: state.AgentID}, nil
}

type forkRecordingRequester struct {
	team    *forkRecordingTeamExecutor
	agentID string
}

func (r forkRecordingRequester) CreateResponse(_ context.Context, request model.ModelRequest) (*model.ModelResponse, error) {
	r.team.mu.Lock()
	r.team.requests[r.agentID] = request
	r.team.mu.Unlock()
	return teamTextResponse("completed " + r.agentID), nil
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
