package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func commandToolchainSearchPaths(roots []string) []string {
	seen := make(map[string]struct{}, len(roots)*4)
	paths := make([]string, 0, len(roots)*4)
	add := func(path string) {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return
		}
		key := filepath.Clean(path)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		paths = append(paths, path)
	}
	for _, root := range roots {
		add(root)
		for _, child := range []string{"bin", "cmd", "Scripts"} {
			add(filepath.Join(root, child))
		}
	}
	return paths
}

func appendSearchPaths(path string, additions []string, separator string) string {
	parts := make([]string, 0, 1+len(additions))
	seen := make(map[string]struct{}, 1+len(additions))
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := filepath.Clean(value)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		parts = append(parts, value)
	}
	for _, value := range strings.Split(path, separator) {
		add(value)
	}
	for _, value := range additions {
		add(value)
	}
	return strings.Join(parts, separator)
}
