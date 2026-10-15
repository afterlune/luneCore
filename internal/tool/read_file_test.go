package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestReadFileReturnsNumberedLinePages(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "lines.txt"), []byte("one\n二\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := (ReadFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"lines.txt","limit":2}`))
	if first.Err != nil || !strings.Contains(first.Output, "1 | one") || !strings.Contains(first.Output, "2 | 二") || !strings.Contains(first.Output, "next_start_line=3; more=true") {
		t.Fatalf("first read page = %#v", first)
	}
	second := (ReadFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"lines.txt","start_line":3,"limit":2}`))
	if second.Err != nil || !strings.Contains(second.Output, "3 | three") || !strings.Contains(second.Output, "4 | four") || !strings.Contains(second.Output, "more=false") {
		t.Fatalf("second read page = %#v", second)
	}
}

func TestReadFileBoundsLongLinesAndOutput(t *testing.T) {
	workspace := t.TempDir()
	content := strings.Repeat("x", maxToolRunes+100) + "\nnext\n"
	if err := os.WriteFile(filepath.Join(workspace, "long.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result := (ReadFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"long.txt"}`))
	if result.Err != nil {
		t.Fatalf("read long line: %v", result.Err)
	}
	if !strings.Contains(result.Output, "[line truncated; remainder omitted]") || !strings.Contains(result.Output, "next_start_line=2") {
		t.Fatalf("long-line output = %q", result.Output[len(result.Output)-100:])
	}
	if count := utf8.RuneCountInString(result.Output); count > maxToolRunes {
		t.Fatalf("read_file output is %d runes, limit %d", count, maxToolRunes)
	}
}

func TestReadFileRejectsInvalidLineRanges(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "file.txt"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{
		`{"path":"file.txt","start_line":0}`,
		`{"path":"file.txt","limit":0}`,
		`{"path":"file.txt","limit":501}`,
	} {
		if result := (ReadFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(args)); result.Err == nil {
			t.Errorf("read_file(%s) returned no validation error", args)
		}
	}
}
