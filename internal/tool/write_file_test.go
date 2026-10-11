package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileToolRespectsCanceledContext(t *testing.T) {
	workspace := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := (WriteFileTool{Workspace: workspace}).Run(ctx, []byte(`{"path":"created.txt","content":"new"}`))
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("write_file error = %v, want context.Canceled", result.Err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "created.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created.txt stat error = %v, want os.ErrNotExist", err)
	}
}
