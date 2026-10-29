package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
)

func benchConfig(t testing.TB, workspace, stateDir string) core.Config {
	provider := &smokeStepProvider{}
	return core.Config{
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
		Memory: core.MemoryConfig{Disabled: true},
	}
}

func BenchmarkCoreCreation(b *testing.B) {
	workspace := b.TempDir()
	stateDir := b.TempDir()
	cfg := benchConfig(b, workspace, stateDir)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		c, err := core.New(cfg)
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionRuntimeOpenClose(b *testing.B) {
	workspace := b.TempDir()
	stateDir := b.TempDir()
	cfg := benchConfig(b, workspace, stateDir)

	c, err := core.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close(context.Background())

	ctx := context.Background()
	sess, err := c.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		rt, err := c.OpenSessionRuntime(ctx, sess.ID)
		if err != nil {
			b.Fatal(err)
		}
		if err := rt.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuestionDraftSave(b *testing.B) {
	workspace := b.TempDir()
	stateDir := b.TempDir()

	questions := []core.UserQuestionItem{
		{
			ID:       "opt",
			Question: "Which option?",
			Options:  []core.UserQuestionOption{{Label: "A"}, {Label: "B"}},
		},
	}
	qData, _ := json.Marshal(map[string]any{"questions": questions})

	provider := &smokeStepProvider{
		steps: []func(request model.ModelRequest) (*model.ModelResponse, error){
			func(req model.ModelRequest) (*model.ModelResponse, error) {
				return &model.ModelResponse{
					Status: "completed",
					Output: []model.ResponseOutputItem{
						model.OutputFunctionCall{CallID: "ask-call", Name: "ask_user", Arguments: string(qData)},
					},
				}, nil
			},
		},
	}

	cfg := core.Config{
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
		Memory: core.MemoryConfig{Disabled: true},
	}

	c, err := core.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close(context.Background())

	ctx := context.Background()
	sess, err := c.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		b.Fatal(err)
	}
	rt, err := c.OpenSessionRuntime(ctx, sess.ID)
	if err != nil {
		b.Fatal(err)
	}
	defer rt.Close(context.Background())

	taskInput, _ := core.NewTaskInput("ask")
	_, err = rt.SubmitTurn(ctx, core.TurnRequest{Input: taskInput}, core.StartIfIdle)
	if err != nil {
		b.Fatal(err)
	}

	var wait core.UserQuestionRequest
	for {
		reqs, err := rt.PendingRequests(ctx)
		if err == nil {
			for _, r := range reqs {
				if r.Kind == core.PendingUserQuestion && r.UserQuestion != nil {
					wait = *r.UserQuestion
					break
				}
			}
		}
		if wait.ID != "" {
			break
		}
	}

	identity := core.UserQuestionAnswer{
		ExecutionID:  wait.ExecutionID,
		RunID:        wait.RunID,
		InvocationID: wait.InvocationID,
		CallID:       wait.CallID,
	}

	draftPayload := []byte(`{"stage":"bench","draft":{"value":42}}`)

	b.Run("SavePayload", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := rt.SaveUserQuestionDraftPayload(ctx, identity, draftPayload); err != nil {
				b.Fatal(err)
			}
		}
	})

	structured := core.UserQuestionDraft{
		CurrentQuestionID: "opt",
		Responses: []core.UserQuestionDraftResponse{
			{QuestionID: "opt", SelectedOption: 1, Confirmed: true},
		},
	}

	b.Run("SaveStructured", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := rt.SaveUserQuestionDraft(ctx, identity, structured); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkSessionCoordinatorHighConcurrency(b *testing.B) {
	workspace := b.TempDir()
	stateDir := b.TempDir()
	cfg := benchConfig(b, workspace, stateDir)

	c, err := core.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close(context.Background())

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		id := 0
		for pb.Next() {
			sess := fmt.Sprintf("session-%d", id%20)
			_ = c.State()
			_, _ = c.StateForSession(sess)
			id++
		}
	})
}
