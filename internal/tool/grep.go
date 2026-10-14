package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/afterlune/luneCore/internal/model"
)

const (
	grepSchema      = `{"type":"object","properties":{"pattern":{"type":"string","description":"Case-sensitive regular expression matched against each text line"},"path":{"type":"string","description":"Workspace-relative file or directory; defaults to the workspace root"}},"required":["pattern"],"additionalProperties":false}`
	maxGrepMatches  = 200
	maxGrepLineSize = 1 << 20
)

// GrepTool searches text files line by line with a regular expression.
type GrepTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (GrepTool) Definition() model.FunctionTool {
	return optionalPathDefinition("grep", "Search text files by case-sensitive regular expression. Directories are searched recursively; .git is skipped.", grepSchema)
}

func (t GrepTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("grep stopped: %w", err)}
	}
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid grep arguments: %w", err)}
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return Result{Err: errors.New("pattern must not be empty")}
	}
	pattern, err := regexp.Compile(args.Pattern)
	if err != nil {
		return Result{Err: fmt.Errorf("invalid grep pattern: %w", err)}
	}
	workspaceRoot, closeRoot, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
	root, rootInfo, err := resolveWorkspaceEntry(t.Workspace, args.Path)
	if err != nil {
		return Result{Err: err}
	}
	rootRelative, err := rootRelativePath(workspaceRoot, root)
	if err != nil {
		return Result{Err: err}
	}
	if isInsideGitDirectory(workspaceRoot.Name(), root) {
		return Result{Output: "Search skipped: .git is excluded."}
	}

	output := newToolText()
	matches := 0
	matchFile := func(relative string, info fs.FileInfo) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := workspaceRoot.Open(relative)
		if err != nil {
			return err
		}
		openedInfo, err := file.Stat()
		if err != nil {
			file.Close()
			return err
		}
		if !openedInfo.Mode().IsRegular() {
			file.Close()
			return nil
		}
		err = grepFile(ctx, file, pattern, func(lineNumber int, line string) error {
			if matches == maxGrepMatches {
				output.Truncate("match limit reached")
				return errStopWalk
			}
			matches++
			if !output.Write(fmt.Sprintf("%s:%d:%s\n", filepath.ToSlash(relative), lineNumber, line)) {
				return errStopWalk
			}
			return nil
		})
		return err
	}

	if !rootInfo.IsDir() {
		err = matchFile(rootRelative, rootInfo)
	} else {
		err = walkWorkspaceFrom(ctx, workspaceRoot.FS(), filepath.ToSlash(rootRelative), func(relative string, entry fs.DirEntry) error {
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return matchFile(filepath.FromSlash(relative), info)
		})
	}
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	if errors.Is(err, errWalkLimit) {
		output.Truncate("scan limit reached")
		err = nil
	}
	if err != nil {
		return Result{Err: fmt.Errorf("search files: %w", err)}
	}
	if matches == 0 && !output.truncated {
		return Result{Output: "No matches."}
	}
	return Result{Output: output.String()}
}

func isInsideGitDirectory(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(filepath.ToSlash(relative), "/") {
		if segment == ".git" {
			return true
		}
	}
	return false
}

func grepFile(ctx context.Context, file *os.File, expression *regexp.Regexp, visit func(int, string) error) error {
	defer file.Close()

	prefix := make([]byte, 8192)
	count, readErr := io.ReadFull(file, prefix)
	if bytes.IndexByte(prefix[:count], 0) >= 0 {
		return nil
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return readErr
	}
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(prefix[:count]), file))
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxGrepLineSize)
	lineNumber := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		lineNumber++
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.IndexByte(line, 0) >= 0 {
			return nil
		}
		if !utf8.ValidString(line) || !expression.MatchString(line) {
			continue
		}
		if err := visit(lineNumber, line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("line exceeds %d bytes in %q", maxGrepLineSize, file.Name())
		}
		return err
	}
	return nil
}
