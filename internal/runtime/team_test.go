package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/agentruntime"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint/memory"
)

type teamGraphProbe struct {
	active  atomic.Int32
	maximum atomic.Int32
	arrived atomic.Int32
	allIn   chan struct{}
	release chan struct{}
	summary atomic.Pointer[string]
}

type teamGraphProvider struct {
	name  string
	probe *teamGraphProbe
	lead  atomic.Int32
}

func (p *teamGraphProvider) Name() string { return p.name }

func (p *teamGraphProvider) StreamResponse(ctx context.Context, request model.ModelRequest, profile model.ModelProfile, _ func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	return p.CreateResponse(ctx, request, profile)
}

func (p *teamGraphProvider) CreateResponse(ctx context.Context, request model.ModelRequest, profile model.ModelProfile) (*model.ModelResponse, error) {
	switch profile.Model {
	case "lead-model":
		if p.lead.Add(1) == 1 {
			return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{model.OutputFunctionCall{
				CallID: "delegate-1", Name: "delegate_team",
				Arguments: `{"tasks":[{"id":"reader","model":"fast","task":"inspect package layout"},{"id":"reviewer","model":"review","task":"review permission flow"}]}`,
			}}}, nil
		}
		encoded, _ := json.Marshal(request.Input)
		text := string(encoded)
		p.probe.summary.Store(&text)
		return textResponse("synthesized team results"), nil
	case "fast-model", "review-model":
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
		if profile.Model == "review-model" {
			return nil, errors.New("review provider unavailable")
		}
		return textResponse("reader found the relevant behavior"), nil
	default:
		return nil, errors.New("unexpected model profile " + profile.Model)
	}
}

func textResponse(text string) *model.ModelResponse {
	return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{
		model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: text}}},
	}, Usage: model.ResponseUsage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}}
}

func TestTeamBranchesRunConcurrentlyAndJoinInAssignmentOrder(t *testing.T) {
	probe := &teamGraphProbe{allIn: make(chan struct{}), release: make(chan struct{})}
	providers := map[string]model.Provider{
		model.ProviderDeepSeek:  &teamGraphProvider{name: model.ProviderDeepSeek, probe: probe},
		model.ProviderAnthropic: &teamGraphProvider{name: model.ProviderAnthropic, probe: probe},
	}
	config := model.Config{
		Providers: map[string]model.ProviderConfig{
			model.ProviderDeepSeek:  {Protocol: model.ProtocolResponses, BaseURL: "https://deepseek.example", APIKey: "test"},
			model.ProviderAnthropic: {Protocol: model.ProtocolAnthropicMessages, BaseURL: "https://anthropic.example", APIKey: "test"},
		},
		Models: map[string]model.ModelProfile{
			"lead":   {Provider: model.ProviderDeepSeek, Model: "lead-model"},
			"fast":   {Provider: model.ProviderDeepSeek, Model: "fast-model", ContextWindow: 64000},
			"review": {Provider: model.ProviderAnthropic, Model: "review-model", MaxOutputTokens: 1024},
		},
		DefaultModel: "lead", MaxParallelAgents: 2,
	}
	catalog, err := model.NewProviderCatalogWithProviders(config, providers)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	checkpoints, err := memory.New[agentgraph.State](graph.JSONClone[agentgraph.State])
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sessionStore.Close()
	core := &Core{
		workspace: t.TempDir(), models: catalog, registry: registry, sessions: sessionStore,
		activeTasks: make(map[string]TaskState),
	}
	saved, err := core.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	core.activeTasks[saved.ID] = TaskState{SessionID: saved.ID, TaskID: "team-task", ExecutionID: "team-execution", Status: TaskRunning}
	core.team = &teamExecutor{core: core, models: catalog, tools: registry}
	profile, _ := catalog.Profile("lead")
	requesterClient, err := catalog.NewClient("lead", registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	requester := agentruntime.NewUsageTrackingRequester(requesterClient, "lead", "lead", profile.Provider, profile.Model)
	agent, err := agentgraph.NewWithOptions(requester, registry, agentgraph.AgentOptions{
		ID: "lead", Name: "Lead", Profile: "lead", Provider: profile.Provider, Model: profile.Model,
		Protocol: model.ProtocolResponses, Team: core.team,
		Execution: agentgraph.ExecutionOptions{MaxTeamChildrenPerAgent: 2, MaxTeamDepth: 1},
		StreamObserverFor: func(agent agentgraph.State, event model.ModelStreamEvent) {
			core.onModelStreamForTask(TaskState{}, agent.AgentID, agent.AgentName, agent.ModelProfile, agent.Provider, agent.Model, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input, err := model.NewResponseInput("inspect the project")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = publictool.WithCallIdentity(ctx, publictool.CallIdentity{
		SessionID: saved.ID, TaskID: "team-task", ExecutionID: "team-execution", RunID: "team-graph-run", AgentID: "lead",
	})
	completed := make(chan struct {
		result graph.Result[agentgraph.State]
		err    error
	}, 1)
	go func() {
		_, result, runErr := agent.Start(ctx, "team-graph-run", nil, input, agentgraph.TaskOptions{ExecutionID: "team-execution"}, checkpoints, graph.Options{MaxConcurrency: 2})
		completed <- struct {
			result graph.Result[agentgraph.State]
			err    error
		}{result, runErr}
	}()
	select {
	case <-probe.allIn:
	case <-time.After(5 * time.Second):
		close(probe.release)
		t.Fatal("team branches did not run concurrently")
	}
	if got := probe.maximum.Load(); got != 2 {
		t.Errorf("simultaneous team requests = %d, want 2", got)
	}
	close(probe.release)
	run := <-completed
	if run.err != nil || run.result.Status != graph.StatusCompleted || run.result.Final == nil {
		t.Fatalf("team run = status %q, final=%v, error=%v", run.result.Status, run.result.Final != nil, run.err)
	}
	if got := string(mustJSON(t, run.result.Final.History)); !strings.Contains(got, "reader found the relevant behavior") || !strings.Contains(got, "review provider unavailable") {
		t.Fatalf("synthesis history did not contain both branch results: %s", got)
	}
	readerIndex := strings.Index(string(mustJSON(t, run.result.Final.History)), "## reader")
	reviewerIndex := strings.Index(string(mustJSON(t, run.result.Final.History)), "## reviewer")
	if readerIndex < 0 || reviewerIndex <= readerIndex {
		t.Fatalf("team results lost assignment order: %s", run.result.Final.History.Items)
	}
	if summary := probe.summary.Load(); summary == nil || !strings.Contains(*summary, "review provider unavailable") {
		t.Fatalf("lead did not receive partial failure for synthesis: %v", summary)
	}
	usageSession, err := core.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if totals := usageSession.AgentUsage["lead/reader"]; totals.Requests != 1 || totals.Total != 5 {
		t.Errorf("team usage for lead/reader = %#v, want one measured request", totals)
	}
	for i := len(usageSession.UsageRequests) - 1; i >= 0; i-- {
		if usageSession.UsageRequests[i].AgentID == "lead/reader" {
			if got := usageSession.UsageRequests[i].ContextWindow; got != 64000 {
				t.Errorf("worker context window = %d, want profile capacity 64000", got)
			}
			return
		}
	}
	t.Error("worker usage request was not persisted")
}

func TestTeamContextWindowInheritsParentUnlessProfileOverridesIt(t *testing.T) {
	providers := map[string]model.Provider{
		model.ProviderDeepSeek: &teamGraphProvider{name: model.ProviderDeepSeek},
	}
	catalog, err := model.NewProviderCatalogWithProviders(model.Config{
		Providers: map[string]model.ProviderConfig{
			model.ProviderDeepSeek: {Protocol: model.ProtocolResponses, BaseURL: "https://deepseek.example", APIKey: "test"},
		},
		Models: map[string]model.ModelProfile{
			"inherited":  {Provider: model.ProviderDeepSeek, Model: "worker-model"},
			"configured": {Provider: model.ProviderDeepSeek, Model: "configured-model", ContextWindow: 128000},
		},
		DefaultModel: "inherited",
	}, providers)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	executor := &teamExecutor{core: &Core{}, models: catalog, tools: registry}
	parent := agentgraph.State{
		AgentID: "lead/worker", ContextWindow: 1_000_000, Provider: model.ProviderDeepSeek,
		Protocol: model.ProtocolResponses,
	}

	inherited, err := executor.PrepareBranch(context.Background(), parent, agentgraph.TeamAssignment{Model: "inherited"}, agentgraph.TaskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inherited.ContextWindow != 1_000_000 {
		t.Fatalf("inherited profile context window = %d, want parent capacity 1000000", inherited.ContextWindow)
	}
	if _, err := executor.Requester(context.Background(), inherited); err != nil {
		t.Fatalf("requester rejected inherited context capacity: %v", err)
	}

	configured, err := executor.PrepareBranch(context.Background(), parent, agentgraph.TeamAssignment{Model: "configured"}, agentgraph.TaskOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if configured.ContextWindow != 128000 {
		t.Fatalf("configured profile context window = %d, want 128000", configured.ContextWindow)
	}
	if _, err := executor.Requester(context.Background(), configured); err != nil {
		t.Fatalf("requester rejected configured profile capacity: %v", err)
	}

	staleCheckpoint := configured
	staleCheckpoint.ContextWindow = 1_000_000
	if _, err := executor.Requester(context.Background(), staleCheckpoint); err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("requester error = %v, want changed configured context capacity", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
