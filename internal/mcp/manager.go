package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

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
	Workspace          string
	StateDir           string
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
	mu          sync.Mutex
	connectGate chan struct{}
	stateMu     sync.Mutex
	generations map[string]uint64
	connecting  map[string]context.CancelFunc
	logger      *slog.Logger
	workspace   string
	stateFile   string
	servers     map[string]projectconfig.MCPServerConfig
	registry    *tool.Registry
	limits      Limits
	interact    InteractionHandler
	enabled     map[string]bool
	instances   map[string]*serverInstance
	errors      map[string]string
	closed      bool
}

type serverInstance struct {
	client   transport
	tools    []*remoteTool
	category model.ToolCategory
	config   projectconfig.MCPServerConfig
	owner    *Manager
}

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
		servers: options.Servers, registry: options.Registry, limits: options.Limits,
		interact: options.RequestInteraction, enabled: make(map[string]bool),
		instances: make(map[string]*serverInstance), errors: make(map[string]string),
	}
	if err := manager.readEnabled(); err != nil {
		return nil, err
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
	if err := manager.writeEnabled(); err != nil {
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
	delete(manager.instances, name)
	delete(manager.enabled, name)
	delete(manager.errors, name)
	manager.unregister(instance)
	manager.mu.Unlock()
	var closeErr error
	if instance != nil {
		closeErr = instance.client.Close()
	}
	return errors.Join(closeErr, manager.writeEnabled())
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
		manager.unregister(instance)
	}
	manager.instances = make(map[string]*serverInstance)
	manager.mu.Unlock()
	var failures []error
	for _, instance := range instances {
		if err := instance.client.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (manager *Manager) connect(name string) error {
	return manager.connectServer(context.Background(), name, false)
}

func (manager *Manager) reconnect(name string) error {
	return manager.connectServer(context.Background(), name, true)
}

func (manager *Manager) connectServer(ctx context.Context, name string, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	manager.mu.Lock()
	generation := manager.generations[name]
	manager.mu.Unlock()
	select {
	case manager.connectGate <- struct{}{}:
		defer func() { <-manager.connectGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	manager.mu.Lock()
	if manager.closed || manager.generations[name] != generation || (!force && !manager.enabled[name]) {
		manager.mu.Unlock()
		return errors.New("MCP server is disabled or its connection attempt was invalidated")
	}
	if err := ctx.Err(); err != nil {
		manager.mu.Unlock()
		return err
	}
	existing := manager.instances[name]
	if existing != nil {
		stdio, isStdio := existing.client.(*stdioTransport)
		if !force && (!isStdio || stdio.Alive()) {
			manager.mu.Unlock()
			return nil
		}
		delete(manager.instances, name)
		manager.unregister(existing)
	}
	config, exists := manager.servers[name]
	manager.connecting[name] = cancel
	manager.mu.Unlock()
	defer func() { manager.mu.Lock(); delete(manager.connecting, name); manager.mu.Unlock() }()
	if existing != nil {
		if old, ok := existing.client.(*stdioTransport); ok {
			err := old.closeContext(ctx)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case <-old.processDone:
			default:
				return err
			}
		} else {
			_ = existing.client.Close()
		}
	}
	if !exists {
		return fmt.Errorf("unknown MCP server %q", name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var client transport
	switch config.Transport {
	case projectconfig.MCPTransportStdio:
		started, err := startStdio(config, manager.workspace)
		if err != nil {
			return err
		}
		client = started
	case projectconfig.MCPTransportStreamableHTTP:
		client = newHTTPTransport(name, config)
	default:
		return fmt.Errorf("MCP server %q has unsupported transport %q", name, config.Transport)
	}
	instance := &serverInstance{client: client, config: config, owner: manager}
	instance.category = model.ToolCategory{Name: "mcp", Description: "Tools provided by enabled MCP servers."}
	tools, err := manager.listTools(ctx, name, instance)
	if err != nil {
		_ = closeConnection(ctx, client)
		return err
	}
	instance.tools = tools
	manager.mu.Lock()
	if manager.closed || manager.generations[name] != generation || ctx.Err() != nil {
		manager.mu.Unlock()
		_ = closeConnection(ctx, client)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("MCP connection attempt was invalidated")
	}
	registered := make([]*remoteTool, 0, len(tools))
	for _, remote := range tools {
		remote.server = instance
		metadata := model.ToolMetadata{Category: instance.category, PermissionClass: model.ToolPermissionNetwork, ReadOnly: remote.readOnly}
		if err := manager.registry.Register(tool.ToolRegistration{Tool: remote, Metadata: metadata}); err != nil {
			for _, registeredTool := range registered {
				manager.registry.Unregister(registeredTool.registeredID)
			}
			manager.mu.Unlock()
			_ = closeConnection(ctx, client)
			return fmt.Errorf("register MCP tool %q: %w", remote.remoteName, err)
		}
		registered = append(registered, remote)
	}
	manager.instances[name] = instance
	if force {
		manager.enabled[name] = true
	}
	delete(manager.errors, name)
	manager.mu.Unlock()
	return nil
}

func closeConnection(ctx context.Context, client transport) error {
	if stdio, ok := client.(*stdioTransport); ok {
		return stdio.closeContext(ctx)
	}
	return client.Close()
}

func instanceClient(instance *serverInstance) transport {
	if instance == nil {
		return nil
	}
	return instance.client
}

func (manager *Manager) Authorize(ctx context.Context, name string) error {
	manager.mu.Lock()
	config, exists := manager.servers[name]
	instance := manager.instances[name]
	manager.mu.Unlock()
	if !exists {
		return fmt.Errorf("unknown MCP server %q", name)
	}
	if config.Transport != projectconfig.MCPTransportStreamableHTTP {
		return errors.New("OAuth authorization is only available for Streamable HTTP servers")
	}
	var client *httpTransport
	if instance != nil {
		client, _ = instance.client.(*httpTransport)
	}
	if client == nil {
		client = newHTTPTransport(name, config)
	}
	return client.Authorize(ctx)
}

func (manager *Manager) unregister(instance *serverInstance) {
	if instance == nil {
		return
	}
	for _, remote := range instance.tools {
		manager.registry.Unregister(remote.registeredID)
	}
}

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
