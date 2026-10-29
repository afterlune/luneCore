package session

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

func TestFieldMutationsAcrossStoresPreserveIndependentOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := a.Create(t.Context(), Snapshot{ID: "s", Workspace: "w", Data: json.RawMessage(`{"history":["canonical"],"session_usage":{"requests":0}}`)}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, store := range []*Store{a, b} {
		wg.Go(func() {
			for i := 0; i < 20; i++ {
				if err := store.ModifyUsage(t.Context(), "s", "w", func(raw json.RawMessage) (json.RawMessage, error) {
					var usage struct {
						SessionUsage struct {
							Requests int `json:"requests"`
						} `json:"session_usage"`
					}
					if err := json.Unmarshal(raw, &usage); err != nil {
						return nil, err
					}
					usage.SessionUsage.Requests++
					return json.Marshal(usage)
				}); err != nil {
					t.Error(err)
					return
				}
				if err := store.UpdateSettings(t.Context(), "s", "w", json.RawMessage(`{"model":"test"}`)); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	saved, err := a.Get(t.Context(), "s", "w")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		History      []string `json:"history"`
		SessionUsage struct {
			Requests int `json:"requests"`
		} `json:"session_usage"`
	}
	if err := json.Unmarshal(saved.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionUsage.Requests != 40 || len(got.History) != 1 || got.History[0] != "canonical" {
		t.Fatalf("lost fields: %s", saved.Data)
	}
	if _, err := a.db.ExecContext(t.Context(), `DELETE FROM sessions WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateSettings(t.Context(), "s", "w", json.RawMessage(`{}`)); err == nil {
		t.Fatal("deleted session resurrected")
	}
}

func TestFieldUpdatesPreserveJSONTypesAndRollbackInvalidMutation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Create(t.Context(), Snapshot{ID: "s", Workspace: "w", Data: json.RawMessage(`{"history":["keep"],"other":{"untouched":true}}`)}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`"plain string"`, `true`, `3.5`, `null`, `[1,"two"]`, `{"nested":false}`} {
		if err := store.UpdateSettings(t.Context(), "s", "w", json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
		saved, err := store.Get(t.Context(), "s", "w")
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(saved.Data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["settings"]) != raw || string(fields["history"]) != `["keep"]` || string(fields["other"]) != `{"untouched":true}` {
			t.Fatalf("type or ownership changed: %s", saved.Data)
		}
	}
	before, err := store.Get(t.Context(), "s", "w")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ModifyUsage(t.Context(), "s", "w", func(json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{"session_usage":`), nil }); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	after, err := store.Get(t.Context(), "s", "w")
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Data) != string(after.Data) {
		t.Fatal("invalid mutation partially committed")
	}
}
