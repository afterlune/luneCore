package tool

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

const (
	maxWalkEntries = 10_000
	maxToolRunes   = 20_000
)

var errStopWalk = errors.New("stop workspace walk")

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
