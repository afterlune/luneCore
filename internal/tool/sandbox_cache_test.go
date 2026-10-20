package tool

import (
	"path/filepath"
	"testing"
)

func TestWorkspaceSandboxCachePathIsStableAndWorkspaceScoped(t *testing.T) {
	root := t.TempDir()
	firstWorkspace := t.TempDir()
	secondWorkspace := t.TempDir()
	first, err := workspaceSandboxCachePath(root, firstWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := workspaceSandboxCachePath(root, firstWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := workspaceSandboxCachePath(root, secondWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if first != repeated {
		t.Fatalf("same workspace cache paths differ: %q and %q", first, repeated)
	}
	if first == second {
		t.Fatalf("different workspaces share cache path %q", first)
	}
	if filepath.Dir(first) != filepath.Join(root, "lune", "sandbox") {
		t.Fatalf("cache path %q is outside the sandbox cache root", first)
	}
}
