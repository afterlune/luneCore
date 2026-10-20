//go:build linux && (amd64 || arm64)

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxEnvironmentUsesPersistentCachesAndExistingGoTempDir(t *testing.T) {
	tempDir := t.TempDir()
	cacheDir := t.TempDir()
	if err := prepareSandboxDirectories(tempDir, cacheDir); err != nil {
		t.Fatal(err)
	}
	environment := sandboxEnvironment(tempDir, cacheDir, tempDir, nil)
	values := make(map[string]string, len(environment))
	for _, value := range environment {
		key, value, ok := strings.Cut(value, "=")
		if ok {
			values[key] = value
		}
	}
	for key, want := range map[string]string{
		"GOCACHE":        filepath.Join(cacheDir, "go-build"),
		"GOMODCACHE":     filepath.Join(cacheDir, "go-mod"),
		"XDG_CACHE_HOME": filepath.Join(cacheDir, "xdg"),
		"GOTMPDIR":       filepath.Join(tempDir, "go-tmp"),
		"TMPDIR":         tempDir,
	} {
		if values[key] != want {
			t.Errorf("%s=%q, want %q", key, values[key], want)
		}
		if key != "TMPDIR" {
			if info, err := os.Stat(want); err != nil || !info.IsDir() {
				t.Errorf("%s directory %q is unavailable: info=%v error=%v", key, want, info, err)
			}
		}
	}
}
