package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileChangeCountsSeparateEditsAndFinalNewline(t *testing.T) {
	for _, tc := range []struct {
		before, after  string
		added, removed int
	}{
		{"", "hello\n", 1, 0},
		{"one\ntwo\nthree\nfour\nfive\n", "ONE\ntwo\nthree\nfour\nFIVE\n", 2, 2},
		{"one", "one\n", 1, 1},
	} {
		change := fileChange("file", tc.before, tc.after).Changes[0]
		if change.Added != tc.added || change.Removed != tc.removed {
			t.Fatalf("change=%+v", change)
		}
	}
}

func TestEditAndWriteReturnActualChangesOnlyAfterSuccess(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(path, []byte("first\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result := (EditFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"file.txt","old_text":"second","new_text":"changed"}`))
	if result.Err != nil || result.Details == nil || result.Details.Changes[0].Added != 1 || result.Details.Changes[0].Removed != 1 {
		t.Fatalf("edit=%+v", result)
	}
	result = (EditFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"file.txt","old_text":"missing","new_text":"changed"}`))
	if result.Err == nil || result.Details != nil {
		t.Fatal("failed edit reported changes")
	}
	result = (WriteFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"file.txt","content":"new\n"}`))
	if result.Err != nil || result.Details == nil || result.Details.Changes[0].Removed != 2 {
		t.Fatalf("write=%+v", result)
	}
}

func TestDiffPreviewIsBounded(t *testing.T) {
	change := fileChange("file", "", strings.Repeat("a\n", 1000)).Changes[0]
	if !change.Truncated || len(change.Lines) > 512 || change.Added != 1000 {
		t.Fatalf("change counts=%d lines=%d truncated=%v", change.Added, len(change.Lines), change.Truncated)
	}
}

func TestLargeWriteSucceedsWithoutInventingAPreview(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "large.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxDiffSourceBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	result := (WriteFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"large.txt","content":"new"}`))
	if result.Err != nil || result.Details != nil {
		t.Fatalf("large-file write=%+v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatal("large file was not replaced")
	}
}
