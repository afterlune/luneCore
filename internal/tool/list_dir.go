package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const listDirSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative or absolute directory; use . for the workspace root."},"offset":{"type":"integer","minimum":0,"description":"Zero-based entry offset; defaults to 0"},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum entries to return; defaults to 100"}},"additionalProperties":false}`

// ListDirTool lists a page of sorted direct children of a workspace directory.
type ListDirTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (ListDirTool) Definition() model.FunctionTool {
	return optionalPathDefinition("list_dir", "List sorted directory entries for a workspace or absolute directory path; use offset and limit to page.", listDirSchema)
}

func (t ListDirTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("list_dir stopped: %w", err)}
	}
	var args struct {
		Path   string `json:"path"`
		Offset *int   `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid list_dir arguments: %w", err)}
	}
	page, err := parsePageRequest(args.Offset, args.Limit)
	if err != nil {
		return Result{Err: fmt.Errorf("invalid list_dir pagination: %w", err)}
	}
	workspaceRoot, closeWorkspace, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeWorkspace()
	resolved, err := resolveToolPath(workspaceRoot.Name(), args.Path, pathDirectory, true)
	if err != nil {
		return Result{Err: err}
	}
	root, closeRoot, relative, err := openResolvedToolPath(ctx, workspaceRoot, resolved, publictool.PathRead)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
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
	entries, err := directoryFile.ReadDir(maxDirectoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return Result{Err: fmt.Errorf("list directory: %w", err)}
	}
	scanLimited := len(entries) > maxDirectoryEntries
	if scanLimited {
		entries = entries[:maxDirectoryEntries]
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("list_dir stopped: %w", err)}
	}

	var output pageText
	start := min(page.offset, len(entries))
	end := min(start+page.limit, len(entries))
	nextOffset := start
	for index := start; index < end; index++ {
		if err := ctx.Err(); err != nil {
			return Result{Err: fmt.Errorf("list_dir stopped: %w", err)}
		}
		record := fmt.Sprintf("%s\t%s\n", entryKind(entries[index]), entries[index].Name())
		if !output.Add(record) {
			if output.Empty() && output.AddClipped(record, "\n[entry truncated]") {
				nextOffset = index + 1
			} else {
				nextOffset = index
			}
			break
		}
		nextOffset = index + 1
	}
	more := nextOffset < len(entries)
	if output.Empty() && len(entries) == 0 {
		return Result{Output: "Directory is empty.\n[end of results]"}
	}
	if output.Empty() {
		return Result{Output: output.String(pageFooter(nextOffset, more, scanLimited))}
	}
	return Result{Output: output.String(pageFooter(nextOffset, more, scanLimited))}
}

func (ListDirTool) InstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error) {
	return listDirInstructionDirectories(ctx, r, args)
}

