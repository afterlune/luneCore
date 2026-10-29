package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	publictool "github.com/afterlune/luneCore/tool"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

// Run explicitly with LUNE_CACHE_SMOKE_CONFIG pointing at a private config.json.
// The same test runs on Windows and Linux, entirely in an isolated workspace.
func TestCacheRealAPISmoke(t *testing.T) {
	configPath := os.Getenv("LUNE_CACHE_SMOKE_CONFIG")
	if configPath == "" {
		t.Skip("real API test is opt-in")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var project projectconfig.ProjectConfig
	if err = json.Unmarshal(data, &project); err != nil {
		t.Fatal(err)
	}
	profileID := project.Models.DefaultProfile
	profile := project.Models.Profiles[profileID]
	profile.MaxOutputTokens = 2048
	project.Models.Profiles = map[string]model.ModelProfile{profileID: profile}
	catalog, err := model.NewProviderCatalog((projectconfig.Config{ProjectConfig: project}).ModelConfig())
	if err != nil {
		t.Fatal(err)
	}
	emptyTools, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer emptyTools.Close()
	inner, err := catalog.NewClient(profileID, emptyTools, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	budget := &cacheSmokeProvider{inner: inner, name: profile.Provider}
	base := os.Getenv("LUNE_CACHE_SMOKE_DIR")
	if base == "" {
		base = t.TempDir()
	}
	if err = os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(base, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	client, err := New(Config{Workspace: base, StateDir: filepath.Join(base, "state"), ModelConfig: &project.Models,
		ModelProviders: map[string]model.Provider{profile.Provider: budget}, Memory: MemoryConfig{Disabled: true},
		Logger: slog.New(slog.NewTextHandler(logFile, nil)), SandboxHelperPath: os.Getenv("LUNE_CACHE_SMOKE_HELPER"),
		Execution: ExecutionOptions{ModelMaxAttempts: 1, MaxSteps: 64},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err = client.SetPermissionMode(PermissionWorkspace); err != nil {
		t.Fatal(err)
	}
	seed := model.NewResponseInputItems(model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("Reference fixture for cache verification; no action required. " + strings.Repeat("The fixture has a local text file and a read-only verification command. ", 256))}, model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText("Understood.")})
	encoded, _ := json.Marshal(seed)
	saved, err := client.CreateSession(ctx, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.SetSessionSettings(ctx, saved.ID, SessionSettings{ModelProfile: profileID, ReasoningEffort: "low"}); err != nil {
		t.Fatal(err)
	}
	rt, err := client.OpenSessionRuntime(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rt.SubscribeEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var approvals int
	var eventMu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			if e.Kind == EventApprovalRequired {
				eventMu.Lock()
				approvals++
				eventMu.Unlock()
				_ = rt.ResolveApprovalDecision(context.Background(), e.Approval.ID, publictool.ApprovalAllowOnce)
			}
		}
	}()
	defer func() { cancel(); <-done }()
	defer func() {
		budget.mu.Lock()
		defer budget.mu.Unlock()
		t.Logf("CACHE_SMOKE_SUMMARY requests=%d input=%d cached=%d output=%d", budget.requests, budget.input, budget.cached, budget.output)
	}()
	wait := func(sub TurnSubmission, err error) TurnResult {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		result, err := rt.WaitTurn(ctx, sub.ExecutionID)
		if err != nil || result.Status != TaskCompleted {
			t.Fatalf("turn=%s error=%v", result.Status, err)
		}
		return result
	}
	submit := func(text string, plan bool) TurnResult {
		input, err := NewTaskInput(text)
		if err != nil {
			t.Fatal(err)
		}
		return wait(rt.SubmitTurn(ctx, TurnRequest{Input: input, PlanMode: plan}, StartIfIdle))
	}
	submit("Use tools to write cache-smoke.txt containing alpha, run a shell command that prints the file, then use a file tool to replace alpha with beta. Do these in that order. Do not delegate, ask questions, install anything or run unrelated commands. Reply briefly.", false)
	contents, err := os.ReadFile(filepath.Join(base, "cache-smoke.txt"))
	if err != nil || !strings.Contains(string(contents), "beta") {
		t.Fatalf("first task file verification: %v", err)
	}
	submit("Read cache-smoke.txt using a file tool, verify it contains beta, then reply briefly. Do not delegate.", false)
	planned := submit("Plan exactly one change: replace beta with gamma in cache-smoke.txt. No questions or delegation are needed. Do not modify files yet.", true)
	if planned.ProposedPlan == nil {
		t.Fatal("missing proposed plan")
	}
	implementation, err := rt.ImplementPlan(ctx, planned.ExecutionID, PlanImplementationOptions{Mode: PlanImplementSameContext, PermissionMode: PermissionWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	wait(implementation.Submission, nil)
	contents, err = os.ReadFile(filepath.Join(base, "cache-smoke.txt"))
	if err != nil || !strings.Contains(string(contents), "gamma") {
		t.Fatalf("implementation file verification: %v", err)
	}
	eventMu.Lock()
	count := approvals
	eventMu.Unlock()
	if count == 0 {
		t.Fatal("command did not exercise approval")
	}
	final, err := client.GetSession(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range final.UsageRequests {
		t.Logf("request=%s attempt=%d input=%d cached=%d output=%d changes=%v", u.RequestID, u.RequestAttempt, u.Usage.InputTokens, u.Usage.InputTokensDetails.CachedTokens, u.Usage.OutputTokens, u.RequestChanges)
	}
	if final.SessionUsage.Cached == 0 {
		t.Fatal("provider reported no cache hits")
	}
}

type cacheSmokeProvider struct {
	mu                    sync.Mutex
	inner                 *model.Client
	name                  string
	requests              int
	input, cached, output int64
}

func (p *cacheSmokeProvider) Name() string { return p.name }
func (p *cacheSmokeProvider) reserve(request model.ModelRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.requests >= 12 || p.input+model.EstimateInputTokens(request) > 250000 || p.output+2048 > 10000 {
		return fmt.Errorf("cache smoke budget reached")
	}
	p.requests++
	return nil
}
func (p *cacheSmokeProvider) record(response *model.ModelResponse) {
	if response == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.input += response.Usage.InputTokens
	p.cached += response.Usage.InputTokensDetails.CachedTokens
	p.output += response.Usage.OutputTokens
}
func (p *cacheSmokeProvider) CreateResponse(ctx context.Context, request model.ModelRequest, _ model.ModelProfile) (*model.ModelResponse, error) {
	if err := p.reserve(request); err != nil {
		return nil, err
	}
	response, err := p.inner.CreateResponse(ctx, request)
	p.record(response)
	return response, err
}
func (p *cacheSmokeProvider) StreamResponse(ctx context.Context, request model.ModelRequest, _ model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	if err := p.reserve(request); err != nil {
		return nil, err
	}
	response, err := p.inner.StreamResponse(ctx, request, emit)
	p.record(response)
	return response, err
}
