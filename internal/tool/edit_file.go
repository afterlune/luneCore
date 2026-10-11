package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/afterlune/luneCore/internal/model"
)

const editFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Path relative to the workspace"},"old_text":{"type":"string","description":"Exact text to find; must occur once"},"new_text":{"type":"string","description":"Replacement text"}},"required":["path","old_text","new_text"],"additionalProperties":false}`

// EditFileTool replaces one exact, unique text occurrence inside a file.
type EditFileTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (EditFileTool) Definition() model.FunctionTool {
	return functionDefinition("edit_file", "Replace one unique exact text occurrence in a file in the workspace.", editFileSchema)
}

func (t EditFileTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("edit stopped: %w", err)}
	}
	var args struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid edit_file arguments: %w", err)}
	}
	if args.OldText == "" {
		return Result{Err: errors.New("old_text must not be empty")}
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
		return Result{Err: fmt.Errorf("edit stopped: %w", err)}
	}
	file, err := root.OpenFile(relative, os.O_RDWR, 0)
	if err != nil {
		return Result{Err: fmt.Errorf("open %q for editing: %w", args.Path, err)}
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
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("edit stopped: %w", err)}
	}
	count := bytes.Count(contents, []byte(args.OldText))
	if count != 1 {
		return Result{Err: fmt.Errorf("old_text must occur exactly once in %q; found %d occurrences", args.Path, count)}
	}
	updated := bytes.Replace(contents, []byte(args.OldText), []byte(args.NewText), 1)
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("edit stopped: %w", err)}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Result{Err: fmt.Errorf("seek %q: %w", args.Path, err)}
	}
	if err := file.Truncate(0); err != nil {
		return Result{Err: fmt.Errorf("truncate %q: %w", args.Path, err)}
	}
	if _, err := file.Write(updated); err != nil {
		return Result{Err: fmt.Errorf("write %q: %w", args.Path, err)}
	}
	return Result{Output: fmt.Sprintf("Updated %s", args.Path)}
}
