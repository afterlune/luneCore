package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	publictool "github.com/afterlune/luneCore/tool"
)

type pathKind uint8

const (
	pathFile pathKind = iota
	pathDirectory
	pathEntry
)

type resolvedToolPath struct {
	absolute string
	scope    string
	relative string
	internal bool
	info     fs.FileInfo
}

// resolveToolPath validates a file or directory and identifies the smallest
// directory that must be granted when it lies outside the workspace.
func resolveToolPath(workspace, requested string, kind pathKind, mustExist bool) (resolvedToolPath, error) {
	if strings.TrimSpace(requested) == "" {
		if kind == pathDirectory || kind == pathEntry {
			requested = "."
		} else {
			return resolvedToolPath{}, errors.New("path must not be empty")
		}
	}
	root, err := canonicalWorkspace(workspace)
	if err != nil {
		return resolvedToolPath{}, err
	}
	var abs string
	if !filepath.IsAbs(requested) {
		abs = filepath.Clean(filepath.Join(root, requested))
		if !pathWithin(root, abs) {
			return resolvedToolPath{}, fmt.Errorf("path %q escapes the workspace root %q; relative paths cannot access outside files. Please derive and provide the full absolute path (e.g. %q) to request a session directory grant", requested, root, abs)
		}
	} else {
		abs = filepath.Clean(requested)
	}

	if pathWithin(root, abs) {
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return resolvedToolPath{}, fmt.Errorf("make workspace-relative path: %w", err)
		}
		if rel == "" {
			rel = "."
		}
		var resolved string
		var info fs.FileInfo
		switch kind {
		case pathFile:
			resolved, err = resolveWorkspacePathAtRoot(root, rel, mustExist)
			if err == nil {
				info, err = os.Stat(resolved)
				if errors.Is(err, fs.ErrNotExist) && !mustExist {
					info, err = nil, nil
				}
			}
		case pathDirectory:
			resolved, info, err = resolveWorkspaceEntryAtRoot(root, rel)
			if err == nil && !info.IsDir() {
				err = errors.New("path is not a directory")
			}
		case pathEntry:
			resolved, info, err = resolveWorkspaceEntryAtRoot(root, rel)
		default:
			err = errors.New("unsupported path kind")
		}
		if err != nil {
			return resolvedToolPath{}, err
		}
		if !pathWithin(root, resolved) {
			return resolvedToolPath{}, errors.New("path resolves outside the workspace")
		}
		if info != nil && !info.IsDir() && !info.Mode().IsRegular() {
			return resolvedToolPath{}, errors.New("path does not refer to a regular file")
		}
		return resolvedToolPath{absolute: resolved, scope: root, relative: filepath.FromSlash(rel), internal: true, info: info}, nil
	}

	if kind == pathDirectory {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return resolvedToolPath{}, fmt.Errorf("resolve directory %q: %w", requested, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return resolvedToolPath{}, fmt.Errorf("stat directory %q: %w", requested, err)
		}
		if !info.IsDir() {
			return resolvedToolPath{}, errors.New("path is not a directory")
		}
		return resolvedToolPath{absolute: resolved, scope: resolved, relative: ".", info: info}, nil
	}

	if kind == pathEntry && mustExist {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return resolvedToolPath{}, fmt.Errorf("resolve path %q: %w", requested, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return resolvedToolPath{}, fmt.Errorf("stat path %q: %w", requested, err)
		}
		if info.IsDir() {
			return resolvedToolPath{absolute: resolved, scope: resolved, relative: ".", info: info}, nil
		}
		if !info.Mode().IsRegular() {
			return resolvedToolPath{}, errors.New("path does not refer to a regular file")
		}
		return resolvedToolPath{absolute: resolved, scope: filepath.Dir(resolved), relative: filepath.Base(resolved), info: info}, nil
	}

	if mustExist {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return resolvedToolPath{}, fmt.Errorf("resolve file %q: %w", requested, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return resolvedToolPath{}, fmt.Errorf("stat file %q: %w", requested, err)
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return resolvedToolPath{}, errors.New("path does not refer to a regular file")
		}
		return resolvedToolPath{absolute: resolved, scope: filepath.Dir(resolved), relative: filepath.Base(resolved), info: info}, nil
	}

	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return resolvedToolPath{}, errors.New("write target must be a regular file and must not be a symlink")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return resolvedToolPath{}, fmt.Errorf("inspect file %q: %w", requested, err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return resolvedToolPath{}, fmt.Errorf("parent directory for %q must exist: %w", requested, err)
	}
	return resolvedToolPath{absolute: filepath.Join(parent, filepath.Base(abs)), scope: parent, relative: filepath.Base(abs)}, nil
}

func pathGrantFor(workspace, requested string, kind pathKind, mustExist bool, access publictool.PathAccess) (*publictool.PathGrant, error) {
	resolved, err := resolveToolPath(workspace, requested, kind, mustExist)
	if err != nil {
		return nil, err
	}
	if resolved.internal {
		return nil, nil
	}
	return &publictool.PathGrant{Path: resolved.scope, Access: access}, nil
}

func openResolvedToolPath(ctx context.Context, workspace *os.Root, resolved resolvedToolPath, access publictool.PathAccess) (*os.Root, func(), string, error) {
	if resolved.internal {
		relative, err := rootRelativePath(workspace, resolved.absolute)
		if err != nil {
			return nil, nil, "", err
		}
		return workspace, func() {}, relative, nil
	}
	if !callHasPathGrant(ctx, resolved.scope, access) {
		return nil, nil, "", fmt.Errorf("access to %q requires a session directory grant", resolved.scope)
	}
	root, err := os.OpenRoot(resolved.scope)
	if err != nil {
		return nil, nil, "", fmt.Errorf("open granted directory %q: %w", resolved.scope, err)
	}
	opened, statErr := root.Stat(".")
	current, currentErr := os.Stat(resolved.scope)
	if statErr != nil || currentErr != nil || !opened.IsDir() || !os.SameFile(opened, current) {
		_ = root.Close()
		return nil, nil, "", errors.New("granted directory changed while opening it")
	}
	return root, func() { _ = root.Close() }, resolved.relative, nil
}

func callHasPathGrant(ctx context.Context, scope string, access publictool.PathAccess) bool {
	grant, ok := ApprovedPathGrantFromContext(ctx)
	if !ok {
		return false
	}
	return PathGrantAllows([]publictool.PathGrant{grant}, publictool.PathGrant{Path: scope, Access: access})
}

func pathGrantRequestForFile(workspace string, raw json.RawMessage, access publictool.PathAccess, mustExist bool) (*publictool.PathGrant, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return pathGrantFor(workspace, args.Path, pathFile, mustExist, access)
}

func pathGrantRequestForDirectory(workspace string, raw json.RawMessage, key string) (*publictool.PathGrant, error) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	var requested string
	if encoded, ok := args[key]; ok {
		if err := json.Unmarshal(encoded, &requested); err != nil {
			return nil, fmt.Errorf("%s must be a string", key)
		}
	}
	return pathGrantFor(workspace, requested, pathDirectory, true, publictool.PathRead)
}

func (t ReadFileTool) PathAccessRequest(_ context.Context, raw json.RawMessage) (*publictool.PathGrant, error) {
	return pathGrantRequestForFile(t.Workspace, raw, publictool.PathRead, true)
}

func (t ReadImageTool) PathAccessRequest(_ context.Context, raw json.RawMessage) (*publictool.PathGrant, error) {
	return pathGrantRequestForFile(t.Workspace, raw, publictool.PathRead, true)
}

func (t WriteFileTool) PathAccessRequest(_ context.Context, raw json.RawMessage) (*publictool.PathGrant, error) {
	return pathGrantRequestForFile(t.Workspace, raw, publictool.PathReadWrite, false)
}

func (t EditFileTool) PathAccessRequest(_ context.Context, raw json.RawMessage) (*publictool.PathGrant, error) {
	return pathGrantRequestForFile(t.Workspace, raw, publictool.PathReadWrite, true)
}

func (t ListDirTool) PathAccessRequest(_ context.Context, raw json.RawMessage) (*publictool.PathGrant, error) {
	return pathGrantRequestForDirectory(t.Workspace, raw, "path")
}

func (t GlobTool) PathAccessRequest(_ context.Context, raw json.RawMessage) (*publictool.PathGrant, error) {
	return pathGrantRequestForDirectory(t.Workspace, raw, "path")
}

func (t GrepTool) PathAccessRequest(_ context.Context, raw json.RawMessage) (*publictool.PathGrant, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return pathGrantFor(t.Workspace, args.Path, pathEntry, true, publictool.PathRead)
}
