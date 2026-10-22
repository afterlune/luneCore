package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	projectconfig "github.com/afterlune/luneCore/internal/config"
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

func TestEnabledStateRequiresRestoreOptIn(t *testing.T) {
	stateDir := t.TempDir()
	stateFile := filepath.Join(stateDir, "mcp-enabled.json")
	if err := os.WriteFile(stateFile, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	newManager := func(restore bool) *Manager {
		t.Helper()
		registry := testRegistry(t)
		manager, err := NewManager(Options{
			Workspace: t.TempDir(), StateDir: stateDir, RestoreEnabled: restore,
			Registry: registry,
			Servers: map[string]projectconfig.MCPServerConfig{
				"docs": {Transport: projectconfig.MCPTransportStreamableHTTP, URL: "https://docs.example.test/mcp"},
			},
		})
		if err != nil {
			t.Fatalf("NewManager(restore=%v): %v", restore, err)
		}
		return manager
	}

	defaultManager := newManager(false)
	if statuses := defaultManager.List(); len(statuses) != 1 || statuses[0].Enabled {
		t.Fatalf("default manager status = %#v, want configured but disabled", statuses)
	}
	if err := defaultManager.Disable("docs"); err != nil {
		t.Fatalf("default manager Disable: %v", err)
	}
	if data, err := os.ReadFile(stateFile); err != nil || string(data) != "not json" {
		t.Fatalf("default manager changed state file: data=%q err=%v", data, err)
	}

	if err := os.WriteFile(stateFile, []byte(`{"enabled":["docs"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	defaultManager = newManager(false)
	if statuses := defaultManager.List(); len(statuses) != 1 || statuses[0].Enabled {
		t.Fatalf("default manager restored enabled state: %#v", statuses)
	}

	optInManager := newManager(true)
	if statuses := optInManager.List(); len(statuses) != 1 || !statuses[0].Enabled {
		t.Fatalf("opt-in manager did not restore enabled state: %#v", statuses)
	}
}
