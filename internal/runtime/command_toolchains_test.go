package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalizeCommandToolchainPaths(t *testing.T) {
	path := t.TempDir()
	input := []string{path, filepath.Clean(path)}
	got, err := normalizeCommandToolchainPaths(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("normalized paths = %#v, want one path", got)
	}
	if runtime.GOOS != "windows" && filepath.Clean(got[0]) != filepath.Clean(path) {
		t.Fatalf("normalized path = %q, want %q", got[0], path)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]string{{" "}, {filepath.Join(t.TempDir(), "missing")}, {file}} {
		if _, err := normalizeCommandToolchainPaths(invalid); err == nil {
			t.Fatalf("invalid toolchain paths were accepted: %#v", invalid)
		}
	}
}

func TestCoreCommandToolchainPaths(t *testing.T) {
	c := &Core{}
	path := t.TempDir()
	if err := c.SetCommandToolchainPaths([]string{path}); err != nil {
		t.Fatal(err)
	}
	got := c.CommandToolchainPaths()
	if len(got) != 1 {
		t.Fatalf("CommandToolchainPaths = %v, want 1 entry", got)
	}
}
