package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCoreReleasesPinnedWorkspaceRoot(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		defer server.Close()
		workspace, stateDir := t.TempDir(), t.TempDir()
		writeSettingsConfig(t, workspace, server.URL)
		client, err := New(Config{Workspace: workspace, StateDir: stateDir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := client.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		assertWorkspaceCanMove(t, workspace)
	})

	t.Run("startup failure", func(t *testing.T) {
		workspace, stateDir := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte(`{"unknown":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{Workspace: workspace, StateDir: stateDir}); err == nil {
			t.Fatal("New accepted an invalid workspace config")
		}
		assertWorkspaceCanMove(t, workspace)
	})
}

func assertWorkspaceCanMove(t *testing.T, workspace string) {
	t.Helper()
	moved := workspace + "-moved"
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatalf("workspace root remained pinned after Core shutdown: %v", err)
	}
}
