package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/afterlune/luneCore/memory"
)

func TestWorkspaceMemoryIsolationAndSourceSessionCleanup(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	for _, snapshot := range []Snapshot{
		{ID: "session-a", Workspace: "workspace-a", Data: json.RawMessage(`{}`)},
		{ID: "session-b", Workspace: "workspace-b", Data: json.RawMessage(`{}`)},
	} {
		if err := store.Save(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		workspace string
		memory    memory.Item
	}{
		{"workspace-a", memory.Item{ID: "a-derived", Category: memory.CategoryDecision, Content: "The project keeps stable names.", Importance: 2, SourceSessionID: "session-a", SourceExecutionID: "execution-a"}},
		{"workspace-a", memory.Item{ID: "a-manual", Category: memory.CategoryConvention, Content: "The project keeps stable names.", Importance: 1}},
		{"workspace-b", memory.Item{ID: "b-derived", Category: memory.CategoryDecision, Content: "The project keeps stable names.", Importance: 2, SourceSessionID: "session-b", SourceExecutionID: "execution-b"}},
	} {
		if err := store.CreateMemory(ctx, item.workspace, item.memory, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	resultsA, err := store.SearchMemoriesText(ctx, "workspace-a", "stable names", 10)
	if err != nil || len(resultsA) != 2 {
		t.Fatalf("workspace A search = %#v, error=%v", resultsA, err)
	}
	resultsB, err := store.SearchMemoriesText(ctx, "workspace-b", "stable names", 10)
	if err != nil || len(resultsB) != 1 || resultsB[0].Item.ID != "b-derived" {
		t.Fatalf("workspace B search = %#v, error=%v", resultsB, err)
	}
	deleted, err := store.Delete(ctx, "session-a", "workspace-a")
	if err != nil || !deleted {
		t.Fatalf("delete source session = %v, error=%v", deleted, err)
	}
	resultsA, err = store.SearchMemoriesText(ctx, "workspace-a", "stable names", 10)
	if err != nil || len(resultsA) != 1 || resultsA[0].Item.ID != "a-manual" {
		t.Fatalf("workspace A after session deletion = %#v, error=%v", resultsA, err)
	}
	resultsB, err = store.SearchMemoriesText(ctx, "workspace-b", "stable names", 10)
	if err != nil || len(resultsB) != 1 || resultsB[0].Item.ID != "b-derived" {
		t.Fatalf("workspace B after other workspace deletion = %#v, error=%v", resultsB, err)
	}
}

func TestReplaceExtractedMemoriesIsIdempotent(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	item := memory.Item{
		ID: "memory-one", Category: memory.CategoryPreference, Content: "Prefer concise answers.", Importance: 3,
		SourceSessionID: "session-one", SourceExecutionID: "execution-one",
	}
	for range 2 {
		if err := store.ReplaceExtractedMemories(ctx, "workspace", "session-one", "execution-one", []memory.Item{item}, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.ListMemories(ctx, "workspace", 10)
	if err != nil || len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("extracted memories after repeated save = %#v, error=%v", items, err)
	}
}
