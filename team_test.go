package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

type parallelProbe struct {
	active  atomic.Int32
	maximum atomic.Int32
	arrived atomic.Int32
	allIn   chan struct{}
	release chan struct{}
}

type parallelTestProvider struct {
	name  string
	probe *parallelProbe
}

type cancelOnceProvider struct {
	started chan struct{}
	calls   atomic.Int32
}

func (p *cancelOnceProvider) Name() string { return model.ProviderDeepSeek }
func (p *cancelOnceProvider) CreateResponse(ctx context.Context, request model.ModelRequest, profile model.ModelProfile) (*model.ModelResponse, error) {
	return p.StreamResponse(ctx, request, profile, nil)
}
func (p *cancelOnceProvider) StreamResponse(ctx context.Context, request model.ModelRequest, profile model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	if p.calls.Add(1) == 1 {
		close(p.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	response := &model.ModelResponse{
		Status: "completed", Model: profile.Model,
		Output: []model.ResponseOutputItem{model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "resumed result"}}}},
	}
	if emit != nil {
		if err := emit(model.ModelStreamEvent{Type: model.ModelStreamTextDelta, Delta: "resumed result"}); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (p *parallelTestProvider) Name() string { return p.name }
func (p *parallelTestProvider) CreateResponse(ctx context.Context, request model.ResponseRequest, profile model.ModelProfile) (*model.Response, error) {
	return p.respond(ctx, request, profile, nil)
}
func (p *parallelTestProvider) StreamResponse(ctx context.Context, request model.ResponseRequest, profile model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.Response, error) {
	return p.respond(ctx, request, profile, emit)
}
func (p *parallelTestProvider) respond(ctx context.Context, request model.ResponseRequest, profile model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.Response, error) {
	active := p.probe.active.Add(1)
	defer p.probe.active.Add(-1)
	for previous := p.probe.maximum.Load(); active > previous; previous = p.probe.maximum.Load() {
		if p.probe.maximum.CompareAndSwap(previous, active) {
			break
		}
	}
	if p.probe.arrived.Add(1) == 2 {
		close(p.probe.allIn)
	}
	select {
	case <-p.probe.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	text := profile.Model + " completed " + responseInputText(request.Input)
	if emit != nil {
		if err := emit(model.ModelStreamEvent{Type: model.ModelStreamTextDelta, Delta: text}); err != nil {
			return nil, err
		}
	}
	return &model.Response{
		Status: "completed", Model: profile.Model,
		Output: []model.ResponseOutputItem{model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: text}}}},
		Usage:  model.ResponseUsage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5},
	}, nil
}

func TestTeamExecutorRunsPinnedProviderProfilesConcurrently(t *testing.T) {
	probe := &parallelProbe{allIn: make(chan struct{}), release: make(chan struct{})}
	config := model.Config{
		Providers: map[string]model.ProviderConfig{
			model.ProviderDeepSeek:  {Protocol: model.ProtocolResponses, BaseURL: "https://deepseek.example", APIKey: "ds-test"},
			model.ProviderAnthropic: {Protocol: model.ProtocolAnthropicMessages, BaseURL: "https://anthropic.example", APIKey: "anthropic-test"},
		},
		Models: map[string]model.ModelProfile{
			"lead":   {Provider: model.ProviderDeepSeek, Model: "deepseek-test"},
			"text":   {Provider: model.ProviderDeepSeek, Model: "deepseek-worker"},
			"review": {Provider: model.ProviderAnthropic, Model: "claude-review", MaxOutputTokens: 1024},
		},
		DefaultModel: "lead", MaxParallelAgents: 2,
	}
	catalog, err := model.NewProviderCatalogWithProviders(config, map[string]model.Provider{
		model.ProviderDeepSeek:  &parallelTestProvider{name: model.ProviderDeepSeek, probe: probe},
		model.ProviderAnthropic: &parallelTestProvider{name: model.ProviderAnthropic, probe: probe},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := memory.New[agentgraph.State](graph.JSONClone[agentgraph.State])
	if err != nil {
		t.Fatal(err)
	}
	teamStore, err := openTeamStore(filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer teamStore.Close()
	core := &Core{active: TaskState{Status: TaskRunning}, approvals: make(map[string]chan bool)}
	var eventsMu sync.Mutex
	var events []Event
	core.handler = func(event Event) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, event)
	}
	team := &teamExecutor{core: core, models: catalog, tools: registry, checkpoints: checkpoints, store: teamStore}
	assignments := []agentgraph.TeamAssignment{
		{ID: "reader", Model: "text", Task: "inspect package layout"},
		{ID: "reviewer", Model: "review", Task: "review permission flow"},
	}
	resultsCh := make(chan []agentgraph.TeamMemberResult, 1)
	errCh := make(chan error, 1)
	go func() {
		results, err := team.Run(context.Background(), "parent-task:team-call", assignments, agentgraph.TaskOptions{MaxToolRounds: 5})
		resultsCh <- results
		errCh <- err
	}()
	select {
	case <-probe.allIn:
	case <-time.After(5 * time.Second):
		close(probe.release)
		t.Fatal("team members did not reach the model concurrently")
	}
	if got := probe.maximum.Load(); got != 2 {
		t.Errorf("simultaneous model requests = %d, want 2", got)
	}
	close(probe.release)
	results := <-resultsCh
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Provider != model.ProviderDeepSeek || results[1].Provider != model.ProviderAnthropic {
		t.Fatalf("team results = %#v", results)
	}
	if results[0].Model != "deepseek-worker" || results[1].Model != "claude-review" {
		t.Fatalf("member models changed: %#v", results)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	started, finished, usage, teamRuns := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, event := range events {
		switch event.Kind {
		case EventAgentStarted:
			started[event.AgentID] = event.Provider != "" && event.Model != ""
			teamRuns[event.AgentID] = event.TeamRunID == "parent-task:team-call"
		case EventAgentFinished:
			finished[event.AgentID] = event.Status == TaskCompleted
			teamRuns[event.AgentID] = teamRuns[event.AgentID] && event.TeamRunID == "parent-task:team-call"
		case EventUsage:
			usage[event.AgentID] = event.Usage.TotalTokens == 5
		}
	}
	for _, id := range []string{"reader", "reviewer"} {
		if !started[id] || !finished[id] || !usage[id] || !teamRuns[id] {
			t.Errorf("agent %q lifecycle/usage/group missing: started=%v finished=%v usage=%v teamRunID=%v", id, started[id], finished[id], usage[id], teamRuns[id])
		}
	}
}

func TestCoreLoadsProviderCatalogFromWorkspaceConfig(t *testing.T) {
	workspace := t.TempDir()
	config := `{"providers":{"deepseek":{"protocol":"responses","baseURL":"https://deepseek.example","apiKey":"test-key"}},"models":{"primary":{"provider":"deepseek","model":"deepseek-flash"}},"defaultModel":"primary","contextWindow":64000,"autoCompact":true,"attachments":{"maxFiles":3}}`
	if err := os.WriteFile(filepath.Join(workspace, projectconfig.FileName), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	core, err := New(Config{Workspace: workspace, StateDir: filepath.Join(workspace, ".state")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if core.models.DefaultModel() != "primary" || core.teamRuns == nil {
		t.Fatalf("Core provider/team state not initialized: default=%q teamStore=%v", core.models.DefaultModel(), core.teamRuns != nil)
	}
	if core.taskDefaults.ContextWindow != 64000 || !core.taskDefaults.AutoCompact || core.taskDefaults.AutoCompactThreshold != projectconfig.DefaultAutoCompactThreshold {
		t.Fatalf("Core task defaults = %#v", core.taskDefaults)
	}
	if core.attachmentLimits.MaxFiles != 3 {
		t.Fatalf("Core attachment limits = %#v, want configured maxFiles 3", core.attachmentLimits)
	}
	input, err := json.Marshal(taskInputWithImages(
		model.InputImage{FileID: "file-1"}, model.InputImage{FileID: "file-2"},
		model.InputImage{FileID: "file-3"}, model.InputImage{FileID: "file-4"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.RunTask(context.Background(), TaskRequest{SessionID: "not-started", Input: TaskInput{Raw: input}}); err == nil || !strings.Contains(err.Error(), "attachments.maxFiles") {
		t.Fatalf("Core.RunTask did not enforce the configured image count: %v", err)
	}
	if err := core.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCoreTeamMemberHistoryIsIsolatedFromParentAndResultsReturnToParent(t *testing.T) {
	leadCalls := atomic.Int32{}
	requests := make(chan model.ResponseRequest, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- request
		var output []model.ResponseOutputItem
		switch request.Model {
		case "deepseek-lead":
			if leadCalls.Add(1) == 1 {
				output = []model.ResponseOutputItem{model.OutputFunctionCall{
					CallID: "team-call-1", Name: "delegate_team",
					Arguments: `{"tasks":[{"id":"worker","model":"worker","task":"CHILD_TASK_MARKER"}]}`,
				}}
			} else {
				output = []model.ResponseOutputItem{model.OutputMessage{
					Role:    model.RoleAssistant,
					Content: []model.OutputContentPart{model.OutputText{Text: "Parent synthesized result."}},
				}}
			}
		case "deepseek-worker":
			output = []model.ResponseOutputItem{model.OutputMessage{
				Role:    model.RoleAssistant,
				Content: []model.OutputContentPart{model.OutputText{Text: "CHILD_RESULT_MARKER"}},
			}}
		default:
			t.Errorf("unexpected model request %q", request.Model)
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		response := &model.ModelResponse{
			ID: fmt.Sprintf("response-%d", leadCalls.Load()), Object: "response", Status: "completed",
			Model: request.Model, Output: output,
		}
		data, err := json.Marshal(model.ResponseStreamEvent{
			Type: model.EventResponseCompleted, SequenceNumber: 1, Response: response,
		})
		if err != nil {
			t.Errorf("encode completed response: %v", err)
			http.Error(w, "encode response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", data)
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	writeProjectSettingsConfig(t, workspace, projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			model.ProviderDeepSeek: {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "test-key"},
		},
		Models: map[string]model.ModelProfile{
			"lead":   {Provider: model.ProviderDeepSeek, Model: "deepseek-lead"},
			"worker": {Provider: model.ProviderDeepSeek, Model: "deepseek-worker"},
		},
		DefaultModel: "lead", MaxParallelAgents: 1,
	})
	client := openSettingsCore(t, workspace, stateDir)
	ctx := context.Background()
	history := model.NewResponseInputItems(
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("PARENT_HISTORY_MARKER")},
		model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText("PARENT_ASSISTANT_MARKER")},
	)
	encodedHistory, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := client.CreateSession(ctx, encodedHistory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunTask(ctx, TaskRequest{
		SessionID: saved.ID, Input: mustSettingsTaskInput(t, "PARENT_CURRENT_MARKER"),
	}); err != nil {
		t.Fatalf("RunTask with team dispatch: %v", err)
	}

	observed := make([]model.ResponseRequest, 0, 3)
	for range 3 {
		select {
		case request := <-requests:
			observed = append(observed, request)
		case <-time.After(5 * time.Second):
			t.Fatalf("observed %d model requests, want parent, child, and parent synthesis", len(observed))
		}
	}
	if len(observed) != 3 || observed[0].Model != "deepseek-lead" || observed[1].Model != "deepseek-worker" || observed[2].Model != "deepseek-lead" {
		t.Fatalf("request models = %#v, want lead, worker, lead", []string{observed[0].Model, observed[1].Model, observed[2].Model})
	}
	inputs := make([]string, len(observed))
	for index, request := range observed {
		encoded, err := json.Marshal(request.Input)
		if err != nil {
			t.Fatalf("encode request %d input: %v", index, err)
		}
		inputs[index] = string(encoded)
	}
	if !strings.Contains(inputs[0], "PARENT_HISTORY_MARKER") || !strings.Contains(inputs[0], "PARENT_CURRENT_MARKER") {
		t.Fatalf("first parent request lost its history: %s", inputs[0])
	}
	if !strings.Contains(inputs[1], "CHILD_TASK_MARKER") || strings.Contains(inputs[1], "PARENT_HISTORY_MARKER") ||
		strings.Contains(inputs[1], "PARENT_ASSISTANT_MARKER") || strings.Contains(inputs[1], "PARENT_CURRENT_MARKER") {
		t.Fatalf("child request inherited parent history or missed its assignment: %s", inputs[1])
	}
	if !strings.Contains(inputs[2], "PARENT_HISTORY_MARKER") || !strings.Contains(inputs[2], "PARENT_CURRENT_MARKER") ||
		!strings.Contains(inputs[2], "CHILD_RESULT_MARKER") {
		t.Fatalf("parent synthesis request lost parent history or child result: %s", inputs[2])
	}
}

func TestCorePersistsTeamUsageAttributionOnSessionRequests(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	usage := Usage{InputTokens: 33, OutputTokens: 11, TotalTokens: 44}
	if err := client.recordUsageForAgent(context.Background(), saved.ID, usage, "research", "review", model.ProviderAnthropic, "claude-review", 0, 0); err != nil {
		t.Fatalf("record worker usage: %v", err)
	}
	restored, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.UsageRequests) != 1 {
		t.Fatalf("usage requests = %#v", restored.UsageRequests)
	}
	record := restored.UsageRequests[0]
	if record.AgentID != "research" || record.ModelProfile != "review" || record.Provider != model.ProviderAnthropic || record.Model != "claude-review" || record.Usage != usage {
		t.Fatalf("persisted team usage attribution = %#v", record)
	}
	if !restored.AgentUsageComplete || restored.AgentUsage["research"].Requests != 1 ||
		restored.AgentUsage["research"].Input != usage.InputTokens || restored.AgentUsage["research"].Output != usage.OutputTokens {
		t.Fatalf("persisted team usage totals = %#v complete=%v", restored.AgentUsage, restored.AgentUsageComplete)
	}
}

func responseInputText(input *model.ResponseInput) string {
	if input == nil || input.Text == nil {
		return "structured input"
	}
	return *input.Text
}

func TestTeamStoreRejectsChangedAssignmentsOnResume(t *testing.T) {
	store, err := openTeamStore(filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	profiles := map[string]model.ModelProfile{"one": {Provider: model.ProviderDeepSeek, Model: "deepseek-test"}}
	first := []agentgraph.TeamAssignment{{ID: "research", Model: "one", Task: "look at input"}}
	if _, _, err := store.GetOrCreate("parent", "call", first, profiles); err != nil {
		t.Fatal(err)
	}
	changed := []agentgraph.TeamAssignment{{ID: "research", Model: "one", Task: "different task"}}
	if _, _, err := store.GetOrCreate("parent", "call", changed, profiles); err == nil {
		t.Fatal("changed assignments should not reuse a saved team checkpoint")
	}
}

func TestTeamExecutorRejectsMemberCheckpointWithoutTaskSettingsSnapshot(t *testing.T) {
	provider := &cancelOnceProvider{started: make(chan struct{})}
	modelConfig := model.Config{
		Providers: map[string]model.ProviderConfig{
			model.ProviderDeepSeek: {Protocol: model.ProtocolResponses, BaseURL: "https://deepseek.example", APIKey: "test-key"},
		},
		Models:       map[string]model.ModelProfile{"worker": {Provider: model.ProviderDeepSeek, Model: "deepseek-worker"}},
		DefaultModel: "worker",
	}
	catalog, err := model.NewProviderCatalogWithProviders(modelConfig, map[string]model.Provider{model.ProviderDeepSeek: provider})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := memory.New[agentgraph.State](graph.JSONClone[agentgraph.State])
	if err != nil {
		t.Fatal(err)
	}
	store, err := openTeamStore(filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team := &teamExecutor{
		core: &Core{approvals: make(map[string]chan bool)}, models: catalog, tools: registry,
		checkpoints: checkpoints, store: store,
	}
	assignments := []agentgraph.TeamAssignment{{ID: "worker", Model: "worker", Task: "inspect the code"}}
	teamRun, _, err := store.GetOrCreate("parent-run", "call-1", assignments, catalog.Profiles())
	if err != nil {
		t.Fatal(err)
	}
	member := teamRun.Members[0]
	member.Status = "paused"
	if err := store.UpdateMember(teamRun.ID, member); err != nil {
		t.Fatal(err)
	}
	childRunID := teamRun.ID + ":" + member.ID
	if err := checkpoints.Create(context.Background(), graph.Checkpoint[agentgraph.State]{
		FormatVersion: graph.CheckpointFormatVersion,
		RunID:         childRunID,
		MachineID:     "lune-agent-v1",
		Revision:      1,
		NextID:        2,
		Invocations: []graph.Invocation[agentgraph.State]{
			{ID: "old-invocation", Node: "model_request", Status: graph.InvocationReady, State: agentgraph.State{RunID: childRunID}},
		},
	}); err != nil {
		t.Fatalf("create old member checkpoint: %v", err)
	}

	_, err = team.runMember(context.Background(), teamRun.ID, member, agentgraph.TaskOptions{Goal: "must not inherit"})
	if err == nil || !strings.Contains(err.Error(), "task settings version 0") {
		t.Fatalf("runMember error = %v, want unsupported settings checkpoint error", err)
	}
	currentMember, err := store.Member(teamRun.ID, member.ID)
	if err != nil || currentMember.Status != "paused" {
		t.Fatalf("member after rejected resume = %#v, error=%v; want preserved paused status", currentMember, err)
	}
	checkpoint, err := checkpoints.Load(context.Background(), childRunID)
	if err != nil || checkpoint.Invocations[0].State.TaskSettingsVersion != 0 {
		t.Fatalf("member checkpoint after rejected resume = %#v, error=%v; want preserved checkpoint", checkpoint, err)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("provider requests = %d, want 0 before rejecting old checkpoint", calls)
	}
}

func TestTeamExecutorResumesPausedMemberWithoutChangingItsModel(t *testing.T) {
	provider := &cancelOnceProvider{started: make(chan struct{})}
	config := model.Config{
		Providers: map[string]model.ProviderConfig{model.ProviderDeepSeek: {Protocol: model.ProtocolResponses, BaseURL: "https://deepseek.example", APIKey: "test-key"}},
		Models: map[string]model.ModelProfile{
			"lead":   {Provider: model.ProviderDeepSeek, Model: "deepseek-lead"},
			"worker": {Provider: model.ProviderDeepSeek, Model: "deepseek-worker"},
		},
		DefaultModel: "lead", MaxParallelAgents: 2,
	}
	catalog, err := model.NewProviderCatalogWithProviders(config, map[string]model.Provider{model.ProviderDeepSeek: provider})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := memory.New[agentgraph.State](graph.JSONClone[agentgraph.State])
	if err != nil {
		t.Fatal(err)
	}
	store, err := openTeamStore(filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	team := &teamExecutor{core: &Core{approvals: make(map[string]chan bool)}, models: catalog, tools: registry, checkpoints: checkpoints, store: store}
	assignments := []agentgraph.TeamAssignment{{ID: "worker", Model: "worker", Task: "inspect the code"}}
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, runErr := team.Run(ctx, "parent-task:team-call", assignments, agentgraph.TaskOptions{})
		firstDone <- runErr
	}()
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("worker did not reach its model request")
	}
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted team error = %v, want context canceled", err)
	}
	member, err := store.Member("parent-task:team-call", "worker")
	if err != nil || member.Status != "paused" {
		t.Fatalf("saved member state = %#v, error = %v; want paused", member, err)
	}
	results, err := team.Run(context.Background(), "parent-task:team-call", assignments, agentgraph.TaskOptions{})
	if err != nil {
		t.Fatalf("resume team: %v", err)
	}
	if len(results) != 1 || results[0].Text != "resumed result" || results[0].Model != "deepseek-worker" {
		t.Fatalf("resumed results = %#v", results)
	}
	if got := provider.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want interrupted call plus resumed call", got)
	}
}

var _ model.Provider = (*parallelTestProvider)(nil)
