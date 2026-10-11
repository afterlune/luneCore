//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGrepSkipsSpecialFilesBeforeOpeningThem(t *testing.T) {
	workspace := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workspace, "blocked.pipe"), 0o600); err != nil {
		t.Fatalf("create fifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("needle in a regular file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan Result, 1)
	go func() {
		result <- (GrepTool{Workspace: workspace}).Run(ctx, json.RawMessage(`{"pattern":"needle"}`))
	}()
	select {
	case got := <-result:
		if got.Err != nil || !strings.Contains(got.Output, "source.txt") || strings.Contains(got.Output, "blocked.pipe") {
			t.Fatalf("grep with fifo = %#v", got)
		}
	case <-ctx.Done():
		t.Fatal("grep blocked while opening a FIFO in the workspace")
	}
}
