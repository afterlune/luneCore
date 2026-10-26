package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const readFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative path, or an absolute path outside the workspace (which requires a session directory grant)"},"start_line":{"type":["integer","null"],"minimum":1,"description":"One-based first line to read; defaults to 1"},"limit":{"type":["integer","null"],"minimum":1,"maximum":500,"description":"Maximum lines to return; defaults to 200"}},"required":["path","start_line","limit"],"additionalProperties":false}`

// ReadFileTool reads a bounded line range from a UTF-8 or plain-text file.
type ReadFileTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (ReadFileTool) Definition() model.FunctionTool {
	return functionDefinition("read_file", "Read a workspace or session-granted external file by line range; output includes line numbers and the next start_line when more content remains.", readFileSchema)
}

func (t ReadFileTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("read stopped: %w", err)}
	}
	var args struct {
		Path      string `json:"path"`
		StartLine *int   `json:"start_line"`
		Limit     *int   `json:"limit"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid read_file arguments: %w", explainReadFileArgumentError(err))}
	}
	startLine := 1
	if args.StartLine != nil {
		if *args.StartLine < 1 {
			return Result{Err: errors.New("start_line must be 1 or greater")}
		}
		startLine = *args.StartLine
	}
	lineLimit := 200
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > maxPageSize {
			return Result{Err: errors.New("limit must be between 1 and 500")}
		}
		lineLimit = *args.Limit
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
	root, closeRoot, relative, err := openResolvedToolPath(ctx, workspaceRoot, resolved, publictool.PathRead)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
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

	reader := bufio.NewReader(file)
	lineNumber := 1
	for lineNumber < startLine {
		_, _, found, _, err := readBoundedLine(ctx, reader, 0)
		if err != nil {
			return Result{Err: fmt.Errorf("read %q: %w", args.Path, err)}
		}
		if !found {
			return Result{Output: fmt.Sprintf("No lines at or after start_line=%d.\n[end of file]", startLine)}
		}
		lineNumber++
	}

	var output pageText
	emitted := 0
	nextLine := startLine
	more := false
	lineTruncated := false
	for emitted < lineLimit {
		if err := ctx.Err(); err != nil {
			return Result{Err: fmt.Errorf("read stopped: %w", err)}
		}
		line, truncated, found, newline, err := readBoundedLine(ctx, reader, maxReadLineBytes)
		if err != nil {
			return Result{Err: fmt.Errorf("read %q: %w", args.Path, err)}
		}
		if !found {
			break
		}
		lineText := string(line)
		if newline {
			lineText = strings.TrimSuffix(lineText, "\r")
		}
		lineText = strings.ToValidUTF8(lineText, "�")
		record := fmt.Sprintf("%d | %s\n", lineNumber, lineText)
		if output.Add(record) {
			emitted++
			nextLine = lineNumber + 1
			lineNumber++
			if truncated {
				lineTruncated = true
			}
			continue
		}
		if output.Empty() {
			marker := "\n[line truncated; remainder omitted]"
			if truncated {
				marker = "\n[line exceeds 1 MiB; remainder omitted]"
			}
			if output.AddClipped(record, marker) {
				emitted++
				nextLine = lineNumber + 1
				lineNumber++
				lineTruncated = true
				continue
			}
		}
		more = true
		break
	}
	if !more && emitted == lineLimit {
		_, _, more, _, err = readBoundedLine(ctx, reader, 0)
		if err != nil {
			return Result{Err: fmt.Errorf("read %q: %w", args.Path, err)}
		}
	}
	if emitted == 0 {
		return Result{Output: fmt.Sprintf("No lines at or after start_line=%d.\n[end of file]", startLine)}
	}
	footer := fmt.Sprintf("[next_start_line=%d; more=%t; line_truncated=%t]", nextLine, more, lineTruncated)
	return Result{Output: output.String(footer)}
}

func explainReadFileArgumentError(err error) error {
	const unknownFieldPrefix = "json: unknown field "
	message := err.Error()
	if !strings.HasPrefix(message, unknownFieldPrefix) {
		return err
	}
	field := strings.Trim(strings.TrimPrefix(message, unknownFieldPrefix), `"`)
	if field == "limit_number" {
		return fmt.Errorf("unknown field %q; use \"limit\" for the maximum number of lines; accepted fields are \"path\", \"start_line\", and \"limit\"", field)
	}
	return fmt.Errorf("unknown field %q; accepted fields are \"path\", \"start_line\", and \"limit\"", field)
}

func readBoundedLine(ctx context.Context, reader *bufio.Reader, maximum int) ([]byte, bool, bool, bool, error) {
	line := make([]byte, 0, min(maximum, 4096))
	truncated := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, false, false, err
		}
		fragment, err := reader.ReadSlice('\n')
		contentLength := len(fragment)
		if contentLength > 0 && fragment[contentLength-1] == '\n' {
			contentLength--
		}
		if !truncated && contentLength > 0 {
			remaining := maximum - len(line)
			if remaining < 0 {
				remaining = 0
			}
			if contentLength > remaining {
				line = append(line, fragment[:remaining]...)
				truncated = true
			} else {
				line = append(line, fragment[:contentLength]...)
			}
		}
		switch {
		case err == nil:
			return line, truncated, true, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(fragment) > 0 || len(line) > 0 || truncated {
				return line, truncated, true, false, nil
			}
			return nil, false, false, false, nil
		default:
			return nil, false, false, false, err
		}
	}
}

func (ReadFileTool) InstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error) {
	return fileInstructionDirectoriesForPath(ctx, r, args, true)
}
