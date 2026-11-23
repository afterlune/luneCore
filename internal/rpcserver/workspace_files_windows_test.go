package rpcserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorkspaceFilesRejectWindowsJunction(t *testing.T) {
	root, external := t.TempDir(), t.TempDir()
	link := filepath.Join(root, "linked-dir")
	if err := os.WriteFile(filepath.Join(external, "external.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, external).CombinedOutput(); err != nil {
		t.Skipf("cannot create a temporary directory junction: %v: %s", err, output)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		path      string
		directory bool
	}{{"linked-dir", true}, {"linked-dir/external.txt", false}} {
		file, err := openWorkspaceEntry(root, info, target.path, target.directory)
		if err == nil {
			file.Close()
			t.Errorf("opened junction target %q", target.path)
		}
	}
}
