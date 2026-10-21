package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestWriteEnabledMergesConcurrentManagerUpdates(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "mcp-enabled.json")
	first := &Manager{stateFile: stateFile}
	second := &Manager{stateFile: stateFile}

	results := make(chan error, 2)
	go func() { results <- first.writeEnabled("docs", true) }()
	go func() { results <- second.writeEnabled("search", true) }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("write enabled state: %v", err)
		}
	}

	if err := first.writeEnabled("docs", false); err != nil {
		t.Fatalf("disable docs: %v", err)
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Enabled []string `json:"enabled"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	sort.Strings(state.Enabled)
	if !reflect.DeepEqual(state.Enabled, []string{"search"}) {
		t.Fatalf("enabled servers after concurrent updates = %v, want [search]", state.Enabled)
	}
}
