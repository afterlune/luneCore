package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
)

const (
	maxProjectInstructionBytes      = 32 << 10
	maxProjectInstructionTotalBytes = 64 << 10
)

func (c *Core) initialProjectInstructions(ctx context.Context) ([]agentgraph.ProjectInstruction, error) {
	return c.loadProjectInstructions(ctx, nil, []string{"."})
}

func (c *Core) projectInstructionsForTask(ctx context.Context, state agentgraph.State) ([]agentgraph.ProjectInstruction, error) {
	return c.loadProjectInstructions(ctx, state.ProjectInstructions, []string{"."})
}

func (c *Core) projectInstructionsForTool(ctx context.Context, state agentgraph.State, call model.OutputFunctionCall) ([]agentgraph.ProjectInstruction, error) {
	directories, relevant, err := c.projectInstructionDirectoriesForTool(ctx, call)
	if err != nil || !relevant {
		return nil, err
	}
	directories = append([]string{"."}, directories...)
	return c.loadProjectInstructions(ctx, state.ProjectInstructions, directories)
}

func (c *Core) projectInstructionDirectoriesForTool(ctx context.Context, call model.OutputFunctionCall) ([]string, bool, error) {
	var args struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	switch call.Name {
	case "read_file", "read_image", "write_file", "edit_file", "list_dir", "grep":
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return nil, true, fmt.Errorf("decode %s arguments for project instruction lookup: %w", call.Name, err)
		}
	case "glob":
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return nil, true, fmt.Errorf("decode glob arguments for project instruction lookup: %w", err)
		}
		base, inWorkspace, err := c.projectInstructionPath(args.Path, true)
		if err != nil || !inWorkspace {
			return nil, true, err
		}
		if base != "." {
			resolved, isDirectory, resolveErr := c.registry.WorkspaceResolvedEntryPath(ctx, base)
			if resolveErr != nil {
				return nil, true, resolveErr
			}
			if !isDirectory {
				return nil, true, errors.New("glob path is not a directory")
			}
			base = resolved
		}
		pattern := args.Pattern
		if base != "." {
			pattern = filepath.ToSlash(filepath.Join(filepath.FromSlash(base), filepath.FromSlash(pattern)))
		}
		directories, err := c.registry.WorkspaceGlobDirectories(ctx, pattern)
		return directories, true, err
	default:
		return nil, false, nil
	}

	if call.Name == "list_dir" || call.Name == "grep" {
		requested := args.Path
		if strings.TrimSpace(requested) == "" {
			requested = "."
		}
		clean, inWorkspace, err := c.projectInstructionPath(requested, true)
		if err != nil {
			return nil, true, err
		}
		if !inWorkspace {
			return nil, true, nil
		}
		clean, err = cleanProjectInstructionPath(clean, true)
		if err != nil {
			return nil, true, err
		}
		resolved, isDirectory, err := c.registry.WorkspaceResolvedEntryPath(ctx, clean)
		if err != nil {
			return nil, true, err
		}
		if call.Name == "list_dir" {
			return []string{resolved}, true, nil
		}
		if isDirectory {
			directories, err := c.registry.WorkspaceInstructionDirectories(ctx, resolved)
			if err != nil {
				return nil, true, err
			}
			return append(directories, resolved), true, nil
		}
		return c.fileInstructionDirectories(ctx, clean, resolved)
	}

	clean, inWorkspace, err := c.projectInstructionPath(args.Path, false)
	if err != nil {
		return nil, true, err
	}
	if !inWorkspace {
		return nil, true, nil
	}
	clean, err = cleanProjectInstructionPath(clean, false)
	if err != nil {
		return nil, true, err
	}
	resolved, err := c.registry.WorkspaceResolvedFilePath(ctx, clean, call.Name != "write_file")
	if err != nil {
		return nil, true, err
	}
	return c.fileInstructionDirectories(ctx, clean, resolved)
}

// projectInstructionPath resolves absolute targets to workspace-relative
// paths. External targets receive only the workspace-root guidance; their
// directory contents are not inspected before file-tool authorization.
func (c *Core) projectInstructionPath(requested string, allowDot bool) (string, bool, error) {
	if strings.TrimSpace(requested) == "" && allowDot {
		return ".", true, nil
	}
	if filepath.IsAbs(requested) {
		absolute := filepath.Clean(requested)
		relative, err := filepath.Rel(c.workspace, absolute)
		if err != nil {
			return "", false, fmt.Errorf("resolve workspace instruction path: %w", err)
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", false, nil
		}
		if relative == "." && !allowDot {
			return "", false, errors.New("path refers to a directory; use list_dir to inspect it")
		}
		return filepath.ToSlash(relative), true, nil
	}
	clean, err := cleanProjectInstructionPath(requested, allowDot)
	if err != nil {
		return "", false, err
	}
	return clean, true, nil
}

func (c *Core) fileInstructionDirectories(ctx context.Context, requested, resolved string) ([]string, bool, error) {
	requestedParent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(requested)))
	resolvedParent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(resolved)))
	canonicalParent, _, err := c.registry.WorkspaceResolvedEntryPath(ctx, requestedParent)
	if err != nil {
		return nil, true, err
	}
	directories := []string{resolvedParent}
	if canonicalParent == requestedParent {
		directories = append(directories, requestedParent)
	}
	return directories, true, nil
}

func (c *Core) loadProjectInstructions(ctx context.Context, existing []agentgraph.ProjectInstruction, directories []string) ([]agentgraph.ProjectInstruction, error) {
	known := make(map[string]struct{}, len(existing))
	totalBytes := 0
	for _, instruction := range existing {
		known[instruction.Path] = struct{}{}
		totalBytes += len(instruction.Content)
	}
	var loaded []agentgraph.ProjectInstruction
	for _, directory := range directories {
		clean, err := cleanProjectInstructionPath(directory, true)
		if err != nil {
			return nil, err
		}
		ancestors := projectInstructionAncestors(clean)
		for _, ancestor := range ancestors {
			instructionPath := "AGENTS.md"
			if ancestor != "." {
				instructionPath = filepath.ToSlash(filepath.Join(ancestor, "AGENTS.md"))
			}
			if _, ok := known[instructionPath]; ok {
				continue
			}
			content, err := c.registry.ReadWorkspaceFile(ctx, instructionPath, maxProjectInstructionBytes)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			if errors.Is(err, fs.ErrNotExist) {
				content = nil
			}
			if totalBytes+len(content) > maxProjectInstructionTotalBytes {
				return nil, fmt.Errorf("project instructions exceed the %d-byte execution limit", maxProjectInstructionTotalBytes)
			}
			loaded = append(loaded, agentgraph.ProjectInstruction{Path: instructionPath, Content: string(content)})
			totalBytes += len(content)
			known[instructionPath] = struct{}{}
		}
	}
	return loaded, nil
}

func projectInstructionAncestors(directory string) []string {
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

func cleanProjectInstructionPath(value string, allowDot bool) (string, error) {
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
		return "", errors.New("workspace path escapes the root")
	}
	clean = filepath.ToSlash(clean)
	if !fs.ValidPath(clean) {
		return "", errors.New("workspace path is invalid")
	}
	return clean, nil
}
