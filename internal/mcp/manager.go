package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"sort"
	"sync"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

type Limits struct {
	MaxBinaryBlocks int
	MaxFileBytes    int64
	MaxTotalBytes   int64
}

type InteractionHandler func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)

type Options struct {
	Workspace string
	StateDir  string
	// RestoreEnabled loads and persists enabled server state when true.
	RestoreEnabled     bool
	Logger             *slog.Logger
	Servers            map[string]projectconfig.MCPServerConfig
	Registry           *tool.Registry
	Limits             Limits
	RequestInteraction InteractionHandler
}

type ServerStatus struct {
	Name      string
	Transport projectconfig.MCPTransport
	Endpoint  string
	Command   string
	Enabled   bool
	Connected bool
	ToolCount int
	LastError string
}

type Manager struct {
	mu             sync.Mutex
	connectGate    chan struct{}
	generations    map[string]uint64
	connecting     map[string]context.CancelFunc
	logger         *slog.Logger
	workspace      string
	stateFile      string
	restoreEnabled bool
	servers        map[string]projectconfig.MCPServerConfig
	registry       *tool.Registry
	limits         Limits
	interact       InteractionHandler
	enabled        map[string]bool
	instances      map[string]*serverInstance
	errors         map[string]string
	closed         bool
}

type serverInstance struct {
	client            transport
	tools             []*remoteTool
	category          model.ToolCategory
	config            projectconfig.MCPServerConfig
	owner             *Manager
	nextHostOperation uint64
	hostOperations    map[uint64]context.CancelCauseFunc
	hostOperationsWG  sync.WaitGroup
}

var ErrServerLifecycleChanged = errors.New("MCP server lifecycle changed")

type remoteTool struct {
	server       *serverInstance
	serverName   string
	remoteName   string
	definition   model.FunctionTool
	readOnly     bool
	bindings     []headerBinding
	registeredID string
}

func NewManager(options Options) (*Manager, error) {
	if options.Registry == nil {
		return nil, errors.New("MCP manager requires a tool registry")
	}
	if options.Workspace == "" || options.StateDir == "" {
		return nil, errors.New("MCP manager requires workspace and state directory")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Limits.MaxBinaryBlocks <= 0 {
		options.Limits.MaxBinaryBlocks = 5
	}
	if options.Limits.MaxFileBytes <= 0 {
		options.Limits.MaxFileBytes = 10 << 20
	}
	if options.Limits.MaxTotalBytes <= 0 {
		options.Limits.MaxTotalBytes = 20 << 20
	}
	manager := &Manager{
		logger:      options.Logger,
		connectGate: make(chan struct{}, 1),
		generations: make(map[string]uint64),
		connecting:  make(map[string]context.CancelFunc),
		workspace:   options.Workspace, stateFile: filepath.Join(options.StateDir, "mcp-enabled.json"),
		restoreEnabled: options.RestoreEnabled,
		servers:        options.Servers, registry: options.Registry, limits: options.Limits,
		interact: options.RequestInteraction, enabled: make(map[string]bool),
		instances: make(map[string]*serverInstance), errors: make(map[string]string),
	}
	if manager.restoreEnabled {
		if err := manager.readEnabled(); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func (manager *Manager) RestoreEnabled() []error {
	manager.mu.Lock()
	names := make([]string, 0, len(manager.enabled))
	for name, enabled := range manager.enabled {
		if enabled {
			names = append(names, name)
		}
	}
	manager.mu.Unlock()
	sort.Strings(names)
	var failures []error
	for _, name := range names {
		if err := manager.connect(name); err != nil {
			manager.mu.Lock()
			manager.errors[name] = err.Error()
			manager.mu.Unlock()
			failures = append(failures, fmt.Errorf("restore MCP server %q: %w", name, err))
		}
	}
	return failures
}

func (manager *Manager) Enable(name string) error {
	if err := manager.reconnect(name); err != nil {
		manager.mu.Lock()
		manager.errors[name] = err.Error()
		manager.mu.Unlock()
		return err
	}
	if err := manager.persistEnabled(name, true); err != nil {
		return errors.Join(err, manager.Disable(name))
	}
	return nil
}

func (manager *Manager) Disable(name string) error {
	manager.mu.Lock()
	manager.generations[name]++
	if cancel := manager.connecting[name]; cancel != nil {
		cancel()
	}
	instance := manager.instances[name]
	manager.cancelHostOperationsLocked(instance)
	delete(manager.instances, name)
	delete(manager.enabled, name)
	delete(manager.errors, name)
	manager.unregister(instance)
	manager.mu.Unlock()
	var closeErr error
	if instance != nil {
		instance.hostOperationsWG.Wait()
		closeErr = instance.client.Close()
	}
	return errors.Join(closeErr, manager.persistEnabled(name, false))
}

func (manager *Manager) persistEnabled(name string, enabled bool) error {
	if !manager.restoreEnabled {
		return nil
	}
	return manager.writeEnabled(name, enabled)
}

func (manager *Manager) List() []ServerStatus {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	names := make([]string, 0, len(manager.servers))
	for name := range manager.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]ServerStatus, 0, len(names))
	for _, name := range names {
		config := manager.servers[name]
		instance := manager.instances[name]
		connected := instance != nil
		if stdio, ok := instanceClient(instance).(*stdioTransport); ok {
			connected = stdio.Alive()
		}
		status := ServerStatus{Name: name, Transport: config.Transport, Enabled: manager.enabled[name],
			Connected: connected, LastError: manager.errors[name]}
		if config.Transport == projectconfig.MCPTransportStreamableHTTP {
			status.Endpoint = safeEndpoint(config.URL)
		} else {
			status.Command = filepath.Base(config.Command)
		}
		if instance != nil {
			status.ToolCount = len(instance.tools)
		}
		result = append(result, status)
	}
	return result
}

func safeEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String()
}

func (manager *Manager) Close() error {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	for _, cancel := range manager.connecting {
		cancel()
	}
	instances := make([]*serverInstance, 0, len(manager.instances))
	for _, instance := range manager.instances {
		instances = append(instances, instance)
		manager.cancelHostOperationsLocked(instance)
		manager.unregister(instance)
	}
	manager.instances = make(map[string]*serverInstance)
	manager.mu.Unlock()
	var failures []error
	for _, instance := range instances {
		instance.hostOperationsWG.Wait()
		if err := instance.client.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (manager *Manager) cancelHostOperationsLocked(instance *serverInstance) {
	if instance == nil {
		return
	}
	for _, cancel := range instance.hostOperations {
		cancel(ErrServerLifecycleChanged)
	}
}
