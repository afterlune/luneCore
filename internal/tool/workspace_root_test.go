package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceRootHandleRejectsSymlinkEscapeAddedAfterOpen(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := openWorkspaceRoot(workspace)
	if err != nil {
		t.Fatalf("open workspace root: %v", err)
	}
	defer root.Close()
	if err := os.Symlink(outside, filepath.Join(workspace, "late-link")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	if _, err := root.Open(filepath.Join("late-link", "secret.txt")); err == nil {
		t.Fatal("workspace root opened a symlink target outside its anchored directory")
	}
	if _, err := root.OpenFile(filepath.Join("late-link", "created.txt"), os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		t.Fatal("workspace root created a file through an outside symlink")
	}
	if contents, err := os.ReadFile(secret); err != nil || string(contents) != "secret" {
		t.Fatalf("outside file changed: contents=%q err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file was created: err=%v", err)
	}
	glob := (GlobTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"pattern":"late-link/**"}`))
	if glob.Err != nil || strings.Contains(glob.Output, "secret.txt") {
		t.Fatalf("glob followed an outside symlink: %#v", glob)
	}
	grep := (GrepTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"pattern":"secret"}`))
	if grep.Err != nil || strings.Contains(grep.Output, "secret") {
		t.Fatalf("grep followed an outside symlink: %#v", grep)
	}
}

func TestWorkspaceRootHandleStaysOnOpenedDirectoryAfterRename(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "secret.txt"), []byte("original workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := openWorkspaceRoot(workspace)
	if err != nil {
		t.Fatalf("open workspace root: %v", err)
	}
	movedWorkspace := workspace + "-moved"
	if err := os.Rename(workspace, movedWorkspace); err != nil {
		// Windows roots keep the opened directory from being renamed. Confirm
		// the original path still identifies the same directory in that case.
		openedInfo, openedErr := root.Stat(".")
		pathInfo, pathErr := os.Stat(workspace)
		root.Close()
		if openedErr != nil || pathErr != nil || !os.SameFile(openedInfo, pathInfo) {
			t.Fatalf("root no longer matches workspace after failed rename: opened=%v path=%v rename=%v", openedErr, pathErr, err)
		}
		return
	}
	defer root.Close()
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatalf("create replacement workspace path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "secret.txt"), []byte("replacement workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	contents, err := root.ReadFile("secret.txt")
	if err != nil || string(contents) != "original workspace" {
		t.Fatalf("root read after path replacement = %q, error=%v", contents, err)
	}
	if err := root.WriteFile("created.txt", []byte("pinned"), 0o600); err != nil {
		t.Fatalf("write through pinned root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("write escaped to the replacement directory: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(movedWorkspace, "created.txt")); err != nil || string(contents) != "pinned" {
		t.Fatalf("write did not stay in the opened directory: contents=%q err=%v", contents, err)
	}
}

func TestWorkspaceFileToolsRejectOutsideSymlinkTargets(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "outside-link")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	for name, result := range map[string]Result{
		"read":  (ReadFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"outside-link/secret.txt"}`)),
		"edit":  (EditFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"outside-link/secret.txt","old_text":"secret","new_text":"changed"}`)),
		"write": (WriteFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"outside-link/created.txt","content":"changed"}`)),
		"list":  (ListDirTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"outside-link"}`)),
		"grep":  (GrepTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"outside-link/secret.txt","pattern":"secret"}`)),
	} {
		if result.Err == nil || !strings.Contains(result.Err.Error(), "outside the workspace") {
			t.Errorf("%s result = %#v; want an outside-workspace error", name, result)
		}
	}
	if contents, err := os.ReadFile(secret); err != nil || string(contents) != "secret" {
		t.Fatalf("outside file changed: contents=%q err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file was created: err=%v", err)
	}
}

func TestWorkspaceFileToolsKeepFollowingSymlinksInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, "source.txt"), filepath.Join(workspace, "alias.txt")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "folder", "entry.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, "folder"), filepath.Join(workspace, "folder-alias")); err != nil {
		t.Skipf("directory symlinks are unavailable: %v", err)
	}

	read := (ReadFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"alias.txt"}`))
	if read.Err != nil || read.Output != "first" {
		t.Fatalf("read through an in-workspace symlink = %#v", read)
	}
	write := (WriteFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"alias.txt","content":"second"}`))
	if write.Err != nil {
		t.Fatalf("write through an in-workspace symlink: %v", write.Err)
	}
	edit := (EditFileTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"alias.txt","old_text":"second","new_text":"third"}`))
	if edit.Err != nil {
		t.Fatalf("edit through an in-workspace symlink: %v", edit.Err)
	}
	if contents, err := os.ReadFile(filepath.Join(workspace, "source.txt")); err != nil || string(contents) != "third" {
		t.Fatalf("symlink target contents = %q, error=%v", contents, err)
	}
	listed := (ListDirTool{Workspace: workspace}).Run(context.Background(), json.RawMessage(`{"path":"folder-alias"}`))
	if listed.Err != nil || !strings.Contains(listed.Output, "entry.txt") {
		t.Fatalf("list through an in-workspace directory symlink = %#v", listed)
	}
}

func TestDefaultRegistryRejectsWorkspacePathReplacement(t *testing.T) {
	workspace := t.TempDir()
	registry, err := NewDefaultRegistry(workspace)
	if err != nil {
		t.Fatalf("NewDefaultRegistry: %v", err)
	}
	defer registry.Close()
	movedWorkspace := workspace + "-moved"
	if err := os.Rename(workspace, movedWorkspace); err != nil {
		openedInfo, openedErr := registry.workspaceRoot.root.Stat(".")
		pathInfo, pathErr := os.Stat(workspace)
		if openedErr != nil || pathErr != nil || !os.SameFile(openedInfo, pathInfo) {
			t.Fatalf("registry root lost its workspace identity: opened=%v path=%v rename=%v", openedErr, pathErr, err)
		}
		return
	}
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatalf("create replacement workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "secret.txt"), []byte("replacement secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := registry.Run(context.Background(), "read_file", json.RawMessage(`{"path":"secret.txt"}`))
	if result.Err == nil {
		t.Fatalf("read after workspace path replacement = %#v", result)
	}
	if strings.Contains(result.Output, "replacement secret") {
		t.Fatalf("read returned content from replacement workspace: %q", result.Output)
	}
}
