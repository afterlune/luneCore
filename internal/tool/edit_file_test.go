package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEditFileToolRespectsCanceledContext(t *testing.T) {
	workspace := t.TempDir()
	file := filepath.Join(workspace, "existing.txt")
	if err := os.WriteFile(file, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := (EditFileTool{Workspace: workspace}).Run(ctx, []byte(`{"path":"existing.txt","old_text":"original","new_text":"changed"}`))
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("edit_file error = %v, want context.Canceled", result.Err)
	}
	contents, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "original" {
		t.Fatalf("existing.txt = %q, want original", contents)
	}
}
