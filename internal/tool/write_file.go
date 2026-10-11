package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/afterlune/luneCore/internal/model"
)

const writeFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the workspace"},"content":{"type":"string","description":"Complete file contents to write"}},"required":["path","content"],"additionalProperties":false}`

// WriteFileTool creates or replaces a file inside the workspace.
type WriteFileTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (WriteFileTool) Definition() model.FunctionTool {
	return functionDefinition("write_file", "Create or replace a file in the workspace with the supplied contents.", writeFileSchema)
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
	path, err := resolveWorkspacePath(t.Workspace, args.Path, false)
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
	file, err := root.OpenFile(relative, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return Result{Err: fmt.Errorf("write %q: %w", args.Path, err)}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Result{Err: fmt.Errorf("stat %q: %w", args.Path, err)}
	}
	if !info.Mode().IsRegular() {
		return Result{Err: fmt.Errorf("path %q does not refer to a regular file", args.Path)}
	}
	if err := file.Truncate(0); err != nil {
		return Result{Err: fmt.Errorf("truncate %q: %w", args.Path, err)}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Result{Err: fmt.Errorf("seek %q: %w", args.Path, err)}
	}
	if _, err := io.WriteString(file, args.Content); err != nil {
		return Result{Err: fmt.Errorf("write %q: %w", args.Path, err)}
	}
	return Result{Output: fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), args.Path)}
}
