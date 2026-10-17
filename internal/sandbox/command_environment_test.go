package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandToolchainSearchPathsIncludesOnlyExistingConventionalDirectories(t *testing.T) {
	root := t.TempDir()
	for _, child := range []string{"bin", "Scripts"} {
		if err := os.Mkdir(filepath.Join(root, child), 0700); err != nil {
			t.Fatal(err)
		}
	}
	paths := commandToolchainSearchPaths([]string{root, root})
	if len(paths) != 3 {
		t.Fatalf("search paths = %#v, want root, bin, and Scripts", paths)
	}
	for _, path := range []string{root, filepath.Join(root, "bin"), filepath.Join(root, "Scripts")} {
		if !containsPath(paths, path) {
			t.Errorf("search paths %v do not include %q", paths, path)
		}
	}
	other := t.TempDir()
	separator := string(os.PathListSeparator)
	joined := appendSearchPaths(root+separator+other, []string{other, root}, separator)
	if len(splitSearchPaths(joined, separator)) != 2 {
		t.Fatalf("deduplicated search path = %q", joined)
	}
}

func splitSearchPaths(path, separator string) []string {
	var paths []string
	for _, item := range strings.Split(path, separator) {
		if item != "" {
			paths = append(paths, item)
		}
	}
	return paths
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if filepath.Clean(path) == filepath.Clean(want) {
			return true
		}
	}
	return false
}
