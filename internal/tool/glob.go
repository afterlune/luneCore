package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

const (
	globSchema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Workspace-relative glob pattern; supports ** for recursive matching"},"offset":{"type":["integer","null"],"minimum":0,"description":"Zero-based match offset; defaults to 0"},"limit":{"type":["integer","null"],"minimum":1,"maximum":500,"description":"Maximum matches to return; defaults to 100"}},"required":["pattern","offset","limit"],"additionalProperties":false}`
)

// GlobTool returns workspace-relative paths matching a glob pattern.
type GlobTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (GlobTool) Definition() model.FunctionTool {
	return functionDefinition("glob", "Find workspace files and directories by glob pattern; ** matches recursively. Use offset and limit to continue through large result sets.", globSchema)
}

func (t GlobTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("glob stopped: %w", err)}
	}
	var args struct {
		Pattern string `json:"pattern"`
		Offset  *int   `json:"offset"`
		Limit   *int   `json:"limit"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid glob arguments: %w", err)}
	}
	page, err := parsePageRequest(args.Offset, args.Limit)
	if err != nil {
		return Result{Err: fmt.Errorf("invalid glob pagination: %w", err)}
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

	var output pageText
	matches := 0
	nextOffset := page.offset
	more := false
	scanLimited := false
	err = walkWorkspace(ctx, root, func(name string, entry fs.DirEntry) error {
		if name == "." {
			return nil
		}
		if !globMatches(pattern, filepath.ToSlash(name)) {
			return nil
		}
		if matches == maxSearchMatches {
			scanLimited = true
			more = true
			return errStopWalk
		}
		index := matches
		if index < page.offset {
			matches++
			return nil
		}
		if index >= page.offset+page.limit {
			nextOffset = index
			more = true
			return errStopWalk
		}
		record := fmt.Sprintf("%s\t%s\n", entryKind(entry), name)
		if !output.Add(record) {
			if output.Empty() && output.AddClipped(record, "\n[match truncated]") {
				matches++
				nextOffset = matches
			} else {
				nextOffset = index
			}
			more = true
			return errStopWalk
		}
		matches++
		nextOffset = matches
		return nil
	})
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	if errors.Is(err, errWalkLimit) {
		scanLimited = true
		err = nil
	}
	if err != nil {
		return Result{Err: err}
	}
	if output.Empty() && matches == 0 && !scanLimited {
		return Result{Output: "No matches.\n[end of results]"}
	}
	if output.Empty() {
		return Result{Output: pageFooter(nextOffset, more, scanLimited)}
	}
	return Result{Output: output.String(pageFooter(nextOffset, more, scanLimited))}
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
