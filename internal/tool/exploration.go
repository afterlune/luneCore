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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/afterlune/luneCore/internal/model"
)

const (
	listDirSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative directory; use . for the workspace root"}},"additionalProperties":false}`
	globSchema    = `{"type":"object","properties":{"pattern":{"type":"string","description":"Workspace-relative glob pattern; supports ** for recursive matching"}},"required":["pattern"],"additionalProperties":false}`
	grepSchema    = `{"type":"object","properties":{"pattern":{"type":"string","description":"Case-sensitive regular expression matched against each text line"},"path":{"type":"string","description":"Workspace-relative file or directory; defaults to the workspace root"}},"required":["pattern"],"additionalProperties":false}`

	maxListEntries  = 500
	maxGlobMatches  = 500
	maxGrepMatches  = 200
	maxWalkEntries  = 10_000
	maxToolRunes    = 20_000
	maxGrepLineSize = 1 << 20
)

var errStopWalk = errors.New("stop workspace walk")

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

// GlobTool returns workspace-relative paths matching a glob pattern.
type GlobTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (GlobTool) Definition() model.FunctionTool {
	return functionDefinition("glob", "Find workspace files and directories by glob pattern; ** matches recursively.", globSchema)
}

func (t GlobTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("glob stopped: %w", err)}
	}
	var args struct {
		Pattern string `json:"pattern"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid glob arguments: %w", err)}
	}
	pattern, err := parseGlobPattern(args.Pattern)
	if err != nil {
		return Result{Err: err}
	}
	root, closeRoot, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()

	output := newToolText()
	matches := 0
	err = walkWorkspace(ctx, root, func(name string, entry fs.DirEntry) error {
		if name == "." {
			return nil
		}
		if !globMatches(pattern, filepath.ToSlash(name)) {
			return nil
		}
		if matches == maxGlobMatches {
			output.Truncate("match limit reached")
			return errStopWalk
		}
		matches++
		if !output.Write(fmt.Sprintf("%s\t%s\n", entryKind(entry), name)) {
			return errStopWalk
		}
		return nil
	})
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	if errors.Is(err, errWalkLimit) {
		output.Truncate("scan limit reached")
		err = nil
	}
	if err != nil {
		return Result{Err: err}
	}
	if matches == 0 && !output.truncated {
		return Result{Output: "No matches."}
	}
	return Result{Output: output.String()}
}

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

func optionalPathDefinition(name, description, schema string) model.FunctionTool {
	return model.FunctionTool{Name: name, Description: description, Parameters: json.RawMessage(schema)}
}

func entryKind(entry fs.DirEntry) string {
	if entry.Type()&os.ModeSymlink != 0 {
		return "symlink"
	}
	if entry.IsDir() {
		return "directory"
	}
	if entry.Type() == 0 || entry.Type().IsRegular() {
		return "file"
	}
	return "other"
}

func parseGlobPattern(raw string) ([]string, error) {
	pattern := strings.TrimSpace(strings.ReplaceAll(raw, `\`, "/"))
	if pattern == "" {
		return nil, errors.New("pattern must not be empty")
	}
	if strings.HasPrefix(pattern, "/") || filepath.IsAbs(filepath.FromSlash(pattern)) {
		return nil, errors.New("glob pattern must be relative to the workspace")
	}
	segments := strings.Split(pattern, "/")
	for _, segment := range segments {
		if segment == ".." {
			return nil, errors.New("glob pattern escapes the workspace")
		}
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return nil, fmt.Errorf("invalid glob pattern: %w", err)
		}
	}
	return segments, nil
}

func globMatches(pattern []string, name string) bool {
	parts := strings.Split(name, "/")
	type position struct{ pattern, name int }
	memo := make(map[position]bool)
	seen := make(map[position]bool)
	var match func(int, int) bool
	match = func(patternIndex, nameIndex int) bool {
		pos := position{patternIndex, nameIndex}
		if seen[pos] {
			return memo[pos]
		}
		seen[pos] = true
		var ok bool
		switch {
		case patternIndex == len(pattern):
			ok = nameIndex == len(parts)
		case pattern[patternIndex] == "**":
			ok = match(patternIndex+1, nameIndex)
			for index := nameIndex; !ok && index < len(parts); index++ {
				ok = match(patternIndex+1, index+1)
			}
		case nameIndex < len(parts):
			segmentMatches, _ := path.Match(pattern[patternIndex], parts[nameIndex])
			ok = segmentMatches && match(patternIndex+1, nameIndex+1)
		}
		memo[pos] = ok
		return ok
	}
	return match(0, 0)
}

var errWalkLimit = errors.New("workspace scan limit reached")

func walkWorkspace(ctx context.Context, root *os.Root, visit func(string, fs.DirEntry) error) error {
	return walkWorkspaceFrom(ctx, root.FS(), ".", visit)
}

func walkWorkspaceFrom(ctx context.Context, filesystem fs.FS, start string, visit func(string, fs.DirEntry) error) error {
	visited := 0
	err := fs.WalkDir(filesystem, start, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if filePath != start && entry.IsDir() && entry.Name() == ".git" {
			return fs.SkipDir
		}
		if visited == maxWalkEntries {
			return errWalkLimit
		}
		visited++
		return visit(filePath, entry)
	})
	return err
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

type toolText struct {
	builder   strings.Builder
	runes     int
	truncated bool
	reason    string
}

func newToolText() *toolText { return &toolText{} }

func (o *toolText) Write(text string) bool {
	if o.truncated {
		return false
	}
	const markerReserve = 128
	remaining := maxToolRunes - markerReserve - o.runes
	if remaining <= 0 {
		o.Truncate("output limit reached")
		return false
	}
	count := 0
	for index := range text {
		if count == remaining {
			o.builder.WriteString(text[:index])
			o.runes += count
			o.Truncate("output limit reached")
			return false
		}
		count++
	}
	o.builder.WriteString(text)
	o.runes += count
	return true
}

func (o *toolText) Truncate(reason string) {
	if !o.truncated {
		o.truncated = true
		o.reason = reason
	}
}

func (o *toolText) String() string {
	if !o.truncated {
		return o.builder.String()
	}
	return o.builder.String() + "\n[truncated: " + o.reason + "]"
}
