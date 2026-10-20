package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

func workspaceSandboxCacheDir(workspace string) (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache directory: %w", err)
	}
	path, err := workspaceSandboxCachePath(root, workspace)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", fmt.Errorf("create workspace sandbox cache: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace sandbox cache: %w", err)
	}
	return resolved, nil
}

func workspaceSandboxCachePath(cacheRoot, workspace string) (string, error) {
	canonical, err := canonicalWorkspace(workspace)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(canonical))
	return filepath.Join(cacheRoot, "lune", "sandbox", hex.EncodeToString(digest[:16])), nil
}
