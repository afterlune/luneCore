package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
)

const maxWorkspaceInstructionGlobMatches = 500

var errWorkspacePathEscapesRoot = errors.New("workspace path escapes the root")

// ReadWorkspaceFile reads a bounded regular file through the registry's
// pinned workspace root.
func (r *Registry) ReadWorkspaceFile(ctx context.Context, relativePath string, maxBytes int64) ([]byte, error) {
	if r == nil || ctx == nil || maxBytes <= 0 {
		return nil, errors.New("tool registry, context, and positive file size limit are required")
	}
	relative, err := cleanWorkspaceRelativePath(relativePath, false)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	identity := r.workspaceRoot
	r.mu.RUnlock()
	if identity == nil || identity.root == nil {
		return nil, errors.New("workspace root is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := identity.root.Open(filepath.FromSlash(relative))
	if err != nil {
		return nil, fmt.Errorf("open workspace file %q: %w", relative, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat workspace file %q: %w", relative, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workspace path %q is not a regular file", relative)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("workspace file %q exceeds the %d-byte instruction limit", relative, maxBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read workspace file %q: %w", relative, err)
	}
	if int64(len(contents)) > maxBytes {
		return nil, fmt.Errorf("workspace file %q exceeds the %d-byte instruction limit", relative, maxBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return contents, nil
}

// WorkspaceResolvedFilePath resolves a built-in file-tool target while keeping
// the result relative to the registry's pinned workspace.
func (r *Registry) WorkspaceResolvedFilePath(ctx context.Context, relativePath string, mustExist bool) (string, error) {
	if r == nil || ctx == nil {
		return "", errors.New("tool registry and context are required")
	}
	relative, err := cleanWorkspaceRelativePath(relativePath, false)
	if err != nil {
		return "", err
	}
	identity, err := r.pinnedWorkspaceRoot()
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	resolved, err := resolveWorkspacePathAtRoot(identity.root.Name(), filepath.FromSlash(relative), mustExist)
	if err != nil {
		return "", err
	}
	resolved, err = rootRelativePath(identity.root, resolved)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return filepath.ToSlash(resolved), nil
}

// WorkspaceResolvedEntryPath resolves an existing file or directory entry and
// reports its kind, including for in-workspace symlinks.
func (r *Registry) WorkspaceResolvedEntryPath(ctx context.Context, relativePath string) (string, bool, error) {
	if r == nil || ctx == nil {
		return "", false, errors.New("tool registry and context are required")
	}
	relative, err := cleanWorkspaceRelativePath(relativePath, true)
	if err != nil {
		return "", false, err
	}
	identity, err := r.pinnedWorkspaceRoot()
	if err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	resolved, info, err := resolveWorkspaceEntryAtRoot(identity.root.Name(), filepath.FromSlash(relative))
	if err != nil {
		return "", false, err
	}
	resolved, err = rootRelativePath(identity.root, resolved)
	if err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return filepath.ToSlash(resolved), info.IsDir(), nil
}

// WorkspaceInstructionDirectories returns directories containing AGENTS.md
// files below a workspace-relative directory. It uses the same bounded,
// root-pinned walk as the built-in exploration tools.
func (r *Registry) WorkspaceInstructionDirectories(ctx context.Context, relativePath string) ([]string, error) {
	if r == nil || ctx == nil {
		return nil, errors.New("tool registry and context are required")
	}
	relative, err := cleanWorkspaceRelativePath(relativePath, true)
	if err != nil {
		return nil, err
	}
	identity, err := r.pinnedWorkspaceRoot()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := identity.root.Stat(filepath.FromSlash(relative))
	if err != nil {
		return nil, fmt.Errorf("stat workspace path %q: %w", relative, err)
	}
	if !info.IsDir() {
		return []string{filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))}, nil
	}
	var directories []string
	seen := make(map[string]struct{})
	err = walkWorkspaceFrom(ctx, identity.root.FS(), relative, func(name string, entry fs.DirEntry) error {
		if entry.Name() != "AGENTS.md" {
			return nil
		}
		directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name)))
		if _, ok := seen[directory]; !ok {
			seen[directory] = struct{}{}
			directories = append(directories, directory)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan project instruction directories below %q: %w", relative, err)
	}
	return directories, nil
}

// WorkspaceGlobDirectories returns the directories of matching paths for
// project-instruction lookup, with bounded result and walk limits.
func (r *Registry) WorkspaceGlobDirectories(ctx context.Context, rawPattern string) ([]string, error) {
	if r == nil || ctx == nil {
		return nil, errors.New("tool registry and context are required")
	}
	pattern, err := parseGlobPattern(rawPattern)
	if err != nil {
		return nil, err
	}
	identity, err := r.pinnedWorkspaceRoot()
	if err != nil {
		return nil, err
	}
	var directories []string
	seen := make(map[string]struct{})
	matches := 0
	err = walkWorkspace(ctx, identity.root, func(name string, entry fs.DirEntry) error {
		if name == "." || !globMatches(pattern, filepath.ToSlash(name)) {
			return nil
		}
		matches++
		directory := name
		if !entry.IsDir() {
			directory = filepath.Dir(name)
		}
		directory = filepath.ToSlash(directory)
		if _, ok := seen[directory]; !ok {
			seen[directory] = struct{}{}
			directories = append(directories, directory)
		}
		if matches >= maxWorkspaceInstructionGlobMatches {
			return errStopWalk
		}
		return nil
	})
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	if errors.Is(err, errWalkLimit) {
		return nil, fmt.Errorf("workspace scan limit reached while resolving glob project instructions")
	}
	if err != nil {
		return nil, err
	}
	return directories, nil
}

func (r *Registry) pinnedWorkspaceRoot() (*workspaceIdentity, error) {
	r.mu.RLock()
	identity := r.workspaceRoot
	r.mu.RUnlock()
	if identity == nil || identity.root == nil {
		return nil, errors.New("workspace root is unavailable")
	}
	return identity, nil
}

func cleanWorkspaceRelativePath(value string, allowDot bool) (string, error) {
	if strings.TrimSpace(value) == "" {
		if allowDot {
			return ".", nil
		}
		return "", errors.New("workspace path must not be empty")
	}
	if filepath.IsAbs(value) || filepath.VolumeName(value) != "" {
		return "", errors.New("workspace path must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == "." {
		if allowDot {
			return ".", nil
		}
		return "", errors.New("workspace path must name a file")
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errWorkspacePathEscapesRoot
	}
	clean = filepath.ToSlash(clean)
	if !fs.ValidPath(clean) {
		return "", errors.New("workspace path is invalid")
	}
	return clean, nil
}
