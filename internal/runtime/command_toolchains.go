package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func normalizeCommandToolchainPaths(paths []string) ([]string, error) {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("command toolchain path must not be empty")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve command toolchain path %q: %w", path, err)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve command toolchain path %q: %w", path, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("inspect command toolchain path %q: %w", path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("command toolchain path %q is not a directory", path)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return nil, fmt.Errorf("resolve command toolchain path %q: %w", path, err)
		}
		key := filepath.Clean(resolved)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, resolved)
	}
	return result, nil
}
