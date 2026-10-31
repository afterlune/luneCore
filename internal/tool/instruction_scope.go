package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

const (
	MaxProjectInstructionBytes      = 32 << 10
	MaxProjectInstructionTotalBytes = 64 << 10
)

// ProjectInstructionFile is one AGENTS.md instruction file discovered for a workspace path.
type ProjectInstructionFile struct {
	Path    string
	Content string
}

// InstructionScopedTool is optionally implemented by tools that access filesystem
// paths and trigger loading nested AGENTS.md guidance.
type InstructionScopedTool interface {
	InstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error)
}

// InstructionDirectoriesForCall queries the tool by name to determine relevant instruction directories.
func (r *Registry) InstructionDirectoriesForCall(ctx context.Context, name string, args json.RawMessage) ([]string, bool, error) {
	if r == nil {
		return nil, false, errors.New("tool registry is nil")
	}
	r.mu.RLock()
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if scoped, ok := t.(InstructionScopedTool); ok {
		return scoped.InstructionDirectories(ctx, r, args)
	}
	return nil, false, nil
}

// LoadProjectInstructions loads AGENTS.md files for all ancestor paths of the given directories.
func (r *Registry) LoadProjectInstructions(ctx context.Context, existing []ProjectInstructionFile, directories []string) ([]ProjectInstructionFile, error) {
	if r == nil {
		return nil, errors.New("tool registry is nil")
	}
	known := make(map[string]struct{}, len(existing))
	totalBytes := 0
	for _, instruction := range existing {
		known[instruction.Path] = struct{}{}
		totalBytes += len(instruction.Content)
	}
	var loaded []ProjectInstructionFile
	for _, directory := range directories {
		clean, err := cleanWorkspaceRelativePath(directory, true)
		if err != nil {
			return nil, err
		}
		ancestors := ProjectInstructionAncestors(clean)
		for _, ancestor := range ancestors {
			instructionPath := "AGENTS.md"
			if ancestor != "." {
				instructionPath = filepath.ToSlash(filepath.Join(ancestor, "AGENTS.md"))
			}
			if _, ok := known[instructionPath]; ok {
				continue
			}
			content, err := r.ReadWorkspaceFile(ctx, instructionPath, MaxProjectInstructionBytes)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			if errors.Is(err, fs.ErrNotExist) {
				content = nil
			}
			if totalBytes+len(content) > MaxProjectInstructionTotalBytes {
				return nil, fmt.Errorf("project instructions exceed the %d-byte execution limit", MaxProjectInstructionTotalBytes)
			}
			loaded = append(loaded, ProjectInstructionFile{Path: instructionPath, Content: string(content)})
			totalBytes += len(content)
			known[instructionPath] = struct{}{}
		}
	}
	return loaded, nil
}

// ProjectInstructionPath resolves absolute targets to workspace-relative paths.
func ProjectInstructionPath(workspace, requested string, allowDot bool) (string, bool, error) {
	if strings.TrimSpace(requested) == "" && allowDot {
		return ".", true, nil
	}
	if filepath.IsAbs(requested) {
		absolute := filepath.Clean(requested)
		if !pathWithin(workspace, absolute) {
			return "", false, nil
		}
		relative, err := filepath.Rel(workspace, absolute)
		if err != nil {
			return "", false, fmt.Errorf("resolve workspace instruction path: %w", err)
		}
		if relative == "." && !allowDot {
			return "", false, errors.New("path refers to a directory; use list_dir to inspect it")
		}
		return filepath.ToSlash(relative), true, nil
	}
	clean, err := cleanWorkspaceRelativePath(requested, allowDot)
	if err != nil {
		if errors.Is(err, errWorkspacePathEscapesRoot) {
			abs := filepath.Clean(filepath.Join(workspace, requested))
			return "", false, fmt.Errorf("path %q escapes the workspace root %q; relative paths cannot access outside files. Please derive and provide the full absolute path (e.g. %q) to request a session directory grant", requested, workspace, abs)
		}
		return "", false, err
	}
	return clean, true, nil
}

// ProjectInstructionAncestors splits a workspace path into all ancestor directory paths.
func ProjectInstructionAncestors(directory string) []string {
	if directory == "." {
		return []string{"."}
	}
	parts := strings.Split(filepath.ToSlash(directory), "/")
	ancestors := make([]string, 0, len(parts)+1)
	ancestors = append(ancestors, ".")
	for index := range parts {
		ancestors = append(ancestors, strings.Join(parts[:index+1], "/"))
	}
	return ancestors
}

// FileInstructionDirectories resolves directory scope for a single file tool call.
func FileInstructionDirectories(ctx context.Context, r *Registry, requested, resolved string) ([]string, bool, error) {
	requestedParent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(requested)))
	resolvedParent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(resolved)))
	canonicalParent, _, err := r.WorkspaceResolvedEntryPath(ctx, requestedParent)
	if err != nil {
		return nil, true, err
	}
	directories := []string{resolvedParent}
	if canonicalParent == requestedParent {
		directories = append(directories, requestedParent)
	}
	return directories, true, nil
}

// CleanProjectInstructionPath validates and normalizes relative workspace paths.
func CleanProjectInstructionPath(value string, allowDot bool) (string, error) {
	return cleanWorkspaceRelativePath(value, allowDot)
}

func fileInstructionDirectoriesForPath(ctx context.Context, r *Registry, args json.RawMessage, mustExist bool) ([]string, bool, error) {
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return nil, true, fmt.Errorf("decode file arguments for project instruction lookup: %w", err)
	}
	pinned, err := r.pinnedWorkspaceRoot()
	if err != nil {
		return nil, true, err
	}
	clean, inWorkspace, err := ProjectInstructionPath(pinned.root.Name(), payload.Path, false)
	if err != nil {
		return nil, true, err
	}
	if !inWorkspace {
		return nil, true, nil
	}
	clean, err = cleanWorkspaceRelativePath(clean, false)
	if err != nil {
		return nil, true, err
	}
	resolved, err := r.WorkspaceResolvedFilePath(ctx, clean, mustExist)
	if err != nil {
		return nil, true, err
	}
	return FileInstructionDirectories(ctx, r, clean, resolved)
}

func listDirInstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error) {
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return nil, true, fmt.Errorf("decode list_dir arguments for project instruction lookup: %w", err)
	}
	requested := payload.Path
	if strings.TrimSpace(requested) == "" {
		requested = "."
	}
	pinned, err := r.pinnedWorkspaceRoot()
	if err != nil {
		return nil, true, err
	}
	clean, inWorkspace, err := ProjectInstructionPath(pinned.root.Name(), requested, true)
	if err != nil {
		return nil, true, err
	}
	if !inWorkspace {
		return nil, true, nil
	}
	clean, err = cleanWorkspaceRelativePath(clean, true)
	if err != nil {
		return nil, true, err
	}
	resolved, _, err := r.WorkspaceResolvedEntryPath(ctx, clean)
	if err != nil {
		return nil, true, err
	}
	return []string{resolved}, true, nil
}

func grepInstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error) {
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return nil, true, fmt.Errorf("decode grep arguments for project instruction lookup: %w", err)
	}
	requested := payload.Path
	if strings.TrimSpace(requested) == "" {
		requested = "."
	}
	pinned, err := r.pinnedWorkspaceRoot()
	if err != nil {
		return nil, true, err
	}
	clean, inWorkspace, err := ProjectInstructionPath(pinned.root.Name(), requested, true)
	if err != nil {
		return nil, true, err
	}
	if !inWorkspace {
		return nil, true, nil
	}
	clean, err = cleanWorkspaceRelativePath(clean, true)
	if err != nil {
		return nil, true, err
	}
	resolved, isDirectory, err := r.WorkspaceResolvedEntryPath(ctx, clean)
	if err != nil {
		return nil, true, err
	}
	if isDirectory {
		directories, err := r.WorkspaceInstructionDirectories(ctx, resolved)
		if err != nil {
			return nil, true, err
		}
		return append(directories, resolved), true, nil
	}
	return FileInstructionDirectories(ctx, r, clean, resolved)
}

func globInstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error) {
	var payload struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return nil, true, fmt.Errorf("decode glob arguments for project instruction lookup: %w", err)
	}
	pinned, err := r.pinnedWorkspaceRoot()
	if err != nil {
		return nil, true, err
	}
	base, inWorkspace, err := ProjectInstructionPath(pinned.root.Name(), payload.Path, true)
	if err != nil || !inWorkspace {
		return nil, true, err
	}
	if base != "." {
		resolved, isDirectory, resolveErr := r.WorkspaceResolvedEntryPath(ctx, base)
		if resolveErr != nil {
			return nil, true, resolveErr
		}
		if !isDirectory {
			return nil, true, errors.New("glob path is not a directory")
		}
		base = resolved
	}
	pattern := payload.Pattern
	if base != "." {
		pattern = filepath.ToSlash(filepath.Join(filepath.FromSlash(base), filepath.FromSlash(pattern)))
	}
	directories, err := r.WorkspaceGlobDirectories(ctx, pattern)
	return directories, true, err
}
