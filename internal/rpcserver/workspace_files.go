package rpcserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"
)

const workspacePageSize = 200
const workspacePreviewLimit = 256 * 1024

var errWorkspacePath = errors.New("invalid workspace path")
var errWorkspaceFile = errors.New("unsupported workspace file")

type workspaceEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (c *connection) workspaceFiles(method string, raw json.RawMessage) (any, *rpcError) {
	var params struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	workspace := c.server.core.Workspace()
	if method == "workspace.info" {
		return map[string]string{"name": filepath.Base(workspace), "path": workspace}, nil
	}
	list := method == "workspace.listDirectory"
	if params.Offset < 0 {
		return nil, &rpcError{Code: -32602, Message: "offset cannot be negative"}
	}
	f, err := openWorkspaceEntry(workspace, c.server.workspaceInfo, params.Path, list)
	if err != nil {
		switch {
		case errors.Is(err, errWorkspacePath):
			return nil, &rpcError{Code: -32602, Message: "invalid workspace path", Data: "workspace_invalid_path"}
		case errors.Is(err, errWorkspaceFile):
			return nil, &rpcError{Code: -32000, Message: "unsupported file or directory", Data: "workspace_unsupported_file"}
		case errors.Is(err, os.ErrNotExist):
			return nil, &rpcError{Code: -32000, Message: "workspace entry not found", Data: "workspace_not_found"}
		default:
			return nil, &rpcError{Code: -32000, Message: "could not open workspace entry", Data: "workspace_unavailable"}
		}
	}
	defer f.Close()

	if list {
		entries, err := f.ReadDir(-1)
		if err != nil {
			return nil, &rpcError{Code: -32000, Message: "could not read directory", Data: "workspace_unavailable"}
		}
		rows := make([]workspaceEntry, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if !workspacePathComponent(name) || !utf8.ValidString(name) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			kind := "file"
			if info.IsDir() {
				kind = "directory"
			} else if !info.Mode().IsRegular() {
				continue
			}
			rows = append(rows, workspaceEntry{Name: name, Kind: kind})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Kind != rows[j].Kind {
				return rows[i].Kind == "directory"
			}
			return rows[i].Name < rows[j].Name
		})
		from := min(params.Offset, len(rows))
		to := from + min(workspacePageSize, len(rows)-from)
		return map[string]any{"items": rows[from:to], "nextOffset": to, "more": to < len(rows)}, nil
	}

	data, err := io.ReadAll(io.LimitReader(f, workspacePreviewLimit+1))
	if err != nil {
		return nil, &rpcError{Code: -32000, Message: "could not read file", Data: "workspace_unavailable"}
	}
	if len(data) > workspacePreviewLimit {
		return nil, &rpcError{Code: -32000, Message: "file exceeds the 256 KiB preview limit", Data: "workspace_file_too_large"}
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, &rpcError{Code: -32000, Message: "only UTF-8 text can be previewed", Data: "workspace_not_text"}
	}
	return map[string]string{"text": string(data)}, nil
}

func workspacePathComponent(name string) bool {
	if name == "" || name == "." || name == ".." || strings.EqualFold(name, ".lune") {
		return false
	}
	if runtime.GOOS == "windows" && strings.TrimRight(name, ". ") != name {
		return false
	}
	_, err := filepath.Localize(name)
	return err == nil
}

// Check each entry against its no-follow stat before using it. The Root
// also prevents a rename or replaced path from resolving outside its parent.
func openWorkspaceEntry(workspace string, workspaceInfo os.FileInfo, name string, directory bool) (*os.File, error) {
	var parts []string
	if name == "" && directory {
		parts = []string{"."}
	} else {
		if !fs.ValidPath(name) {
			return nil, errWorkspacePath
		}
		parts = strings.Split(name, "/")
		for _, component := range parts {
			if !workspacePathComponent(component) {
				return nil, errWorkspacePath
			}
		}
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { root.Close() }()
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(workspaceInfo, info) {
		return nil, errWorkspaceFile
	}
	for _, component := range parts[:len(parts)-1] {
		before, err := root.Lstat(component)
		if err != nil {
			return nil, err
		}
		if !before.IsDir() || workspaceStateDirectory(root, before) {
			return nil, errWorkspaceFile
		}
		child, err := root.OpenRoot(component)
		if err != nil {
			return nil, err
		}
		after, err := child.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			child.Close()
			return nil, errWorkspaceFile
		}
		root.Close()
		root = child
	}
	component := parts[len(parts)-1]
	before, err := root.Lstat(component)
	if err != nil {
		return nil, err
	}
	if (directory && (!before.IsDir() || workspaceStateDirectory(root, before))) || (!directory && !before.Mode().IsRegular()) {
		return nil, errWorkspaceFile
	}
	f, err := openWorkspacePreview(root, component)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, errWorkspaceFile
	}
	return f, nil
}

func workspaceStateDirectory(root *os.Root, info os.FileInfo) bool {
	state, err := root.Lstat(".lune")
	return err == nil && os.SameFile(info, state)
}
