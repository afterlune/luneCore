package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const editFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative path, or an absolute path outside the workspace (which requires a session read-write directory grant)"},"old_text":{"type":"string","description":"Exact text to find; must occur once"},"new_text":{"type":"string","description":"Replacement text"}},"required":["path","old_text","new_text"],"additionalProperties":false}`

// EditFileTool replaces one exact, unique text occurrence inside a file.
type EditFileTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (EditFileTool) Definition() model.FunctionTool {
	return functionDefinition("edit_file", "Replace one unique exact text match in a workspace or session-granted external file; large files are streamed in chunks.", editFileSchema)
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
	workspaceRoot, closeWorkspace, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeWorkspace()
	resolved, err := resolveToolPath(workspaceRoot.Name(), args.Path, pathFile, true)
	if err != nil {
		return Result{Err: err}
	}
	root, closeRoot, relative, err := openResolvedToolPath(ctx, workspaceRoot, resolved, publictool.PathReadWrite)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
	file, err := root.Open(relative)
	if err != nil {
		return Result{Err: fmt.Errorf("open %q for editing: %w", args.Path, err)}
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return Result{Err: fmt.Errorf("stat %q: %w", args.Path, err)}
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return Result{Err: fmt.Errorf("path %q does not refer to a regular file", args.Path)}
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return Result{Err: fmt.Errorf("edit stopped: %w", err)}
	}

	if info.Size() <= maxDiffSourceBytes {
		contents, readErr := io.ReadAll(io.LimitReader(file, maxDiffSourceBytes+1))
		closeErr := file.Close()
		file = nil
		if readErr != nil {
			return Result{Err: fmt.Errorf("read %q: %w", args.Path, readErr)}
		}
		if closeErr != nil {
			return Result{Err: fmt.Errorf("close %q: %w", args.Path, closeErr)}
		}
		if len(contents) <= maxDiffSourceBytes {
			count := bytes.Count(contents, []byte(args.OldText))
			if count != 1 {
				return Result{Err: uniqueTextError(args.Path, count)}
			}
			updated := bytes.Replace(contents, []byte(args.OldText), []byte(args.NewText), 1)
			if err := replaceFile(ctx, root, relative, info.Mode(), true, func(destination io.Writer) error {
				return writeContextBytes(ctx, destination, updated)
			}); err != nil {
				return Result{Err: fmt.Errorf("edit %q: %w", args.Path, err)}
			}
			return Result{Output: fmt.Sprintf("Updated %s", args.Path), Details: fileChange(resolved.absolute, string(contents), string(updated))}
		}
		file, err = root.Open(relative)
		if err != nil {
			return Result{Err: fmt.Errorf("open %q for editing: %w", args.Path, err)}
		}
	}
	if int64(len(args.OldText)) > info.Size() {
		_ = file.Close()
		return Result{Err: uniqueTextError(args.Path, 0)}
	}
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()

	err = replaceFile(ctx, root, relative, info.Mode(), true, func(destination io.Writer) error {
		count, replaceErr := replaceTextStream(ctx, file, destination, []byte(args.OldText), []byte(args.NewText))
		closeErr := file.Close()
		file = nil
		if replaceErr != nil {
			return replaceErr
		}
		if count != 1 {
			return uniqueTextError(args.Path, count)
		}
		if closeErr != nil {
			return fmt.Errorf("close %q: %w", args.Path, closeErr)
		}
		return nil
	})
	if err != nil {
		return Result{Err: fmt.Errorf("edit %q: %w", args.Path, err)}
	}
	return Result{Output: fmt.Sprintf("Updated %s", args.Path)}
}

func uniqueTextError(path string, count int) error {
	return fmt.Errorf("old_text must occur exactly once in %q; found %d occurrences", path, count)
}

func replaceTextStream(ctx context.Context, source io.Reader, destination io.Writer, oldText, newText []byte) (int, error) {
	reader := bufio.NewReaderSize(source, 64*1024)
	readBuffer := make([]byte, 64*1024)
	pending := make([]byte, 0, 64*1024)
	matches := 0
	for {
		if err := ctx.Err(); err != nil {
			return matches, fmt.Errorf("edit stopped: %w", err)
		}
		read, readErr := reader.Read(readBuffer)
		if read > 0 {
			pending = append(pending, readBuffer[:read]...)
		}
		atEOF := errors.Is(readErr, io.EOF)
		if readErr != nil && !atEOF {
			return matches, readErr
		}

		safeEnd := len(pending)
		if !atEOF {
			safeEnd = len(pending) - len(oldText) + 1
			if safeEnd < 0 {
				safeEnd = 0
			}
		}
		position := 0
		for position < safeEnd {
			match := bytes.Index(pending[position:], oldText)
			if match < 0 {
				break
			}
			start := position + match
			if start >= safeEnd {
				break
			}
			if err := writeAll(destination, pending[position:start]); err != nil {
				return matches, fmt.Errorf("write temporary file: %w", err)
			}
			matches++
			if matches > 1 {
				return matches, nil
			}
			if err := writeAll(destination, newText); err != nil {
				return matches, fmt.Errorf("write temporary file: %w", err)
			}
			position = start + len(oldText)
		}
		if position < safeEnd {
			if err := writeAll(destination, pending[position:safeEnd]); err != nil {
				return matches, fmt.Errorf("write temporary file: %w", err)
			}
			position = safeEnd
		}
		pending = append(pending[:0], pending[position:]...)
		if atEOF {
			return matches, nil
		}
		if read == 0 && readErr == nil {
			return matches, io.ErrNoProgress
		}
	}
}

func (EditFileTool) InstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error) {
	return fileInstructionDirectoriesForPath(ctx, r, args, true)
}

