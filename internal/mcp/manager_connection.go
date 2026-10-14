package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

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
