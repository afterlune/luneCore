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
	maxWalkEntries      = 10_000
	maxToolRunes        = 20_000
	maxSearchMatches    = 10_000
	maxDirectoryEntries = maxWalkEntries
	maxReadLineBytes    = 1 << 20
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
		if filePath != start && entry.IsDir() && isIgnoredDir(entry.Name()) {
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

func isIgnoredDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", "node_modules", ".luneagent", ".cache", "dist", "build", ".vite", ".next", ".turbo", ".nuxt", ".output", "coverage":
		return true
	default:
		return false
	}
}
