package agentgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
	"github.com/afterlune/luneGraph/checkpoint/memory"
	"github.com/afterlune/luneGraph/checkpoint/sqlite"
)

type scriptedRequester struct {
	responses []*model.Response
	requests  []model.ResponseRequest
	next      int
}

type scriptedTeamExecutor struct {
	profiles    map[string]model.ModelProfile
	limit       int
	assignments []TeamAssignment
	options     []TaskOptions
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
func (t *scriptedTeamExecutor) MaxParallelAgents() int                  { return t.limit }
func (t *scriptedTeamExecutor) PrepareBranch(_ context.Context, state State, assignment TeamAssignment, options TaskOptions) (State, error) {
	t.assignments = append(t.assignments, assignment)
	t.options = append(t.options, options)
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

func (r *scriptedRequester) CreateResponse(_ context.Context, request model.ResponseRequest) (*model.Response, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var saved model.ResponseRequest
	if err := json.Unmarshal(encoded, &saved); err != nil {
		return nil, err
	}
	r.requests = append(r.requests, saved)
	if r.next >= len(r.responses) {
		return nil, errors.New("no scripted response left")
	}
	response := r.responses[r.next]
	r.next++
	return response, nil
}

type cancelOnceRequester struct {
	started chan struct{}
	calls   int
}

func (r *cancelOnceRequester) CreateResponse(ctx context.Context, _ model.ResponseRequest) (*model.Response, error) {
	r.calls++
	if r.calls == 1 {
		close(r.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return decodeResponse(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}]}`)
}

type recordingTool struct{ calls int }

func (recordingTool) Definition() model.FunctionTool {
	return model.FunctionTool{
		Name:        "read_file",
		Description: "Read a workspace file.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}
}

func (t *recordingTool) Run(context.Context, json.RawMessage) tool.Result {
	t.calls++
	return tool.Result{Output: "file contents"}
}

type namedRecordingTool struct {
	name  string
	calls int
}

type audienceRecordingTool struct{}

func (audienceRecordingTool) Definition() model.FunctionTool {
	return model.FunctionTool{Name: "audience", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (audienceRecordingTool) Run(context.Context, json.RawMessage) tool.Result {
	output := model.NewToolOutputParts(model.InputText{Text: "visible to the model"})
	return tool.Result{ModelOutput: &output, TranscriptOnly: []model.ContentPart{
		model.InputText{Text: "visible only to the user"},
		model.InputImage{ImageURL: "data:image/png;base64,CQgH"},
	}}
}

func (t *namedRecordingTool) Definition() model.FunctionTool {
	return model.FunctionTool{
		Name:        t.name,
		Description: "A test tool.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}
}

func (t *namedRecordingTool) Run(context.Context, json.RawMessage) tool.Result {
	t.calls++
	return tool.Result{Output: t.name + " completed"}
}

func toolRegistration(registeredTool tool.Tool, category model.ToolCategory, permissionClass model.ToolPermissionClass, readOnly bool) tool.ToolRegistration {
	return tool.ToolRegistration{
		Tool: registeredTool,
		Metadata: model.ToolMetadata{
			Category: category, PermissionClass: permissionClass, ReadOnly: readOnly,
		},
	}
}

func TestPlanModeUsesRegisteredReadOnlyMetadata(t *testing.T) {
	customRead := &namedRecordingTool{name: "custom_read"}
	customWrite := &namedRecordingTool{name: "custom_write"}
	networkRead := &namedRecordingTool{name: "network_lookup"}
	unclassifiedRead := &namedRecordingTool{name: "unclassified_read"}
	registry, err := tool.NewRegistry(
		toolRegistration(customRead, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true),
		toolRegistration(customWrite, model.ToolCategory{}, model.ToolPermissionWorkspaceWrite, false),
		toolRegistration(networkRead, model.ToolCategory{}, model.ToolPermissionNetwork, true),
		toolRegistration(unclassifiedRead, model.ToolCategory{}, "future-class", true),
	)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(&scriptedRequester{}, registry)
	if err != nil {
		t.Fatal(err)
	}
	definitions, _, err := agent.visibleTools(State{AgentID: "lead"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	visible := map[string]bool{}
	for _, definition := range definitions {
		if function, ok := definition.(model.FunctionTool); ok {
			visible[function.Name] = true
		}
	}
	for _, name := range []string{"custom_read", "network_lookup"} {
		if !visible[name] {
			t.Errorf("Plan Mode did not expose explicitly read-only tool %q", name)
		}
	}
	for _, name := range []string{"custom_write", "unclassified_read"} {
		if visible[name] {
			t.Errorf("Plan Mode exposed non-read-only or unclassified tool %q", name)
		}
	}
}

func TestTaskGoalIsAppliedWithoutWeakeningPlanInstructions(t *testing.T) {
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"plan ready"}]}]}`),
	}}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("inspect the project")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "goal-plan-instructions", nil, input, TaskOptions{
		Goal: "Map the public API and summarize its dependencies.", PlanMode: true,
	}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || len(requester.requests) != 1 {
		t.Fatalf("result status=%q requests=%d", result.Status, len(requester.requests))
	}
	instructions := requester.requests[0].Instructions
	for _, expected := range []string{
		"Map the public API and summarize its dependencies.",
		"does not override higher-priority instructions",
		"You are in plan mode.",
		"You may use read-only tools only.",
		"Do not claim to have made changes.",
	} {
		if !strings.Contains(instructions, expected) {
			t.Errorf("instructions %q are missing %q", instructions, expected)
		}
	}
}

func TestAgentGraphExecutesMultipleToolCallsFromOneResponse(t *testing.T) {
	readFile := &namedRecordingTool{name: "read_file"}
	writeFile := &namedRecordingTool{name: "write_file"}
	registry, err := tool.NewRegistry(
		toolRegistration(readFile, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true),
		toolRegistration(writeFile, model.ToolCategory{}, model.ToolPermissionWorkspaceWrite, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeWorkspace); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"},{"type":"function_call","call_id":"write_1","name":"write_file","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input, err := model.NewResponseInput("read and then write")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "multiple-tools", nil, input, TaskOptions{}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v, want a completed task", result)
	}
	if readFile.calls != 1 || writeFile.calls != 1 {
		t.Fatalf("tool calls = read_file:%d write_file:%d, want one each", readFile.calls, writeFile.calls)
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(requester.requests))
	}
}

func TestToolAudienceRoutingKeepsUserOnlyTextOutOfModelHistory(t *testing.T) {
	registry, err := tool.NewRegistry(toolRegistration(
		audienceRecordingTool{}, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true,
	))
	if err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"audience-1","name":"audience","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	input := model.NewResponseInputText("start")
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "audience-routing", nil, input, TaskOptions{}, store, graph.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v", result)
	}
	requestHistory, err := json.Marshal(requester.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requestHistory), "visible only to the user") || !strings.Contains(string(requestHistory), "visible to the model") {
		t.Fatalf("model history audience routing = %s", requestHistory)
	}
	transcript, err := json.Marshal(result.Final.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "visible only to the user") || !strings.Contains(string(transcript), "CQgH") {
		t.Fatalf("canonical transcript lost user-only text: %s", transcript)
	}
}

func TestAgentGraphResumesBetweenModelAndToolSteps(t *testing.T) {
	readFile := &recordingTool{}
	registry, err := tool.NewRegistry(toolRegistration(
		readFile,
		model.ToolCategory{Name: "files", Description: "Workspace files."},
		model.ToolPermissionWorkspaceRead,
		true,
	))
	if err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_1","name":"select_tool_category","arguments":"{\"category\":\"files\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	input, err := model.NewResponseInput([]map[string]any{{
		"type": "message",
		"role": "user",
		"content": []map[string]any{
			{"type": "input_text", "text": "read this attachment"},
			{"type": "input_image", "image_url": "data:image/png;base64,AA=="},
			{"type": "input_audio", "data": "opaque-audio-payload"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	options := graph.Options{MaxSteps: 1}
	execution, paused, err := agent.Start(context.Background(), "agent-poc", nil, input, TaskOptions{Model: "override-model"}, store, options)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	checkpoint, checkpointErr := execution.Checkpoint()
	if checkpointErr != nil {
		t.Fatal(checkpointErr)
	}
	if paused.Status != graph.StatusBudget || len(checkpoint.Invocations) != 1 || checkpoint.Invocations[0].Node != toolNode {
		t.Fatalf("paused result = %#v, want a checkpoint at the tool node", paused)
	}

	options.MaxSteps = 100
	_, completed, err := agent.Recover(context.Background(), "agent-poc", nil, store, options)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if completed.Status != graph.StatusCompleted || completed.Final == nil {
		t.Fatalf("completed result = %#v, want a completed checkpoint", completed)
	}
	if completed.Final.FinalResponse == nil {
		t.Fatal("final response was not retained in graph state")
	}
	if readFile.calls != 1 {
		t.Fatalf("read_file executions = %d, want 1", readFile.calls)
	}
	if len(requester.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(requester.requests))
	}
	for index, request := range requester.requests {
		if request.Model != "override-model" {
			t.Errorf("request %d model = %q, want persisted override", index+1, request.Model)
		}
	}
	if completed.Final.ModelOverride != "override-model" {
		t.Errorf("recovered model override = %q, want persisted override", completed.Final.ModelOverride)
	}
	if len(requester.requests[0].Tools) != 1 {
		t.Fatalf("initial tools = %#v, want only category selector", requester.requests[0].Tools)
	}
	for _, index := range []int{1, 2} {
		if len(requester.requests[index].Tools) != 2 {
			t.Errorf("request %d tools = %#v, want selector and read_file", index+1, requester.requests[index].Tools)
		}
	}
	encodedHistory, err := json.Marshal(requester.requests[2].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"input_audio", "opaque-audio-payload", "file contents"} {
		if !strings.Contains(string(encodedHistory), preserved) {
			t.Errorf("recovered history lost %q: %s", preserved, encodedHistory)
		}
	}
}

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
		PlanMode: true, MaxToolRounds: 7, ReasoningEffort: model.ReasoningEffortHigh,
		Goal: "parent goal", ContextWindow: 64000, ReserveOutputTokens: 2048,
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
	if len(team.options) != 2 || !team.options[0].PlanMode || team.options[0].MaxToolRounds != 7 || team.options[0].ReasoningEffort != model.ReasoningEffortHigh {
		t.Fatalf("worker options = %#v", team.options)
	}
	for _, options := range team.options {
		if options.Goal != "" || options.ContextWindow != 0 || options.ReserveOutputTokens != 0 || options.AutoCompact || options.AutoCompactThreshold != 0 {
			t.Fatalf("new task settings unexpectedly inherited by team member: %#v", options)
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

func teamResponseHistoryText(input *model.ResponseInput) string {
	encoded, _ := json.Marshal(input)
	return string(encoded)
}

func TestAgentCanAskUserDuringPlanMode(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"ask-1","name":"ask_user","arguments":"{\"question\":\"Which environment?\",\"options\":[\"local\",\"staging\"]}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will use staging."}]}]}`),
	}}
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		ID: "lead", Name: "Lead", Profile: "main", Provider: model.ProviderDeepSeek, Model: "test-model",
		AskUser: true,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("prepare the deployment plan")
	if err != nil {
		t.Fatal(err)
	}
	execution, waiting, err := agent.Start(context.Background(), "ask-user-plan", nil, input, TaskOptions{PlanMode: true}, store, graph.Options{})
	if err != nil || waiting.Status != graph.StatusWaiting {
		t.Fatalf("Start = status %q, error %v", waiting.Status, err)
	}
	pending := execution.Pending()
	if len(pending) != 1 || pending[0].Continuation != UserQuestionContinuation {
		t.Fatalf("pending user question = %#v", pending)
	}
	checkpoint, err := execution.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	var question State
	for _, invocation := range checkpoint.Invocations {
		if invocation.ID == pending[0].InvocationID {
			question = invocation.State
		}
	}
	if question.UserQuestion != "Which environment?" || len(question.UserQuestionOptions) != 2 || question.UserQuestionOptions[1] != "staging" {
		t.Fatalf("persisted user question = %#v", question)
	}
	answer, err := json.Marshal("staging")
	if err != nil {
		t.Fatal(err)
	}
	completed, err := execution.Advance(context.Background(), []graph.ResumeInput{{
		InvocationID: pending[0].InvocationID, CallID: pending[0].CallID, Payload: answer,
	}}, graph.Options{})
	if err != nil || completed.Status != graph.StatusCompleted || completed.Final == nil {
		t.Fatalf("Advance = status %q, error %v", completed.Status, err)
	}
	var foundAskUser bool
	for _, definition := range requester.requests[0].Tools {
		if function, ok := definition.(model.FunctionTool); ok && function.Name == askUserToolName {
			foundAskUser = true
		}
	}
	if !foundAskUser {
		t.Fatalf("plan-mode tools = %#v, missing %q", requester.requests[0].Tools, askUserToolName)
	}
	encodedInput, err := json.Marshal(requester.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encodedInput, []byte("User's answer: staging")) {
		t.Fatalf("user answer was not returned to the model: %s", encodedInput)
	}
}

type nestedTeamExecutor struct {
	profiles   map[string]model.ModelProfile
	requesters map[string]Requester
}

func (t *nestedTeamExecutor) Profiles() map[string]model.ModelProfile { return t.profiles }
func (t *nestedTeamExecutor) MaxParallelAgents() int                  { return 3 }
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

func TestAgentGraphRecoversSelectedCategoryAfterSQLiteStoreReopen(t *testing.T) {
	readFile := &recordingTool{}
	registry, err := tool.NewRegistry(toolRegistration(
		readFile,
		model.ToolCategory{Name: "files", Description: "Workspace files."},
		model.ToolPermissionWorkspaceRead,
		true,
	))
	if err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_1","name":"select_tool_category","arguments":"{\"category\":\"files\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "category-checkpoints.db")
	store, err := sqlite.Open[State](context.Background(), dbPath, checkpoint.JSON[State]{})
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	input, err := model.NewResponseInput("read a file")
	if err != nil {
		t.Fatal(err)
	}
	execution, paused, err := agent.Start(context.Background(), "category-recovery", nil, input, TaskOptions{}, store, graph.Options{MaxSteps: 2})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if paused.Status != graph.StatusBudget {
		t.Fatalf("paused status = %q, want budget checkpoint", paused.Status)
	}
	cp, checkpointErr := execution.Checkpoint()
	if checkpointErr != nil {
		t.Fatal(checkpointErr)
	}
	selectedCategoryPersisted := false
	for _, invocation := range cp.Invocations {
		if invocation.State.SelectedCategory == "files" {
			selectedCategoryPersisted = true
		}
	}
	if !selectedCategoryPersisted {
		t.Fatalf("checkpoint did not retain selected category: %#v", cp.Invocations)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := sqlite.Open[State](context.Background(), dbPath, checkpoint.JSON[State]{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	recoveredAgent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New after reopen: %v", err)
	}
	_, completed, err := recoveredAgent.Recover(context.Background(), "category-recovery", nil, reopened, graph.Options{MaxSteps: 100})
	if err != nil {
		t.Fatalf("Recover after reopen: %v", err)
	}
	if completed.Status != graph.StatusCompleted || completed.Final == nil {
		t.Fatalf("recovered result = %#v, want completed task", completed)
	}
	if readFile.calls != 1 || len(requester.requests) != 3 {
		t.Fatalf("read_file calls=%d, model requests=%d; want one tool call and three requests", readFile.calls, len(requester.requests))
	}
	for _, index := range []int{1, 2} {
		got := strings.Join(toolNames(requester.requests[index].Tools), ",")
		want := model.CategorySelectorName + ",read_file"
		if got != want {
			t.Errorf("request %d visible tools = %q, want %q", index+1, got, want)
		}
	}
}

func TestAgentGraphGuidesHiddenToolCategoryAndLogsMismatch(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	readFile := &namedRecordingTool{name: "read_file"}
	webSearch := &namedRecordingTool{name: "web_search"}
	registry, err := tool.NewRegistry(
		toolRegistration(readFile, model.ToolCategory{Name: "files", Description: "Workspace files."}, model.ToolPermissionWorkspaceRead, true),
		toolRegistration(webSearch, model.ToolCategory{Name: "web", Description: "Web search."}, model.ToolPermissionNetwork, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_files","name":"select_tool_category","arguments":"{\"category\":\"files\"}"},{"type":"function_call","call_id":"hidden_web","name":"web_search","arguments":"{\"query\":\"PRIVATE-ARGUMENTS\"}"},{"type":"function_call","call_id":"unknown_tool","name":"imaginary_tool","arguments":"{\"secret\":\"PRIVATE-ARGUMENTS\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_web","name":"select_tool_category","arguments":"{\"category\":\"web\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"web_call","name":"web_search","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input, err := model.NewResponseInput("search the web")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "hidden-tool-category", nil, input, TaskOptions{ReasoningEffort: model.ReasoningEffortHigh}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || len(requester.requests) != 4 {
		t.Fatalf("result status=%q requests=%d, want completed with four requests", result.Status, len(requester.requests))
	}
	if readFile.calls != 0 || webSearch.calls != 1 {
		t.Fatalf("tool calls = read_file:%d web_search:%d, want only visible web_search", readFile.calls, webSearch.calls)
	}
	for requestIndex, want := range []string{
		model.CategorySelectorName,
		model.CategorySelectorName + ",read_file",
		model.CategorySelectorName + ",web_search",
		model.CategorySelectorName + ",web_search",
	} {
		got := strings.Join(toolNames(requester.requests[requestIndex].Tools), ",")
		if got != want {
			t.Errorf("request %d visible tools = %q, want %q", requestIndex+1, got, want)
		}
	}
	encodedHistory, err := json.Marshal(requester.requests[1].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"web_search", `category \"web\"`, model.CategorySelectorName, "next request", "imaginary_tool", "not registered in a discoverable category"} {
		if !strings.Contains(string(encodedHistory), expected) {
			t.Errorf("hidden tool history is missing %q: %s", expected, encodedHistory)
		}
	}
	for _, expected := range []string{
		"agent model request started",
		"request=2",
		"reasoning_effort=high",
		"selected_category=files",
		`visible_tools="[select_tool_category read_file]"`,
		"tool category selected",
		"tool call is not in the visible tool set",
		"tool=web_search",
		"visible_tools",
		"expected_category=web",
	} {
		if !strings.Contains(logBuffer.String(), expected) {
			t.Errorf("logs %q missing %q", logBuffer.String(), expected)
		}
	}
	if strings.Contains(logBuffer.String(), "PRIVATE-ARGUMENTS") {
		t.Fatalf("logs exposed tool arguments: %s", logBuffer.String())
	}
}

func TestAgentGraphRecoversAfterCancellation(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	requester := &cancelOnceRequester{started: make(chan struct{})}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input, err := model.NewResponseInput("cancel then resume")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	options := graph.Options{}
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result graph.Result[State]
		err    error
	}, 1)
	go func() {
		_, result, err := agent.Start(ctx, "cancel-poc", nil, input, TaskOptions{}, store, options)
		resultCh <- struct {
			result graph.Result[State]
			err    error
		}{result: result, err: err}
	}()
	<-requester.started
	cancel()
	interrupted := <-resultCh
	if !errors.Is(interrupted.err, context.Canceled) || interrupted.result.Status != graph.StatusCancelled {
		t.Fatalf("interrupted result = %#v, error = %v", interrupted.result, interrupted.err)
	}

	_, recovered, err := agent.Recover(context.Background(), "cancel-poc", nil, store, options)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if recovered.Status != graph.StatusCompleted || requester.calls != 2 {
		t.Fatalf("recovered status=%q requests=%d, want completed after replay", recovered.Status, requester.calls)
	}
}

func TestToolInvocationUsesStableGraphCallIDAcrossReplay(t *testing.T) {
	registered := &namedRecordingTool{name: "replay_tool"}
	registry, err := tool.NewRegistry(toolRegistration(registered, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"model-tool-call","name":"replay_tool","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var mu sync.Mutex
	var toolCallIDs, graphCallIDs []string
	controllerCalls := 0
	registry.SetToolCallController(func(ctx context.Context, _ model.OutputFunctionCall, execute tool.ToolCallExecution) (model.InputFunctionCallOutput, error) {
		mu.Lock()
		toolCallIDs = append(toolCallIDs, publictool.InvocationID(ctx))
		controllerCalls++
		call := controllerCalls
		mu.Unlock()
		if call == 1 {
			close(started)
			<-ctx.Done()
			return model.InputFunctionCallOutput{}, ctx.Err()
		}
		return execute(), nil
	})
	observer := graph.ObserverFunc(func(_ context.Context, event graph.Event) {
		if event.Operation == graph.OperationNode && event.Node == toolNode && event.Phase == graph.PhaseStarted {
			mu.Lock()
			graphCallIDs = append(graphCallIDs, event.CallID)
			mu.Unlock()
		}
	})
	input := model.NewResponseInputText("run and replay a tool")
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result graph.Result[State]
		err    error
	}, 1)
	go func() {
		_, result, runErr := agent.Start(ctx, "stable-tool-call-id", nil, input, TaskOptions{}, store, graph.Options{Observer: observer})
		resultCh <- struct {
			result graph.Result[State]
			err    error
		}{result: result, err: runErr}
	}()
	<-started
	cancel()
	interrupted := <-resultCh
	if !errors.Is(interrupted.err, context.Canceled) || interrupted.result.Status != graph.StatusCancelled {
		t.Fatalf("interrupted tool run = %#v, error=%v", interrupted.result, interrupted.err)
	}
	_, completed, err := agent.Recover(context.Background(), "stable-tool-call-id", nil, store, graph.Options{Observer: observer})
	if err != nil || completed.Status != graph.StatusCompleted {
		t.Fatalf("recovered tool run = %#v, error=%v", completed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(toolCallIDs) != 2 || toolCallIDs[0] == "" || toolCallIDs[0] != toolCallIDs[1] {
		t.Fatalf("tool invocation identities across replay = %v", toolCallIDs)
	}
	if len(graphCallIDs) != 2 || graphCallIDs[0] != graphCallIDs[1] || toolCallIDs[0] != graphCallIDs[0] {
		t.Fatalf("graph callback identities=%v, tool identities=%v", graphCallIDs, toolCallIDs)
	}
	if registered.calls != 1 {
		t.Fatalf("tool side effects = %d, want one after callback replay", registered.calls)
	}
}

func TestAgentGraphRecoversAfterSQLiteStoreReopen(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	requester := &cancelOnceRequester{started: make(chan struct{})}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := sqlite.Open[State](context.Background(), dbPath, checkpoint.JSON[State]{})
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	input, err := model.NewResponseInput([]map[string]any{{
		"type": "message", "role": "user", "content": []map[string]any{
			{"type": "input_text", "text": "persist this task"},
			{"type": "input_audio", "data": "audio-payload"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result graph.Result[State]
		err    error
	}, 1)
	go func() {
		_, result, runErr := agent.Start(ctx, "sqlite-cancel", nil, input, TaskOptions{}, store, graph.Options{})
		resultCh <- struct {
			result graph.Result[State]
			err    error
		}{result: result, err: runErr}
	}()
	<-requester.started
	cancel()
	interrupted := <-resultCh
	if !errors.Is(interrupted.err, context.Canceled) || interrupted.result.Status != graph.StatusCancelled {
		t.Fatalf("interrupted result = %#v, error = %v", interrupted.result, interrupted.err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := sqlite.Open[State](context.Background(), dbPath, checkpoint.JSON[State]{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	recoveredAgent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New after reopen: %v", err)
	}
	_, recovered, err := recoveredAgent.Recover(context.Background(), "sqlite-cancel", nil, reopened, graph.Options{})
	if err != nil {
		t.Fatalf("Recover after reopen: %v", err)
	}
	if recovered.Status != graph.StatusCompleted || recovered.Final == nil || recovered.Final.FinalResponse == nil {
		t.Fatalf("recovered result = %#v, want completed final state", recovered)
	}
	if requester.calls != 2 {
		t.Fatalf("model requests = %d, want original call plus replay", requester.calls)
	}
	encoded, err := json.Marshal(recovered.Final.History)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"persist this task", "input_audio", "audio-payload"} {
		if !strings.Contains(string(encoded), preserved) {
			t.Fatalf("recovered history lost %q: %s", preserved, encoded)
		}
	}
}

func TestAgentGraphPreservesHistoryAndEnforcesPerTaskToolRoundLimit(t *testing.T) {
	readFile := &recordingTool{}
	registry, err := tool.NewRegistry(toolRegistration(readFile, model.ToolCategory{}, model.ToolPermissionWorkspaceRead, true))
	if err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_1","name":"read_file","arguments":"{}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"read_2","name":"read_file","arguments":"{}"}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	options := graph.Options{}
	history, err := model.NewResponseInput([]map[string]any{{
		"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "previous turn"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("current turn")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "round-limit", history, input, TaskOptions{MaxToolRounds: 1}, store, options)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || result.Final == nil {
		t.Fatalf("result = %#v, want completed final state", result)
	}
	final := result.Final
	if final.TaskError == "" || !strings.Contains(final.TaskError, model.ErrToolCallLimit.Error()) {
		t.Fatalf("task error = %q, want tool-call limit error", final.TaskError)
	}
	if readFile.calls != 1 {
		t.Fatalf("read_file executions = %d, want 1 before the next batch is skipped", readFile.calls)
	}
	if len(requester.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(requester.requests))
	}
	encodedRequest, err := json.Marshal(requester.requests[0].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"previous turn", "current turn"} {
		if !strings.Contains(string(encodedRequest), content) {
			t.Errorf("request history lost %q: %s", content, encodedRequest)
		}
	}
	if !strings.Contains(string(mustMarshal(t, final.History)), "Skipped because the tool-call round limit was reached.") {
		t.Fatal("final history does not record the skipped tool call")
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestPlanModeExposesExplorationToolsAndBlocksWrites(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "main.go"), []byte("package main\n// target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewDefaultRegistry(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.SetPermissionMode(tool.PermissionModeFullAccess); err != nil {
		t.Fatal(err)
	}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"select_1","name":"select_tool_category","arguments":"{\"category\":\"files\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"list_1","name":"list_dir","arguments":"{}"},{"type":"function_call","call_id":"glob_1","name":"glob","arguments":"{\"pattern\":\"**/*.go\"}"},{"type":"function_call","call_id":"grep_1","name":"grep","arguments":"{\"pattern\":\"target\"}"},{"type":"function_call","call_id":"write_1","name":"write_file","arguments":"{\"path\":\"blocked.txt\",\"content\":\"no\"}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"plan ready"}]}]}`),
	}}
	agent, err := New(requester, registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input, err := model.NewResponseInput("inspect the repository")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := agent.Start(context.Background(), "plan-exploration-tools", nil, input, TaskOptions{PlanMode: true}, store, graph.Options{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != graph.StatusCompleted || len(requester.requests) != 3 {
		t.Fatalf("result = %#v, model requests = %d", result, len(requester.requests))
	}

	visible := map[string]bool{}
	for _, definition := range requester.requests[1].Tools {
		if function, ok := definition.(model.FunctionTool); ok {
			visible[function.Name] = true
		}
	}
	for _, name := range []string{"list_dir", "glob", "grep", "read_file", model.CategorySelectorName} {
		if !visible[name] {
			t.Errorf("Plan mode did not expose %q: %#v", name, visible)
		}
	}
	for _, name := range []string{"write_file", "edit_file", "command"} {
		if visible[name] {
			t.Errorf("Plan mode exposed %q", name)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "blocked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked write created a file: %v", err)
	}
	encoded, err := json.Marshal(requester.requests[2].Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"src", "src/main.go", "src/main.go:2:// target", "Tool is not available in plan mode."} {
		if !strings.Contains(string(encoded), output) {
			t.Errorf("tool output %q missing from history: %s", output, encoded)
		}
	}
}

func decodeResponse(raw string) (*model.Response, error) {
	var response model.Response
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, err
	}
	return &response, nil
}

func mustDecodeResponse(t *testing.T, raw string) *model.Response {
	t.Helper()
	response, err := decodeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
