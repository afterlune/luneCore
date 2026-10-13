package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/memory"
	publicmodel "github.com/afterlune/luneCore/model"
)

func newMemoryTestCore(t *testing.T, provider publicmodel.Provider, config MemoryConfig) *Core {
	t.Helper()
	if provider == nil {
		provider = &scriptedHostProvider{}
	}
	settings := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	settings.Memory = config
	client, err := New(settings)
	if err != nil {
		t.Fatalf("New memory Core: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close memory Core: %v", err)
		}
	})
	return client
}

func TestWorkspaceMemoryCRUDAndSearch(t *testing.T) {
	client := newMemoryTestCore(t, nil, MemoryConfig{})
	ctx := context.Background()
	item, err := client.AddMemory(ctx, memory.Input{
		Category: memory.CategoryPreference, Content: "请使用简洁中文回答。", Importance: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.ID == "" || item.Importance != 3 {
		t.Fatalf("added memory = %#v", item)
	}
	results, err := client.SearchMemories(ctx, "简洁中文", 5)
	if err != nil || len(results) != 1 || results[0].Item.ID != item.ID {
		t.Fatalf("search results = %#v, error=%v", results, err)
	}

	updated, err := client.UpdateMemory(ctx, item.ID, memory.Input{
		Category: memory.CategoryCorrection, Content: "回答请保持简洁清楚。",
	})
	if err != nil || updated.Category != memory.CategoryCorrection || updated.Content != "回答请保持简洁清楚。" {
		t.Fatalf("updated memory = %#v, error=%v", updated, err)
	}
	if results, err = client.SearchMemories(ctx, "简洁中文", 5); err != nil || len(results) != 0 {
		t.Fatalf("old content remains searchable: %#v, error=%v", results, err)
	}
	if results, err = client.SearchMemories(ctx, "简洁清楚", 5); err != nil || len(results) != 1 || results[0].Item.ID != item.ID {
		t.Fatalf("updated content search = %#v, error=%v", results, err)
	}
	toolResult := client.registry.Run(ctx, memory.SearchToolName, json.RawMessage(`{"query":"简洁清楚"}`))
	if toolResult.Err != nil || !strings.Contains(toolResult.Output, item.ID) {
		t.Fatalf("model memory search tool output = %#v", toolResult)
	}
	if err := client.DeleteMemory(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if items, err := client.ListMemories(ctx, 10); err != nil || len(items) != 0 {
		t.Fatalf("memories after delete = %#v, error=%v", items, err)
	}
}

type testMemoryEmbedder struct{ id string }

func (e *testMemoryEmbedder) ID() string { return e.id }

func (e *testMemoryEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for index, value := range texts {
		if strings.Contains(value, "短答案") || strings.Contains(value, "简洁") {
			vectors[index] = []float32{1, 0}
		} else {
			vectors[index] = []float32{0, 1}
		}
	}
	return vectors, nil
}

func TestWorkspaceMemoryUsesHostEmbeddingsForHybridSearch(t *testing.T) {
	embedder := &testMemoryEmbedder{id: "test-v1"}
	client := newMemoryTestCore(t, nil, MemoryConfig{Embedder: embedder})
	ctx := context.Background()
	first, err := client.AddMemory(ctx, memory.Input{Category: memory.CategoryPreference, Content: "偏好短答案"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AddMemory(ctx, memory.Input{Category: memory.CategoryVerifiedFact, Content: "猫喜欢晒太阳"}); err != nil {
		t.Fatal(err)
	}
	results, err := client.SearchMemories(ctx, "请尽量简洁", 5)
	if err != nil || len(results) != 1 || results[0].Item.ID != first.ID {
		t.Fatalf("semantic search = %#v, error=%v", results, err)
	}
	embedder.id = "test-v2"
	if results, err = client.SearchMemories(ctx, "请尽量简洁", 5); err != nil || len(results) != 0 {
		t.Fatalf("old vectors were used for a changed embedder: %#v, error=%v", results, err)
	}
	if err := client.ReindexMemories(ctx); err != nil {
		t.Fatal(err)
	}
	results, err = client.SearchMemories(ctx, "请尽量简洁", 5)
	if err != nil || len(results) != 1 || results[0].Item.ID != first.ID {
		t.Fatalf("search after reindex = %#v, error=%v", results, err)
	}
	if err := client.ClearMemories(ctx); err != nil {
		t.Fatal(err)
	}
	if items, err := client.ListMemories(ctx, 10); err != nil || len(items) != 0 {
		t.Fatalf("memories after clear = %#v, error=%v", items, err)
	}
}

func memorySubmissionResponse(items string) *publicmodel.ModelResponse {
	return hostToolCallResponse("memory-submit", "propose_workspace_memories", `{"items":`+items+`}`)
}

func TestAcceptedTaskExtractsAndSharesWorkspaceMemory(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "first_request", "Answer the current request.", "请用简洁中文回答"),
		hostTextResponse("收到，我会用简洁中文回答。"),
		memorySubmissionResponse(`[{
			"category":"preference","content":"Prefers concise Chinese answers.","importance":3,
			"basis":"请用简洁中文回答"
		}]`),
		criteriaToolResponse(AcceptanceTaskAnswer, "second_request", "Answer the current request.", "如何组织说明"),
		hostTextResponse("先给结论，再列出要点。"),
		memorySubmissionResponse(`[]`),
	}}
	client := newMemoryTestCore(t, provider, MemoryConfig{})
	ctx := context.Background()
	manual, err := client.AddMemory(ctx, memory.Input{Category: memory.CategoryConvention, Content: "Project names use lower camel case."})
	if err != nil {
		t.Fatal(err)
	}
	firstSession, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.RunTask(ctx, TaskRequest{SessionID: firstSession.ID, Input: mustSettingsTaskInput(t, "请用简洁中文回答")})
	if err != nil || first.Status != TaskCompleted {
		t.Fatalf("first accepted task = %#v, error=%v", first, err)
	}
	memories, err := client.ListMemories(ctx, 10)
	if err != nil || len(memories) != 2 {
		t.Fatalf("memories after extraction = %#v, error=%v", memories, err)
	}
	storedEvents, err := client.sessions.RuntimeEventsAfter(ctx, firstSession.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var sawMemoryStarted, sawMemorySaved bool
	for _, stored := range storedEvents {
		if !strings.Contains(string(stored.Payload), string(EventMemoryStarted)) && !strings.Contains(string(stored.Payload), string(EventMemorySaved)) {
			continue
		}
		var event Event
		if err := json.Unmarshal(stored.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(stored.Payload), "Prefers concise Chinese answers.") {
			t.Fatal("memory event persisted memory content")
		}
		if event.Kind == EventMemoryStarted {
			sawMemoryStarted = event.MemoryStatus == "started" && len(event.MemoryIDs) == 0
		}
		if event.Kind == EventMemorySaved {
			sawMemorySaved = event.MemoryStatus == "saved" && len(event.MemoryIDs) == 1
		}
	}
	if !sawMemoryStarted || !sawMemorySaved {
		t.Fatalf("memory lifecycle events: started=%t saved=%t", sawMemoryStarted, sawMemorySaved)
	}

	secondSession, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.RunTask(ctx, TaskRequest{SessionID: secondSession.ID, Input: mustSettingsTaskInput(t, "如何组织说明")})
	if err != nil || second.Status != TaskCompleted {
		t.Fatalf("second accepted task = %#v, error=%v", second, err)
	}
	provider.mu.Lock()
	requests := append([]publicmodel.ModelRequest(nil), provider.seenRequests...)
	provider.mu.Unlock()
	if len(requests) != 6 || !strings.Contains(requests[4].Instructions, "Prefers concise Chinese answers.") {
		t.Fatalf("second session did not receive workspace memory; request count=%d", len(requests))
	}
	for _, definition := range requests[2].Tools {
		if function, ok := definition.(publicmodel.FunctionTool); !ok || function.Name != "propose_workspace_memories" {
			t.Fatalf("memory extractor received an unexpected tool: %#v", definition)
		}
	}
	results, err := client.SearchMemories(ctx, "concise Chinese", 5)
	if err != nil || len(results) != 1 || results[0].Item.Content != "Prefers concise Chinese answers." {
		t.Fatalf("retrieved extracted memory = %#v, error=%v", results, err)
	}
	if err := client.DeleteSession(ctx, firstSession.ID); err != nil {
		t.Fatalf("delete source session: %v", err)
	}
	memories, err = client.ListMemories(ctx, 10)
	if err != nil || len(memories) != 1 || memories[0].ID != manual.ID {
		t.Fatalf("memories after deleting source session = %#v, error=%v", memories, err)
	}
}

type failingMemoryEmbedder struct{}

func (failingMemoryEmbedder) ID() string { return "failing-v1" }

func (failingMemoryEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedding service unavailable")
}

func TestMemoryEmbeddingFailureKeepsFullTextSearch(t *testing.T) {
	client := newMemoryTestCore(t, nil, MemoryConfig{Embedder: failingMemoryEmbedder{}})
	ctx := context.Background()
	if _, err := client.AddMemory(ctx, memory.Input{Category: memory.CategoryDecision, Content: "Keep workspace memory searchable."}); err != nil {
		t.Fatal(err)
	}
	results, err := client.SearchMemories(ctx, "workspace memory", 5)
	if err != nil || len(results) != 1 {
		t.Fatalf("full-text fallback results = %#v, error=%v", results, err)
	}
}
