package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileToolRespectsCanceledContext(t *testing.T) {
	workspace := t.TempDir()
	file := filepath.Join(workspace, "existing.txt")
	if err := os.WriteFile(file, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := (ReadFileTool{Workspace: workspace}).Run(ctx, []byte(`{"path":"existing.txt"}`))
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("read_file error = %v, want context.Canceled", result.Err)
	}
}
