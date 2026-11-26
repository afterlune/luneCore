package runtime

import (
	"context"
	"encoding/json"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestListSessionIndexReturnsMetadata(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{responses: []*publicmodel.ModelResponse{}})
	ctx := context.Background()
	saved, err := client.CreateSession(ctx, json.RawMessage(`[{"role":"user","content":"history"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetSessionTitle(ctx, saved.ID, "indexed session"); err != nil {
		t.Fatal(err)
	}
	if err := client.SetSessionArchived(ctx, saved.ID, true); err != nil {
		t.Fatal(err)
	}

	entries, err := client.ListSessionIndex(ctx, true)
	if err != nil {
		t.Fatalf("ListSessionIndex: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != saved.ID || entries[0].Title != "indexed session" || !entries[0].Archived || entries[0].UpdatedAt.IsZero() {
		t.Fatalf("session index = %+v", entries)
	}
	active, err := client.ListSessionIndex(ctx, false)
	if err != nil {
		t.Fatalf("ListSessionIndex active: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active session index = %+v, want empty", active)
	}
}
