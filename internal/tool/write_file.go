package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/afterlune/luneCore/internal/model"
)

const writeFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the workspace"},"content":{"type":"string","description":"Complete file contents to write"}},"required":["path","content"],"additionalProperties":false}`

// WriteFileTool creates or replaces a file inside the workspace.
type WriteFileTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (WriteFileTool) Definition() model.FunctionTool {
	return functionDefinition("write_file", "Create or replace a workspace file with complete contents.", writeFileSchema)
}

func (t WriteFileTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("write stopped: %w", err)}
	}
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid write_file arguments: %w", err)}
	}
	root, closeRoot, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
	path, err := resolveWorkspacePathAtRoot(root.Name(), args.Path, false)
	if err != nil {
		return Result{Err: err}
	}
	relative, err := rootRelativePath(root, path)
	if err != nil {
		return Result{Err: err}
	}
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("write stopped: %w", err)}
	}

	mode := fs.FileMode(0o644)
	preserveMode := false
	var before []byte
	captured := false
	info, statErr := root.Stat(relative)
	if statErr == nil {
		if !info.Mode().IsRegular() {
			return Result{Err: fmt.Errorf("path %q does not refer to a regular file", args.Path)}
		}
		mode = info.Mode().Perm()
		preserveMode = true
		if info.Size() <= maxDiffSourceBytes && len(args.Content) <= maxDiffSourceBytes {
			if oldFile, openErr := root.Open(relative); openErr == nil {
				before, openErr = io.ReadAll(io.LimitReader(oldFile, maxDiffSourceBytes+1))
				_ = oldFile.Close()
				captured = openErr == nil && len(before) <= maxDiffSourceBytes
			}
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return Result{Err: fmt.Errorf("stat %q: %w", args.Path, statErr)}
	}

	if err := replaceFile(ctx, root, relative, mode, preserveMode, func(destination io.Writer) error {
		return writeContextString(ctx, destination, args.Content)
	}); err != nil {
		return Result{Err: fmt.Errorf("write %q: %w", args.Path, err)}
	}
	result := Result{Output: fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), args.Path)}
	if captured {
		result.Details = fileChange(path, string(before), args.Content)
	}
	return result
}
