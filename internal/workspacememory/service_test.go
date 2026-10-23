package workspacememory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/session"
	"github.com/afterlune/luneCore/memory"
	publicmodel "github.com/afterlune/luneCore/model"
)

func TestServiceMemorySummaryAndStopwords(t *testing.T) {
	ctx := context.Background()
	store, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	svc := New(store, t.TempDir(), nil, nil)
	for _, item := range []memory.Input{
		{Category: memory.CategoryPreference, Content: "Prefer concise Chinese answers.", Importance: 3},
		{Category: memory.CategoryDecision, Content: "Token budget estimates remain separate from provider measured usage.", Importance: 3},
		{Category: memory.CategoryVerifiedFact, Content: "An unrelated deployment fact that should not be injected.", Importance: 1},
	} {
		if _, err := svc.Add(ctx, item); err != nil {
			t.Fatal(err)
		}
	}

	taskInput, err := json.Marshal(publicmodel.NewResponseInputText("How does the token budget estimate relate to measured usage?"))
	if err != nil {
		t.Fatal(err)
	}

	summary, err := svc.Summary(ctx, taskInput)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "Prefer concise Chinese answers") || !strings.Contains(summary, "Token budget estimates") {
		t.Fatalf("summary omitted pinned or relevant memory: %s", summary)
	}
	if strings.Contains(summary, "unrelated deployment fact") {
		t.Fatalf("summary injected an unrelated memory: %s", summary)
	}

	stopwordInput, err := json.Marshal(publicmodel.NewResponseInputText("What is it?"))
	if err != nil {
		t.Fatal(err)
	}
	if query := MemoryQueryForTaskInput(stopwordInput); query != "" {
		t.Fatalf("stopword-only task produced memory query %q", query)
	}
}

func TestServiceMemoryCRUD(t *testing.T) {
	ctx := context.Background()
	store, err := session.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	svc := New(store, t.TempDir(), nil, nil)
	item, err := svc.Add(ctx, memory.Input{
		Category:   memory.CategoryPreference,
		Content:    "Write clean code.",
		Importance: 2,
	})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	items, err := svc.List(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("List failed: %v, items: %v", err, items)
	}

	item.Content = "Write exceptionally clean code."
	updated, err := svc.Update(ctx, item.ID, memory.Input{
		Category:   item.Category,
		Content:    item.Content,
		Importance: item.Importance,
	})
	if err != nil || updated.Content != item.Content {
		t.Fatalf("Update failed: %v", err)
	}

	if err := svc.Delete(ctx, item.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	items, err = svc.List(ctx, 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("List after delete failed: %v, items: %v", err, items)
	}
}
