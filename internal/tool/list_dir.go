package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/afterlune/luneCore/internal/model"
)

const (
	listDirSchema  = `{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative directory; use . for the workspace root"}},"additionalProperties":false}`
	maxListEntries = 500
)

// ListDirTool lists the direct children of a workspace directory.
type ListDirTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (ListDirTool) Definition() model.FunctionTool {
	return optionalPathDefinition("list_dir", "List names and types of entries in a workspace directory.", listDirSchema)
}

func (t ListDirTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("list_dir stopped: %w", err)}
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid list_dir arguments: %w", err)}
	}
	root, closeRoot, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
	directory, info, err := resolveWorkspaceEntry(t.Workspace, args.Path)
	if err != nil {
		return Result{Err: err}
	}
	if !info.IsDir() {
		return Result{Err: errors.New("path is not a directory")}
	}
	relative, err := rootRelativePath(root, directory)
	if err != nil {
		return Result{Err: err}
	}
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("list_dir stopped: %w", err)}
	}
	directoryFile, err := root.Open(relative)
	if err != nil {
		return Result{Err: fmt.Errorf("list directory: %w", err)}
	}
	defer directoryFile.Close()
	openedInfo, err := directoryFile.Stat()
	if err != nil {
		return Result{Err: fmt.Errorf("stat directory: %w", err)}
	}
	if !openedInfo.IsDir() {
		return Result{Err: errors.New("path is not a directory")}
	}
	entries, err := directoryFile.ReadDir(-1)
	if err != nil {
		return Result{Err: fmt.Errorf("list directory: %w", err)}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if len(entries) == 0 {
		return Result{Output: "Directory is empty."}
	}

	output := newToolText()
	limit := len(entries)
	if limit > maxListEntries {
		limit = maxListEntries
	}
	for _, entry := range entries[:limit] {
		if err := ctx.Err(); err != nil {
			return Result{Err: fmt.Errorf("list_dir stopped: %w", err)}
		}
		if !output.Write(fmt.Sprintf("%s\t%s\n", entryKind(entry), entry.Name())) {
			break
		}
	}
	if len(entries) > limit {
		output.Truncate("entry limit reached")
	}
	return Result{Output: output.String()}
}
