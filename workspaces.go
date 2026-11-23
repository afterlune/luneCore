package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// WorkspaceRuntime is the execution API bound to a single workspace.
type WorkspaceRuntime = Core

// Workspace identifies an open directory for the lifetime of the manager.
type Workspace struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type managedWorkspace struct {
	Workspace
	info    os.FileInfo
	runtime *WorkspaceRuntime
}

// WorkspaceManager owns independently configured workspace runtimes.
// Each managed workspace has exclusive ownership of its .lune state directory.
type WorkspaceManager struct {
	mu      sync.Mutex
	opened  []*managedWorkspace
	closing bool
}

// OpenWorkspace configures a newly opened workspace. The zero manager is ready to use.
func (m *WorkspaceManager) OpenWorkspace(path string, configure func(string) (Config, error)) (Workspace, *WorkspaceRuntime, error) {
	if !filepath.IsAbs(path) {
		return Workspace{}, nil, errors.New("workspace must be an absolute directory path")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Workspace{}, nil, err
	}
	for _, component := range strings.Split(filepath.ToSlash(canonical), "/") {
		if strings.EqualFold(component, ".lune") {
			return Workspace{}, nil, errors.New("cannot open a Lune state directory as a workspace")
		}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return Workspace{}, nil, err
	}
	if !info.IsDir() {
		return Workspace{}, nil, errors.New("workspace must be a directory")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return Workspace{}, nil, errors.New("workspace manager is shutting down")
	}
	for _, existing := range m.opened {
		if os.SameFile(existing.info, info) {
			return existing.Workspace, existing.runtime, nil
		}
		if existing.Path == canonical {
			return Workspace{}, nil, errors.New("the open workspace directory was replaced")
		}
	}

	var config Config
	if configure != nil {
		config, err = configure(canonical)
		if err != nil {
			return Workspace{}, nil, err
		}
	}
	config.Workspace = canonical
	config.StateDir = filepath.Join(canonical, ".lune")
	config.ExclusiveState = true
	state, err := os.Lstat(config.StateDir)
	if err == nil && (!state.IsDir() || state.Mode()&os.ModeSymlink != 0) {
		return Workspace{}, nil, errors.New(".lune must be a real directory in the workspace")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, nil, err
	}
	client, err := New(config)
	if err != nil {
		return Workspace{}, nil, err
	}
	workspace := Workspace{ID: rand.Text(), Path: canonical}
	m.opened = append(m.opened, &managedWorkspace{Workspace: workspace, info: info, runtime: client})
	return workspace, client, nil
}

func (m *WorkspaceManager) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	m.mu.Lock()
	m.closing = true
	opened := m.opened
	m.mu.Unlock()

	results := make(chan error, len(opened))
	for _, entry := range opened {
		go func() {
			err := entry.runtime.Shutdown(ctx)
			if err != nil {
				err = fmt.Errorf("shut down workspace %s: %w", entry.Path, err)
			}
			results <- err
		}()
	}
	var err error
	for range opened {
		err = errors.Join(err, <-results)
	}
	return err
}
