package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func (manager *Manager) readEnabled() error {
	data, err := os.ReadFile(manager.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read MCP enable state: %w", err)
	}
	var state struct {
		Enabled []string `json:"enabled"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode MCP enable state: %w", err)
	}
	for _, name := range state.Enabled {
		if _, exists := manager.servers[name]; exists {
			manager.enabled[name] = true
		} else {
			manager.logger.Warn("ignoring enabled MCP server missing from project config", "server", name)
		}
	}
	return nil
}

func (manager *Manager) writeEnabled() error {
	manager.stateMu.Lock()
	defer manager.stateMu.Unlock()
	manager.mu.Lock()
	names := make([]string, 0, len(manager.enabled))
	for name, enabled := range manager.enabled {
		if enabled {
			names = append(names, name)
		}
	}
	manager.mu.Unlock()
	sort.Strings(names)
	data, err := json.MarshalIndent(struct {
		Enabled []string `json:"enabled"`
	}{Enabled: names}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(manager.stateFile), 0700); err != nil {
		return fmt.Errorf("create MCP state directory: %w", err)
	}
	temporary := manager.stateFile + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write MCP enable state: %w", err)
	}
	if err := os.Rename(temporary, manager.stateFile); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace MCP enable state: %w", err)
	}
	return nil
}
