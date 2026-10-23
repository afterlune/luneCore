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
	publictool "github.com/afterlune/luneCore/tool"
)

const (
	globSchema = `{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern relative to path; supports ** for recursive matching"},"path":{"type":"string","description":"Workspace-relative or absolute directory to search; outside directories require a session read grant. Defaults to the workspace root"},"offset":{"type":["integer","null"],"minimum":0,"description":"Zero-based match offset; defaults to 0"},"limit":{"type":["integer","null"],"minimum":1,"maximum":500,"description":"Maximum matches to return; defaults to 100"}},"required":["pattern","offset","limit"],"additionalProperties":false}`
)

// GlobTool returns workspace-relative paths matching a glob pattern.
type GlobTool struct {
	Workspace string
	workspace *workspaceIdentity
}

func (GlobTool) Definition() model.FunctionTool {
	return functionDefinition("glob", "Find paths matching a pattern under a workspace or session-granted external directory; ** matches recursively. Use offset and limit to page.", globSchema)
}

func (t GlobTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("glob stopped: %w", err)}
	}
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
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
	matcher := newGlobMatcher(pattern)
	workspaceRoot, closeWorkspace, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeWorkspace()
	resolved, err := resolveToolPath(workspaceRoot.Name(), args.Path, pathDirectory, true)
	if err != nil {
		return Result{Err: err}
	}
	root, closeRoot, baseRelative, err := openResolvedToolPath(ctx, workspaceRoot, resolved, publictool.PathRead)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
	baseWalk := filepath.ToSlash(baseRelative)
	if baseWalk == "" {
		baseWalk = "."
	}

	var output pageText
	matches := 0
	nextOffset := page.offset
	more := false
	scanLimited := false
	err = walkWorkspaceFrom(ctx, root.FS(), baseWalk, func(name string, entry fs.DirEntry) error {
		if name == baseWalk {
			return nil
		}
		relativeToBase := strings.TrimPrefix(filepath.ToSlash(name), strings.TrimSuffix(baseWalk, "/")+"/")
		if baseWalk == "." {
			relativeToBase = filepath.ToSlash(name)
		}
		if !matcher.matches(relativeToBase) {
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
		outputName := filepath.ToSlash(filepath.Join(filepath.FromSlash(baseWalk), filepath.FromSlash(relativeToBase)))
		if baseWalk == "." {
			outputName = relativeToBase
		}
		if !resolved.internal {
			outputName = filepath.Join(resolved.absolute, filepath.FromSlash(relativeToBase))
		}
		record := fmt.Sprintf("%s\t%s\n", entryKind(entry), outputName)
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
	matcher := newGlobMatcher(pattern)
	return matcher.matches(name)
}

type globMatcher struct {
	pattern []string
	current []bool
	next    []bool
}

func newGlobMatcher(pattern []string) globMatcher {
	return globMatcher{
		pattern: pattern,
		current: make([]bool, len(pattern)+1),
		next:    make([]bool, len(pattern)+1),
	}
}

// matches evaluates ** with reusable dynamic-programming buffers. The matcher
// is local to one tool call, so no synchronization or per-entry allocations are needed.
func (matcher *globMatcher) matches(name string) bool {
	clear(matcher.current)
	clear(matcher.next)
	matcher.current[0] = true
	matcher.expandDoubleStars(matcher.current)

	for {
		separator := strings.IndexByte(name, '/')
		segment := name
		if separator >= 0 {
			segment = name[:separator]
			name = name[separator+1:]
		}
		clear(matcher.next)
		for index, pattern := range matcher.pattern {
			if !matcher.current[index] {
				continue
			}
			if pattern == "**" {
				matcher.next[index] = true
				continue
			}
			if matched, _ := path.Match(pattern, segment); matched {
				matcher.next[index+1] = true
			}
		}
		matcher.expandDoubleStars(matcher.next)
		matcher.current, matcher.next = matcher.next, matcher.current
		if separator < 0 {
			break
		}
	}
	return matcher.current[len(matcher.pattern)]
}

func (matcher *globMatcher) expandDoubleStars(states []bool) {
	for index, pattern := range matcher.pattern {
		if states[index] && pattern == "**" {
			states[index+1] = true
		}
	}
}

func (GlobTool) InstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error) {
	return globInstructionDirectories(ctx, r, args)
}

