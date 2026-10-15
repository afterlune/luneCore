package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func makeExplorationWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"a.txt":                  "hello world\n",
		".hidden":                "hidden\n",
		"src/main.go":            "package main\nfunc target() {}\n",
		"src/nested/worker.go":   "package nested\n// target invocation\n",
		".git/secret.txt":        "target must not be searched\n",
		"node_modules/noise.txt": "noise\n",
	}
	for name, contents := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{'x', 0, 't', 'a', 'r', 'g', 'e', 't'}, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestListDirToolListsSortedEntriesAndWorkspaceRoot(t *testing.T) {
	root := makeExplorationWorkspace(t)
	result := (ListDirTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{}`))
	if result.Err != nil {
		t.Fatalf("list_dir: %v", result.Err)
	}
	for _, name := range []string{".git", ".hidden", "a.txt", "src"} {
		if !strings.Contains(result.Output, name) {
			t.Errorf("list_dir output %q does not contain %q", result.Output, name)
		}
	}
	if strings.Index(result.Output, ".git") > strings.Index(result.Output, ".hidden") || strings.Index(result.Output, ".hidden") > strings.Index(result.Output, "a.txt") {
		t.Errorf("entries are not sorted: %q", result.Output)
	}
	if !strings.Contains(result.Output, "directory\tsrc") || !strings.Contains(result.Output, "file\ta.txt") {
		t.Errorf("entry types are missing: %q", result.Output)
	}

	nested := (ListDirTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"path":"src"}`))
	if nested.Err != nil || !strings.Contains(nested.Output, "directory\tnested") || !strings.Contains(nested.Output, "file\tmain.go") {
		t.Fatalf("list_dir src = %#v", nested)
	}
	file := (ListDirTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"path":"a.txt"}`))
	if file.Err == nil || file.Err.Error() != "path is not a directory" {
		t.Fatalf("list_dir file error = %v, want a directory error", file.Err)
	}
}

func TestGlobToolSupportsRecursiveAndHiddenMatches(t *testing.T) {
	root := makeExplorationWorkspace(t)
	result := (GlobTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`))
	if result.Err != nil {
		t.Fatalf("glob: %v", result.Err)
	}
	for _, match := range []string{"src/main.go", "src/nested/worker.go"} {
		if !strings.Contains(result.Output, match) {
			t.Errorf("glob output %q does not contain %q", result.Output, match)
		}
	}
	if strings.Contains(result.Output, ".git") {
		t.Errorf("glob searched .git: %q", result.Output)
	}

	hidden := (GlobTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"**/.*"}`))
	if hidden.Err != nil || !strings.Contains(hidden.Output, ".hidden") {
		t.Fatalf("glob hidden result = %#v", hidden)
	}
	invalid := (GlobTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"["}`))
	if invalid.Err == nil || !strings.Contains(invalid.Err.Error(), "invalid glob pattern") {
		t.Fatalf("invalid glob error = %v", invalid.Err)
	}
	outside := (GlobTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"../*"}`))
	if outside.Err == nil || !strings.Contains(outside.Err.Error(), "escapes the workspace") {
		t.Fatalf("outside glob error = %v", outside.Err)
	}
}

func TestGrepToolSearchesTextLinesAndSkipsGitAndBinary(t *testing.T) {
	root := makeExplorationWorkspace(t)
	result := (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"target","path":"."}`))
	if result.Err != nil {
		t.Fatalf("grep: %v", result.Err)
	}
	for _, want := range []string{"src/main.go:2:func target() {}", "src/nested/worker.go:2:// target invocation"} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("grep output %q does not contain %q", result.Output, want)
		}
	}
	if strings.Contains(result.Output, ".git") || strings.Contains(result.Output, "binary.bin") {
		t.Errorf("grep included excluded file: %q", result.Output)
	}

	caseSensitive := (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"TARGET","path":"src"}`))
	if caseSensitive.Err != nil || caseSensitive.Output != "No matches.\n[end of results]" {
		t.Fatalf("case-sensitive grep = %#v", caseSensitive)
	}
	invalid := (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"[","path":"."}`))
	if invalid.Err == nil || !strings.Contains(invalid.Err.Error(), "invalid grep pattern") {
		t.Fatalf("invalid regex error = %v", invalid.Err)
	}
	outside := (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"x","path":"../outside"}`))
	if outside.Err == nil || !strings.Contains(outside.Err.Error(), "escapes the workspace") {
		t.Fatalf("outside grep error = %v", outside.Err)
	}
	missing := (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"x","path":"missing"}`))
	if missing.Err == nil || !strings.Contains(missing.Err.Error(), "does not exist") {
		t.Fatalf("missing grep path error = %v", missing.Err)
	}
}

func TestReadFileDirectoryErrorIsDistinctFromEscape(t *testing.T) {
	root := makeExplorationWorkspace(t)
	directory := (ReadFileTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"path":"src"}`))
	if directory.Err == nil || !strings.Contains(directory.Err.Error(), "refers to a directory") {
		t.Fatalf("directory read error = %v", directory.Err)
	}
	escape := (ReadFileTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"path":"../outside"}`))
	if escape.Err == nil || escape.Err.Error() != "path escapes the workspace" {
		t.Fatalf("escape read error = %v", escape.Err)
	}
}

func TestExplorationToolsRespectCanceledContext(t *testing.T) {
	root := makeExplorationWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, run := range map[string]func() Result{
		"list_dir": func() Result { return (ListDirTool{Workspace: root}).Run(ctx, json.RawMessage(`{}`)) },
		"glob":     func() Result { return (GlobTool{Workspace: root}).Run(ctx, json.RawMessage(`{"pattern":"**/*"}`)) },
		"grep":     func() Result { return (GrepTool{Workspace: root}).Run(ctx, json.RawMessage(`{"pattern":"x"}`)) },
	} {
		if result := run(); !errors.Is(result.Err, context.Canceled) {
			t.Errorf("%s error = %v, want context.Canceled", name, result.Err)
		}
	}
}

func TestGrepToolPaginatesMatches(t *testing.T) {
	root := t.TempDir()
	lines := strings.Repeat("match\n", maxPageSize+10)
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	result := (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"match","limit":50}`))
	if result.Err != nil {
		t.Fatalf("grep: %v", result.Err)
	}
	if !strings.Contains(result.Output, "many.txt:1:match") || !strings.Contains(result.Output, "next_offset=50") {
		t.Fatalf("grep first page = %q", result.Output[len(result.Output)-100:])
	}
	result = (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"match","offset":50,"limit":50}`))
	if result.Err != nil || !strings.Contains(result.Output, "many.txt:51:match") || !strings.Contains(result.Output, "next_offset=100") {
		t.Fatalf("grep second page = %#v", result)
	}
}

func TestGrepToolBoundsOutputSize(t *testing.T) {
	root := t.TempDir()
	line := strings.Repeat("needle", maxToolRunes)
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	result := (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if result.Err != nil {
		t.Fatalf("grep: %v", result.Err)
	}
	if !strings.Contains(result.Output, "[match line truncated]") {
		t.Fatalf("grep output has no line-truncation marker")
	}
	if count := utf8.RuneCountInString(result.Output); count > maxToolRunes {
		t.Fatalf("grep output is %d runes, limit %d", count, maxToolRunes)
	}
}

func TestExplorationToolsAreAllowedInReadOnlyPermissionMode(t *testing.T) {
	root := makeExplorationWorkspace(t)
	registry, err := NewDefaultRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.SetPermissionMode(PermissionModeReadOnly); err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		name string
		args string
	}{
		{"list_dir", `{}`},
		{"glob", `{"pattern":"**/*.go"}`},
		{"grep", `{"pattern":"target"}`},
	}
	for _, call := range calls {
		if result := registry.Run(context.Background(), call.name, json.RawMessage(call.args)); result.Err != nil {
			t.Errorf("%s was rejected in read-only mode: %v", call.name, result.Err)
		}
	}
	if result := registry.Run(context.Background(), "write_file", json.RawMessage(`{"path":"blocked.txt","content":"x"}`)); result.Err == nil {
		t.Fatal("write_file was allowed in read-only mode")
	}
}

func TestExplorationToolsDoNotFollowSymlinksOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	result := (ListDirTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"path":"outside-link"}`))
	if result.Err == nil || !strings.Contains(result.Err.Error(), "resolves outside the workspace") {
		t.Fatalf("outside symlink result = %#v", result)
	}
}

func TestGlobAndGrepMarkScanLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxWalkEntries+1; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%05d.txt", i)), []byte("needle\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	glob := (GlobTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`))
	if glob.Err != nil || !strings.Contains(glob.Output, "[scan limit reached") {
		t.Fatalf("glob scan limit result = %#v", glob)
	}
	grep := (GrepTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"not-present"}`))
	if grep.Err != nil || !strings.Contains(grep.Output, "[scan limit reached") {
		t.Fatalf("grep scan limit result = %#v", grep)
	}
}

func TestListDirPaginatesSortedEntries(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 650; index++ {
		name := filepath.Join(root, fmt.Sprintf("entry-%04d.txt", index))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := (ListDirTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"limit":200}`))
	if first.Err != nil || !strings.Contains(first.Output, "file\tentry-0000.txt") || !strings.Contains(first.Output, "next_offset=200") {
		t.Fatalf("list_dir first page = %#v", first)
	}
	second := (ListDirTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"offset":200,"limit":200}`))
	if second.Err != nil || !strings.Contains(second.Output, "file\tentry-0200.txt") || strings.Contains(second.Output, "entry-0199.txt") {
		t.Fatalf("list_dir second page = %#v", second)
	}
}

func TestGlobPaginatesMatches(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 250; index++ {
		name := filepath.Join(root, fmt.Sprintf("entry-%04d.go", index))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := (GlobTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"*.go","limit":100}`))
	if first.Err != nil || !strings.Contains(first.Output, "entry-0000.go") || !strings.Contains(first.Output, "next_offset=100") {
		t.Fatalf("glob first page = %#v", first)
	}
	second := (GlobTool{Workspace: root}).Run(context.Background(), json.RawMessage(`{"pattern":"*.go","offset":100,"limit":100}`))
	if second.Err != nil || !strings.Contains(second.Output, "entry-0100.go") || strings.Contains(second.Output, "entry-0099.go") {
		t.Fatalf("glob second page = %#v", second)
	}
}
