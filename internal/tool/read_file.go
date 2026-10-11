package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/afterlune/luneCore/internal/model"
)

const readFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the workspace"}},"required":["path"],"additionalProperties":false}`

// ReadFileTool reads a UTF-8 or plain-text file inside the workspace.
type ReadFileTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (ReadFileTool) Definition() model.FunctionTool {
	return functionDefinition("read_file", "Read and return the contents of a file in the workspace.", readFileSchema)
}

func (t ReadFileTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("read stopped: %w", err)}
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid read_file arguments: %w", err)}
	}
	root, closeRoot, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
	path, err := resolveWorkspacePath(t.Workspace, args.Path, true)
	if err != nil {
		return Result{Err: err}
	}
	relative, err := rootRelativePath(root, path)
	if err != nil {
		return Result{Err: err}
	}
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("read stopped: %w", err)}
	}
	file, err := root.Open(relative)
	if err != nil {
		return Result{Err: fmt.Errorf("read %q: %w", args.Path, err)}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Result{Err: fmt.Errorf("stat %q: %w", args.Path, err)}
	}
	if !info.Mode().IsRegular() {
		return Result{Err: fmt.Errorf("path %q does not refer to a regular file", args.Path)}
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return Result{Err: fmt.Errorf("read %q: %w", args.Path, err)}
	}
	return Result{Output: string(contents)}
}
