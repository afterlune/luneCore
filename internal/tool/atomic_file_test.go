package tool

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFileLeavesOriginalWhenStagingFails(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "existing.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	wantErr := errors.New("injected write failure")
	err = replaceFile(context.Background(), root, "existing.txt", 0o600, true, func(destination io.Writer) error {
		if _, err := io.WriteString(destination, "partial"); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("replaceFile error = %v, want injected error", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "original" {
		t.Fatalf("existing file = %q, error = %v; want original contents", contents, err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "existing.txt" {
		t.Fatalf("staging files were not cleaned up: %#v", entries)
	}
}

func TestEditLargeFileMatchesAcrossReadChunks(t *testing.T) {
	workspace := t.TempDir()
	filePath := filepath.Join(workspace, "large.txt")
	needle := "cross-chunk-needle"
	content := make([]byte, maxDiffSourceBytes+64*1024)
	for index := range content {
		content[index] = 'a'
	}
	matchAt := 64*1024 - 5
	copy(content[matchAt:], needle)
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	args := `{"path":"large.txt","old_text":"` + needle + `","new_text":"replacement"}`
	result := (EditFileTool{Workspace: workspace}).Run(context.Background(), []byte(args))
	if result.Err != nil || result.Output != "Updated large.txt" || result.Details != nil {
		t.Fatalf("large edit = %#v", result)
	}
	updated, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated[matchAt:matchAt+len("replacement")]) != "replacement" {
		t.Fatalf("replacement at chunk boundary = %q", updated[matchAt:matchAt+len("replacement")])
	}
	if len(updated) != len(content)-len(needle)+len("replacement") {
		t.Fatalf("updated file length = %d, want %d", len(updated), len(content)-len(needle)+len("replacement"))
	}
}

func TestLargeEditWithNonUniqueTextLeavesOriginal(t *testing.T) {
	workspace := t.TempDir()
	filePath := filepath.Join(workspace, "large.txt")
	content := make([]byte, maxDiffSourceBytes+10)
	for index := range content {
		content[index] = 'a'
	}
	copy(content[100:], "needle")
	copy(content[200:], "needle")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	result := (EditFileTool{Workspace: workspace}).Run(context.Background(), []byte(`{"path":"large.txt","old_text":"needle","new_text":"changed"}`))
	if result.Err == nil {
		t.Fatal("edit with multiple matches succeeded")
	}
	updated, err := os.ReadFile(filePath)
	if err != nil || string(updated) != string(content) {
		t.Fatalf("file changed after rejected edit; error = %v", err)
	}
}
