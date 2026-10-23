package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
)

type smokeStepProvider struct {
	mu    sync.Mutex
	steps []func(request model.ModelRequest) (*model.ModelResponse, error)
	calls int
}

func (p *smokeStepProvider) Name() string { return "smoke_provider" }

func (p *smokeStepProvider) CreateResponse(_ context.Context, request model.ModelRequest, _ model.ModelProfile) (*model.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls >= len(p.steps) {
		return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{
			model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "default fallback done"}}},
		}}, nil
	}
	step := p.steps[p.calls]
	p.calls++
	return step(request)
}

func (p *smokeStepProvider) StreamResponse(ctx context.Context, request model.ModelRequest, profile model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err != nil {
		return nil, err
	}
	if emit != nil {
		if err := emit(model.ModelStreamEvent{Type: model.ModelStreamCompleted, Response: response}); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func TestCoreFullLifecycleSmoke(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()

	testFile := filepath.Join(workspace, "input.txt")
	if err := os.WriteFile(testFile, []byte("smoke test input data"), 0600); err != nil {
		t.Fatal(err)
	}

	var openedURLs []string
	var urlMu sync.Mutex
	browser := core.HeadlessBrowserOpener{
		OnOpen: func(url string) {
			urlMu.Lock()
			openedURLs = append(openedURLs, url)
			urlMu.Unlock()
		},
	}
	credentials := core.NewMemoryCredentialStorage()

	provider := &smokeStepProvider{
		steps: []func(request model.ModelRequest) (*model.ModelResponse, error){
			// Step 1: Lead agent asks clarification
			func(req model.ModelRequest) (*model.ModelResponse, error) {
				questions := []core.UserQuestionItem{
					{
						ID:       "target_action",
						Question: "Which action to perform?",
						Options:  []core.UserQuestionOption{{Label: "Inspect"}, {Label: "Transform"}},
					},
				}
				data, _ := json.Marshal(map[string]any{"questions": questions})
				return &model.ModelResponse{
					Status: "completed",
					Output: []model.ResponseOutputItem{
						model.OutputFunctionCall{CallID: "ask-1", Name: "ask_user", Arguments: string(data)},
					},
				}, nil
			},
			// Step 2: After user answers "Inspect", read input.txt
			func(req model.ModelRequest) (*model.ModelResponse, error) {
				return &model.ModelResponse{
					Status: "completed",
					Output: []model.ResponseOutputItem{
						model.OutputFunctionCall{CallID: "read-1", Name: "read_file", Arguments: `{"path":"input.txt"}`},
					},
				}, nil
			},
			// Step 3: Complete task
			func(req model.ModelRequest) (*model.ModelResponse, error) {
				return &model.ModelResponse{
					Status: "completed",
					Output: []model.ResponseOutputItem{
						model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "Smoke execution finished successfully."}}},
					},
				}, nil
			},
		},
	}

	config := core.Config{
		Workspace:         workspace,
		StateDir:          stateDir,
		BrowserOpener:     browser,
		CredentialStorage: credentials,
		ModelConfig: &model.Config{
			Providers:      map[string]model.ProviderConfig{provider.Name(): {Protocol: model.ProtocolResponses}},
			Profiles:       map[string]model.ModelProfile{"main": {Provider: provider.Name(), Model: "test-model"}},
			DefaultProfile: "main",
		},
		ModelProviders: map[string]model.Provider{
			provider.Name(): provider,
		},
	}

	c, err := core.New(config)
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	session, err := c.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	runtime, err := c.OpenSessionRuntime(ctx, session.ID)
	if err != nil {
		t.Fatalf("OpenSessionRuntime: %v", err)
	}
	defer runtime.Close()

	taskInput, err := core.NewTaskInput("run smoke task")
	if err != nil {
		t.Fatal(err)
	}

	sub, err := runtime.SubmitTurn(ctx, core.TurnRequest{Input: taskInput}, core.StartIfIdle)
	if err != nil {
		t.Fatalf("SubmitTurn: %v", err)
	}

	// 1. Wait for task to pause at clarification
	var wait core.UserQuestionRequest
	deadline := time.Now().Add(5 * time.Second)
	for {
		reqs, err := runtime.PendingRequests(ctx)
		if err == nil {
			for _, r := range reqs {
				if r.Kind == core.PendingUserQuestion && r.UserQuestion != nil {
					wait = *r.UserQuestion
					break
				}
			}
		}
		if wait.ID != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wait.ID == "" {
		t.Fatal("expected pending clarification question")
	}

	identity := core.UserQuestionAnswer{
		ExecutionID:  wait.ExecutionID,
		RunID:        wait.RunID,
		InvocationID: wait.InvocationID,
		CallID:       wait.CallID,
	}

	// 2. Test saving opaque draft payload
	opaquePayload := []byte(`{"ui_stage":"review","selected":"Inspect"}`)
	if err := runtime.SaveUserQuestionDraftPayload(ctx, identity, opaquePayload); err != nil {
		t.Fatalf("SaveUserQuestionDraftPayload: %v", err)
	}

	reqs, err := runtime.PendingRequests(ctx)
	if err != nil {
		t.Fatalf("PendingRequests after payload draft: %v", err)
	}
	var inspectedReq *core.UserQuestionRequest
	for _, req := range reqs {
		if req.Kind == core.PendingUserQuestion && req.UserQuestion != nil {
			inspectedReq = req.UserQuestion
			break
		}
	}
	if inspectedReq == nil || string(inspectedReq.DraftPayload) != string(opaquePayload) {
		t.Fatalf("expected DraftPayload %s, got %+v", string(opaquePayload), inspectedReq)
	}

	// 3. Test saving structured draft
	structuredDraft := core.UserQuestionDraft{
		CurrentQuestionID: "target_action",
		Responses: []core.UserQuestionDraftResponse{
			{QuestionID: "target_action", SelectedOption: 0, Confirmed: true},
		},
	}
	if err := runtime.SaveUserQuestionDraft(ctx, identity, structuredDraft); err != nil {
		t.Fatalf("SaveUserQuestionDraft: %v", err)
	}

	// 4. Resolve the question
	identity.Answers = []core.UserQuestionResponse{
		{QuestionID: "target_action", Answer: "Inspect"},
	}
	if err := runtime.ResolveUserQuestion(ctx, identity); err != nil {
		t.Fatalf("ResolveUserQuestion: %v", err)
	}

	// 5. Wait for turn result to complete
	result, err := runtime.WaitTurn(ctx, sub.ExecutionID)
	if err != nil {
		t.Fatalf("WaitTurn: %v", err)
	}
	if result.Status != core.TaskCompleted {
		t.Fatalf("task status = %s, want %s", result.Status, core.TaskCompleted)
	}

	// 6. Test session state persistence across close and reopen
	if err := runtime.Close(); err != nil {
		t.Fatalf("runtime.Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("c.Close: %v", err)
	}

	// Reopen Core and verify history is intact
	c2, err := core.New(config)
	if err != nil {
		t.Fatalf("reopen Core: %v", err)
	}
	defer c2.Close()

	loadedSession, err := c2.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetSession after reopen: %v", err)
	}
	if len(loadedSession.History) == 0 || string(loadedSession.History) == "null" {
		t.Fatalf("expected non-empty conversation history, got %s", string(loadedSession.History))
	}

	// 7. Test Session Fork
	forked, err := c2.ForkSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if forked.ID == session.ID {
		t.Fatalf("forked session should have unique ID, got %s", forked.ID)
	}
}

func TestCoreConcurrentSessionsIsolationSmoke(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()

	provider := &smokeStepProvider{
		steps: []func(request model.ModelRequest) (*model.ModelResponse, error){
			func(req model.ModelRequest) (*model.ModelResponse, error) {
				return &model.ModelResponse{
					Status: "completed",
					Output: []model.ResponseOutputItem{
						model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: "concurrent turn complete"}}},
					},
				}, nil
			},
		},
	}

	config := core.Config{
		Workspace: workspace,
		StateDir:  stateDir,
		ModelConfig: &model.Config{
			Providers:      map[string]model.ProviderConfig{provider.Name(): {Protocol: model.ProtocolResponses}},
			Profiles:       map[string]model.ModelProfile{"main": {Provider: provider.Name(), Model: "test-model"}},
			DefaultProfile: "main",
		},
		ModelProviders: map[string]model.Provider{
			provider.Name(): provider,
		},
	}

	c, err := core.New(config)
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	const concurrency = 6
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sess, err := c.CreateSession(ctx, json.RawMessage("null"))
			if err != nil {
				errs <- fmt.Errorf("[%d] CreateSession: %w", id, err)
				return
			}
			rt, err := c.OpenSessionRuntime(ctx, sess.ID)
			if err != nil {
				errs <- fmt.Errorf("[%d] OpenSessionRuntime: %w", id, err)
				return
			}
			defer rt.Close()

			input, _ := core.NewTaskInput(fmt.Sprintf("task from session %d", id))
			sub, err := rt.SubmitTurn(ctx, core.TurnRequest{Input: input}, core.StartIfIdle)
			if err != nil {
				errs <- fmt.Errorf("[%d] SubmitTurn: %w", id, err)
				return
			}
			res, err := rt.WaitTurn(ctx, sub.ExecutionID)
			if err != nil {
				errs <- fmt.Errorf("[%d] WaitTurn: %w", id, err)
				return
			}
			if res.Status != core.TaskCompleted {
				errs <- fmt.Errorf("[%d] bad status: %s", id, res.Status)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
