package tool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// canonicalWorkspace resolves a workspace path to an absolute, symlink-free
// directory. Shared by every tool that touches the file system.
func canonicalWorkspace(workspace string) (string, error) {
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get current directory: %w", err)
		}
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", workspace)
	}
	return resolved, nil
}

// workspaceIdentity keeps built-in tools anchored to the directory selected
// when the registry is created.
type workspaceIdentity struct {
	root     *os.Root
	close    sync.Once
	closeErr error
}

func (identity *workspaceIdentity) closeRoot() error {
	if identity == nil || identity.root == nil {
		return nil
	}
	identity.close.Do(func() { identity.closeErr = identity.root.Close() })
	return identity.closeErr
}

// resolvedPath verifies that the path used by the sandbox still refers to the
// directory pinned by the registry's root handle.
func (identity *workspaceIdentity) resolvedPath() (string, error) {
	if identity == nil || identity.root == nil {
		return "", errors.New("workspace root is unavailable")
	}
	pinned, err := identity.root.Stat(".")
	if err != nil {
		return "", fmt.Errorf("inspect pinned workspace: %w", err)
	}
	path := identity.root.Name()
	current, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect workspace path: %w", err)
	}
	if !os.SameFile(pinned, current) {
		return "", errors.New("workspace path no longer refers to the pinned workspace")
	}
	return path, nil
}

// openWorkspaceRoot pins file operations to a resolved workspace directory.
func openWorkspaceRoot(workspace string) (*os.Root, error) {
	path, err := canonicalWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("inspect opened workspace root: %w", err)
	}
	if !openedInfo.IsDir() {
		root.Close()
		return nil, errors.New("opened workspace root is not a directory")
	}
	return root, nil
}

func workspaceRootForTool(workspace string, identity *workspaceIdentity) (*os.Root, func(), error) {
	if identity != nil && identity.root != nil {
		return identity.root, func() {}, nil
	}
	root, err := openWorkspaceRoot(workspace)
	if err != nil {
		return nil, nil, err
	}
	return root, func() { _ = root.Close() }, nil
}

// rootRelativePath converts a validated absolute workspace path to the name
// callers must use with the pinned os.Root handle.
func rootRelativePath(root *os.Root, absolutePath string) (string, error) {
	rootPath := root.Name()
	if !pathWithin(rootPath, absolutePath) {
		return "", errors.New("path resolves outside the workspace")
	}
	relative, err := filepath.Rel(rootPath, absolutePath)
	if err != nil {
		return "", fmt.Errorf("make workspace-relative path: %w", err)
	}
	return relative, nil
}

// resolveWorkspacePath validates a relative tool argument and returns its
// resolved path. It is a preflight check only; callers must perform the actual
// I/O through an os.Root handle to close the symlink replacement race.
func resolveWorkspacePath(workspace, requestedPath string, mustExist bool) (string, error) {
	root, err := canonicalWorkspace(workspace)
	if err != nil {
		return "", err
	}
	return resolveWorkspacePathAtRoot(root, requestedPath, mustExist)
}

// resolveWorkspacePathAtRoot validates a path against a root that has already
// been canonicalized, as with the root pinned by a default tool registry.
func resolveWorkspacePathAtRoot(root, requestedPath string, mustExist bool) (string, error) {
	if strings.TrimSpace(requestedPath) == "" {
		return "", errors.New("path must not be empty")
	}
	if filepath.IsAbs(requestedPath) {
		return "", errors.New("path must be relative to the workspace")
	}

	candidate := filepath.Clean(filepath.Join(root, requestedPath))
	if !pathWithin(root, candidate) {
		return "", errors.New("path escapes the workspace")
	}
	if candidate == root {
		return "", errors.New("path refers to a directory; use list_dir to inspect it")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		if !pathWithin(root, resolved) {
			return "", errors.New("path resolves outside the workspace")
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil {
			return "", fmt.Errorf("stat path %q: %w", requestedPath, statErr)
		}
		if info.IsDir() {
			return "", errors.New("path refers to a directory; use list_dir to inspect it")
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("path does not refer to a regular file")
		}
		if mustExist {
			return resolved, nil
		}
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("resolve path %q: %w", requestedPath, err)
	}
	if mustExist {
		return "", fmt.Errorf("file %q does not exist", requestedPath)
	}
	if _, lstatErr := os.Lstat(candidate); lstatErr == nil {
		return "", errors.New("cannot write through an unresolved symlink")
	} else if !errors.Is(lstatErr, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect path %q: %w", requestedPath, lstatErr)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		return "", fmt.Errorf("parent directory for %q must exist: %w", requestedPath, err)
	}
	if !pathWithin(root, parent) {
		return "", errors.New("path resolves outside the workspace")
	}
	return filepath.Join(parent, filepath.Base(candidate)), nil
}

// resolveWorkspaceEntry preflights an existing workspace-relative path and
// returns its resolved name and metadata. An empty path or "." refers to the
// workspace root. Callers must use an os.Root handle for subsequent I/O.
func resolveWorkspaceEntry(workspace, requestedPath string) (string, fs.FileInfo, error) {
	root, err := canonicalWorkspace(workspace)
	if err != nil {
		return "", nil, err
	}
	return resolveWorkspaceEntryAtRoot(root, requestedPath)
}

// resolveWorkspaceEntryAtRoot resolves an existing entry against a root that
// has already been canonicalized.
func resolveWorkspaceEntryAtRoot(root, requestedPath string) (string, fs.FileInfo, error) {
	if strings.TrimSpace(requestedPath) == "" {
		requestedPath = "."
	}
	if filepath.IsAbs(requestedPath) {
		return "", nil, errors.New("path must be relative to the workspace")
	}

	candidate := filepath.Clean(filepath.Join(root, requestedPath))
	if !pathWithin(root, candidate) {
		return "", nil, errors.New("path escapes the workspace")
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, fmt.Errorf("path %q does not exist", requestedPath)
		}
		return "", nil, fmt.Errorf("resolve path %q: %w", requestedPath, err)
	}
	if !pathWithin(root, resolved) {
		return "", nil, errors.New("path resolves outside the workspace")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("stat path %q: %w", requestedPath, err)
	}
	return resolved, info, nil
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
