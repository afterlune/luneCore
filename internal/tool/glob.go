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
	globSchema     = `{"type":"object","properties":{"pattern":{"type":"string","description":"Workspace-relative glob pattern; supports ** for recursive matching"}},"required":["pattern"],"additionalProperties":false}`
	maxGlobMatches = 500
)

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
