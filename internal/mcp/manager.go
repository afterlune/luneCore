package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
)

const maxMRTRRounds = 5

type Limits struct {
	MaxBinaryBlocks int
	MaxFileBytes    int64
	MaxTotalBytes   int64
}

type InteractionHandler func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)

type Options struct {
	Workspace          string
	StateDir           string
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
	mu        sync.Mutex
	connectMu sync.Mutex
	workspace string
	stateFile string
	servers   map[string]projectconfig.MCPServerConfig
	registry  *tool.Registry
	limits    Limits
	interact  InteractionHandler
	enabled   map[string]bool
	instances map[string]*serverInstance
	errors    map[string]string
	closed    bool
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

var toolNameRunes = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func NewManager(options Options) (*Manager, error) {
	if options.Registry == nil {
		return nil, errors.New("MCP manager requires a tool registry")
	}
	if options.Workspace == "" || options.StateDir == "" {
		return nil, errors.New("MCP manager requires workspace and state directory")
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
		workspace: options.Workspace, stateFile: filepath.Join(options.StateDir, "mcp-enabled.json"),
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
	manager.mu.Lock()
	manager.enabled[name] = true
	delete(manager.errors, name)
	manager.mu.Unlock()
	if err := manager.writeEnabled(); err != nil {
		manager.mu.Lock()
		manager.enabled[name] = false
		instance := manager.instances[name]
		delete(manager.instances, name)
		manager.mu.Unlock()
		manager.unregister(instance)
		if instance != nil {
			_ = instance.client.Close()
		}
		return err
	}
	return nil
}

func (manager *Manager) Disable(name string) error {
	manager.mu.Lock()
	instance := manager.instances[name]
	delete(manager.instances, name)
	delete(manager.enabled, name)
	delete(manager.errors, name)
	manager.mu.Unlock()
	manager.unregister(instance)
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
	instances := make([]*serverInstance, 0, len(manager.instances))
	for _, instance := range manager.instances {
		instances = append(instances, instance)
	}
	manager.instances = make(map[string]*serverInstance)
	manager.mu.Unlock()
	var failures []error
	for _, instance := range instances {
		manager.unregister(instance)
		if err := instance.client.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (manager *Manager) connect(name string) error {
	return manager.connectServer(name, false)
}

func (manager *Manager) reconnect(name string) error {
	return manager.connectServer(name, true)
}

func (manager *Manager) connectServer(name string, force bool) error {
	manager.connectMu.Lock()
	defer manager.connectMu.Unlock()
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return errors.New("MCP manager is closed")
	}
	existing := manager.instances[name]
	if existing != nil {
		stdio, isStdio := existing.client.(*stdioTransport)
		if !force && (!isStdio || stdio.Alive()) {
			manager.mu.Unlock()
			return nil
		}
		delete(manager.instances, name)
	}
	config, exists := manager.servers[name]
	manager.mu.Unlock()
	if existing != nil {
		manager.unregister(existing)
		_ = existing.client.Close()
	}
	if !exists {
		return fmt.Errorf("unknown MCP server %q", name)
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
	listContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tools, err := manager.listTools(listContext, name, instance)
	if err != nil {
		_ = client.Close()
		return err
	}
	instance.tools = tools
	registered := make([]*remoteTool, 0, len(tools))
	for _, remote := range tools {
		remote.server = instance
		metadata := model.ToolMetadata{Category: instance.category, PermissionClass: model.ToolPermissionNetwork, ReadOnly: remote.readOnly}
		if err := manager.registry.Register(tool.ToolRegistration{Tool: remote, Metadata: metadata}); err != nil {
			for _, tool := range registered {
				manager.registry.Unregister(tool.registeredID)
			}
			_ = client.Close()
			return fmt.Errorf("register MCP tool %q: %w", remote.remoteName, err)
		}
		registered = append(registered, remote)
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		for _, remote := range registered {
			manager.registry.Unregister(remote.registeredID)
		}
		_ = client.Close()
		return errors.New("MCP manager closed while connecting")
	}
	manager.instances[name] = instance
	delete(manager.errors, name)
	manager.mu.Unlock()
	return nil
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

// OpenElicitationURL opens a server-provided URL only after the front end has
// obtained explicit confirmation. The URL is used as an exec argument, never
// interpolated into a shell command.
func (manager *Manager) OpenElicitationURL(serverName, target string) error {
	manager.mu.Lock()
	instance := manager.instances[serverName]
	enabled := manager.enabled[serverName]
	manager.mu.Unlock()
	if !enabled || instance == nil {
		return fmt.Errorf("MCP server %q is not enabled", serverName)
	}
	if strings.TrimSpace(target) != target || strings.ContainsAny(target, "\r\n\t ") {
		return errors.New("MCP elicitation URL contains whitespace")
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" || parsed.User != nil || !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return errors.New("MCP elicitation URL must be an absolute http or https URL without userinfo")
	}
	return openOAuthBrowser(target)
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
			slog.Warn("ignoring enabled MCP server missing from project config", "server", name)
		}
	}
	return nil
}

func (manager *Manager) writeEnabled() error {
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

func (manager *Manager) listTools(ctx context.Context, serverName string, instance *serverInstance) ([]*remoteTool, error) {
	var cursor any
	seen := make(map[string]bool)
	tools := make([]*remoteTool, 0)
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = cursor
		}
		data, err := instance.client.Request(ctx, "tools/list", params, "", nil)
		if err != nil {
			return nil, fmt.Errorf("list tools from MCP server %q: %w", serverName, err)
		}
		var response struct {
			Tools []struct {
				Name        string          `json:"name"`
				Title       string          `json:"title"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
				Annotations struct {
					ReadOnlyHint bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
			NextCursor any `json:"nextCursor"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("decode tools from MCP server %q: %w", serverName, err)
		}
		for _, definition := range response.Tools {
			if strings.TrimSpace(definition.Name) == "" || seen[definition.Name] {
				slog.Warn("ignoring MCP tool with an empty or duplicate name", "server", serverName)
				continue
			}
			seen[definition.Name] = true
			schema := definition.InputSchema
			if len(schema) == 0 || !json.Valid(schema) || !isObjectSchema(schema) {
				slog.Warn("ignoring MCP tool with an invalid input schema", "server", serverName, "tool", definition.Name)
				continue
			}
			var bindings []headerBinding
			if instance.config.Transport == projectconfig.MCPTransportStreamableHTTP {
				var valid bool
				bindings, valid = parseHeaderBindings(schema)
				if !valid {
					slog.Warn("ignoring MCP HTTP tool with invalid x-mcp-header schema annotation", "server", serverName, "tool", definition.Name)
					continue
				}
			}
			publicName := toolName(serverName, definition.Name)
			description := definition.Description
			if description == "" {
				description = definition.Title
			}
			description = fmt.Sprintf("MCP server %q tool %q. %s", serverName, definition.Name, description)
			tools = append(tools, &remoteTool{
				serverName: serverName, remoteName: definition.Name,
				definition: model.FunctionTool{Name: publicName, Description: description, Parameters: append(json.RawMessage(nil), schema...)},
				readOnly:   definition.Annotations.ReadOnlyHint, bindings: bindings,
				registeredID: publicName,
			})
		}
		if response.NextCursor == nil || response.NextCursor == "" {
			return tools, nil
		}
		cursorText, ok := response.NextCursor.(string)
		if !ok || cursorText == "" || cursorText == cursor {
			return nil, fmt.Errorf("MCP server %q returned an invalid pagination cursor", serverName)
		}
		cursor = cursorText
	}
	return nil, fmt.Errorf("MCP server %q returned more than 100 tools/list pages", serverName)
}

func isObjectSchema(raw json.RawMessage) bool {
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return false
	}
	if rawType, exists := schema["type"]; exists {
		var schemaType string
		if json.Unmarshal(rawType, &schemaType) != nil || schemaType != "object" {
			return false
		}
	}
	return true
}

func toolName(serverName, remoteName string) string {
	serverSlug := toolNameRunes.ReplaceAllString(serverName, "_")
	toolSlug := toolNameRunes.ReplaceAllString(remoteName, "_")
	serverSlug = strings.Trim(serverSlug, "_")
	toolSlug = strings.Trim(toolSlug, "_")
	if serverSlug == "" {
		serverSlug = "server"
	}
	if toolSlug == "" {
		toolSlug = "tool"
	}
	hash := sha256.Sum256([]byte(serverName + "\x00" + remoteName))
	suffix := hex.EncodeToString(hash[:4])
	room := 64 - len("mcp__") - 2 - len(suffix)
	serverLength := min(12, len(serverSlug), room-1)
	serverSlug = serverSlug[:serverLength]
	toolLength := min(32, len(toolSlug), room-serverLength-1)
	toolSlug = toolSlug[:toolLength]
	return "mcp__" + serverSlug + "__" + toolSlug + "_" + suffix
}

func (remote *remoteTool) Definition() model.FunctionTool { return remote.definition }

func (remote *remoteTool) Run(ctx context.Context, arguments json.RawMessage) tool.Result {
	if stdio, ok := instanceClient(remote.server).(*stdioTransport); ok && !stdio.Alive() {
		if remote.server.owner == nil {
			return tool.Result{Err: errors.New("MCP stdio server exited and cannot be restarted")}
		}
		if err := remote.server.owner.connect(remote.serverName); err != nil {
			remote.server.owner.mu.Lock()
			remote.server.owner.errors[remote.serverName] = err.Error()
			remote.server.owner.mu.Unlock()
			return tool.Result{Err: fmt.Errorf("MCP stdio server exited and restart failed: %w", err)}
		}
		return tool.Result{Err: errors.New("MCP stdio server restarted; retry with the refreshed tool definition")}
	}
	var values map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(arguments)))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil || values == nil {
		return tool.Result{Err: errors.New("MCP tool arguments must be a JSON object")}
	}
	params := map[string]any{"name": remote.remoteName, "arguments": values}
	data, err := remote.server.client.Request(ctx, "tools/call", params, remote.remoteName, remote.bindings)
	if err != nil {
		return tool.Result{Err: err}
	}
	data, err = remote.withMRTR(ctx, "tools/call", params, remote.remoteName, remote.bindings, data)
	if err != nil {
		return tool.Result{Err: err}
	}
	output, transcriptOnly, isError, err := remote.convertToolResult(ctx, data)
	if err != nil {
		return tool.Result{Err: err}
	}
	result := tool.Result{ModelOutput: &output, TranscriptOnly: transcriptOnly}
	if isError {
		result.Err = errors.New("MCP server reported that the tool call failed")
	}
	return result
}

func (remote *remoteTool) withMRTR(ctx context.Context, method string, originalParams map[string]any, name string, bindings []headerBinding, first json.RawMessage) (json.RawMessage, error) {
	response := first
	params := cloneMap(originalParams)
	for round := 0; round <= maxMRTRRounds; round++ {
		var result struct {
			ResultType    string                     `json:"resultType"`
			InputRequests map[string]mcpInputRequest `json:"inputRequests"`
			RequestState  *string                    `json:"requestState"`
		}
		if err := json.Unmarshal(response, &result); err != nil {
			return nil, err
		}
		if result.ResultType != "input_required" {
			return response, nil
		}
		if round == maxMRTRRounds {
			return nil, fmt.Errorf("MCP server %q exceeded the %d-round input limit", remote.serverName, maxMRTRRounds)
		}
		if len(result.InputRequests) == 0 {
			return nil, errors.New("MCP server requested input without any input requests")
		}
		responses := make(map[string]json.RawMessage, len(result.InputRequests))
		keys := make([]string, 0, len(result.InputRequests))
		for key := range result.InputRequests {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			request := result.InputRequests[key]
			if request.Method != "elicitation/create" {
				return nil, fmt.Errorf("MCP server %q requested unsupported interaction %q", remote.serverName, request.Method)
			}
			if remote.serverName == "" || remote.server == nil {
				return nil, errors.New("MCP server interaction is unavailable")
			}
			payload, err := json.Marshal(request)
			if err != nil {
				return nil, err
			}
			answer, err := remote.withInteraction(ctx, payload)
			if err != nil {
				return nil, err
			}
			if err := validateElicitationResponse(answer, request.Params); err != nil {
				return nil, err
			}
			responses[key] = answer
		}
		params["inputResponses"] = responses
		if result.RequestState != nil {
			params["requestState"] = *result.RequestState
		}
		var err error
		response, err = remote.server.client.Request(ctx, method, cloneMap(params), name, bindings)
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("MCP input retry limit reached")
}

type mcpInputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func validateElicitationResponse(raw, requestParams json.RawMessage) error {
	var response struct {
		Action  string          `json:"action"`
		Content json.RawMessage `json:"content"`
	}
	var request struct {
		Mode string `json:"mode"`
	}
	if len(raw) == 0 || !json.Valid(raw) {
		return errors.New("MCP elicitation response is invalid JSON")
	}
	if len(requestParams) == 0 || json.Unmarshal(requestParams, &request) != nil || request.Mode != "form" && request.Mode != "url" {
		return errors.New("MCP elicitation request has an unsupported mode")
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return errors.New("MCP elicitation response is invalid JSON")
	}
	switch response.Action {
	case "accept":
		if request.Mode == "form" {
			var content map[string]json.RawMessage
			if len(response.Content) == 0 || json.Unmarshal(response.Content, &content) != nil || content == nil {
				return errors.New("accepted MCP form response requires a JSON object")
			}
		} else if len(response.Content) > 0 {
			var content map[string]json.RawMessage
			if json.Unmarshal(response.Content, &content) != nil || content == nil {
				return errors.New("accepted MCP URL response content must be a JSON object")
			}
		}
	case "decline", "cancel":
	default:
		return errors.New("MCP elicitation response action must be accept, decline, or cancel")
	}
	return nil
}

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (remote *remoteTool) convertToolResult(ctx context.Context, raw json.RawMessage) (model.ToolOutput, []model.ContentPart, bool, error) {
	var response struct {
		ResultType        string            `json:"resultType"`
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return model.ToolOutput{}, nil, false, fmt.Errorf("decode MCP tool result: %w", err)
	}
	parts := make([]model.ContentPart, 0)
	var transcript []model.ContentPart
	budget := binaryBudget{limits: remote.server.managerLimits()}
	if err := remote.convertContent(ctx, response.Content, 0, &budget, &parts, &transcript); err != nil {
		return model.ToolOutput{}, nil, false, err
	}
	if len(response.StructuredContent) > 0 && json.Valid(response.StructuredContent) {
		var formatted any
		if json.Unmarshal(response.StructuredContent, &formatted) == nil {
			data, _ := json.MarshalIndent(formatted, "", "  ")
			parts = append(parts, model.InputText{Text: "Structured content:\n" + string(data)})
		}
	}
	if len(parts) == 0 {
		parts = append(parts, model.InputText{Text: "MCP tool returned no model-visible content."})
	}
	return model.NewToolOutputParts(parts...), transcript, response.IsError, nil
}

func (instance *serverInstance) managerLimits() Limits {
	if instance == nil || instance.owner == nil {
		return Limits{MaxBinaryBlocks: 5, MaxFileBytes: 10 << 20, MaxTotalBytes: 20 << 20}
	}
	return instance.owner.limits
}

func (remote *remoteTool) convertContent(ctx context.Context, blocks []json.RawMessage, depth int, budget *binaryBudget, parts *[]model.ContentPart, transcript *[]model.ContentPart) error {
	for _, raw := range blocks {
		var block struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Data        string `json:"data"`
			MIMEType    string `json:"mimeType"`
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Annotations struct {
				Audience []string `json:"audience"`
			} `json:"annotations"`
			Resource *struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
				Blob     string `json:"blob"`
			} `json:"resource"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return fmt.Errorf("decode MCP content block: %w", err)
		}
		userOnly := hasAudience(block.Annotations.Audience, "user") && !hasAudience(block.Annotations.Audience, "assistant")
		switch block.Type {
		case "text":
			if userOnly {
				*transcript = append(*transcript, model.InputText{Text: block.Text})
			} else {
				*parts = append(*parts, model.InputText{Text: block.Text})
			}
		case "image", "audio", "resource":
			if block.Type == "resource" && block.Resource != nil {
				if block.Resource.Text != "" {
					if userOnly {
						*transcript = append(*transcript, model.InputText{Text: block.Resource.Text})
					} else {
						*parts = append(*parts, model.InputText{Text: block.Resource.Text})
					}
				}
				if block.Resource.Blob != "" {
					if err := remote.addBinary(block.Resource.Blob, block.Resource.MIMEType, userOnly, ctx, budget, parts, transcript); err != nil {
						return err
					}
				}
				continue
			}
			if err := remote.addBinary(block.Data, block.MIMEType, userOnly, ctx, budget, parts, transcript); err != nil {
				return err
			}
		case "resource_link":
			if block.URI == "" {
				return errors.New("MCP resource link did not include a URI")
			}
			if depth > 0 {
				marker := fmt.Sprintf("[Resource link: %s (%s)]", block.Name, block.URI)
				if userOnly {
					*transcript = append(*transcript, model.InputText{Text: marker})
				} else {
					*parts = append(*parts, model.InputText{Text: marker})
				}
				continue
			}
			params := map[string]any{"uri": block.URI}
			resource, err := remote.server.client.Request(ctx, "resources/read", params, block.URI, nil)
			if err != nil {
				return fmt.Errorf("read MCP resource %q: %w", block.URI, err)
			}
			resource, err = remote.withMRTR(ctx, "resources/read", params, block.URI, nil, resource)
			if err != nil {
				return fmt.Errorf("read MCP resource %q: %w", block.URI, err)
			}
			var read struct {
				Contents []json.RawMessage `json:"contents"`
			}
			if err := json.Unmarshal(resource, &read); err != nil {
				return fmt.Errorf("decode MCP resource %q: %w", block.URI, err)
			}
			if err := remote.convertReadContents(ctx, read.Contents, userOnly, budget, parts, transcript); err != nil {
				return err
			}
		default:
			marker := fmt.Sprintf("[Unsupported MCP content type: %s]", block.Type)
			if userOnly {
				*transcript = append(*transcript, model.InputText{Text: marker})
			} else {
				*parts = append(*parts, model.InputText{Text: marker})
			}
		}
	}
	return nil
}

func (remote *remoteTool) convertReadContents(ctx context.Context, items []json.RawMessage, userOnly bool, budget *binaryBudget, parts *[]model.ContentPart, transcript *[]model.ContentPart) error {
	for _, raw := range items {
		var resource struct {
			MIMEType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		}
		if err := json.Unmarshal(raw, &resource); err != nil {
			return fmt.Errorf("decode MCP resource content: %w", err)
		}
		if resource.Text != "" {
			if userOnly {
				*transcript = append(*transcript, model.InputText{Text: resource.Text})
			} else {
				*parts = append(*parts, model.InputText{Text: resource.Text})
			}
		}
		if resource.Blob != "" {
			if err := remote.addBinary(resource.Blob, resource.MIMEType, userOnly, ctx, budget, parts, transcript); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasAudience(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type binaryBudget struct {
	limits Limits
	blocks int
	total  int64
}

func (remote *remoteTool) addBinary(data, mimeType string, userOnly bool, ctx context.Context, budget *binaryBudget, parts *[]model.ContentPart, transcript *[]model.ContentPart) error {
	decoded, err := jsonBase64(data)
	if err != nil {
		return fmt.Errorf("decode MCP binary content: %w", err)
	}
	budget.blocks++
	budget.total += int64(len(decoded))
	if budget.blocks > budget.limits.MaxBinaryBlocks {
		return fmt.Errorf("MCP result exceeds the %d binary block limit", budget.limits.MaxBinaryBlocks)
	}
	if int64(len(decoded)) > budget.limits.MaxFileBytes {
		return fmt.Errorf("MCP binary block exceeds the %d byte limit", budget.limits.MaxFileBytes)
	}
	if budget.total > budget.limits.MaxTotalBytes {
		return fmt.Errorf("MCP result exceeds the %d byte aggregate limit", budget.limits.MaxTotalBytes)
	}
	if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
		mimeType = strings.ToLower(mediaType)
	} else {
		mimeType = "application/octet-stream"
	}
	if userOnly {
		*transcript = append(*transcript, model.InputText{Text: binaryMarker(mimeType, len(decoded))})
		if strings.HasPrefix(mimeType, "image/") {
			*transcript = append(*transcript, model.InputImage{ImageURL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(decoded)})
		} else if strings.HasPrefix(mimeType, "audio/") {
			if format := audioFormat(mimeType); format != "" {
				*transcript = append(*transcript, model.InputAudio{Data: base64.StdEncoding.EncodeToString(decoded), Format: format})
			}
		}
		return nil
	}
	if strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		encoded := base64.StdEncoding.EncodeToString(decoded)
		*parts = append(*parts, model.InputImage{ImageURL: "data:" + mimeType + ";base64," + encoded})
		return nil
	}
	if strings.HasPrefix(strings.ToLower(mimeType), "audio/") {
		if model.InputAudioSupported(ctx) {
			format := audioFormat(mimeType)
			if format != "" {
				*parts = append(*parts, model.InputAudio{Data: base64.StdEncoding.EncodeToString(decoded), Format: format})
			} else {
				*parts = append(*parts, model.InputText{Text: binaryMarker(mimeType, len(decoded)) + " (audio format is not supported by this model input type)"})
			}
		} else {
			*parts = append(*parts, model.InputText{Text: binaryMarker(mimeType, len(decoded)) + " (the active model profile does not support audio input)"})
		}
		return nil
	}
	*parts = append(*parts, model.InputText{Text: binaryMarker(mimeType, len(decoded))})
	return nil
}

func jsonBase64(value string) ([]byte, error) {
	if result, err := base64.StdEncoding.DecodeString(value); err == nil {
		return result, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

func binaryMarker(mimeType string, size int) string {
	return fmt.Sprintf("[Binary content: %s, %s]", mimeType, byteSize(int64(size)))
}

func byteSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	return fmt.Sprintf("%.1f KiB", float64(size)/1024)
}

func audioFormat(mimeType string) string {
	base := strings.ToLower(strings.SplitN(mimeType, ";", 2)[0])
	switch base {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	default:
		return ""
	}
}

func (remote *remoteTool) withInteraction(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	if remote.server == nil || remote.server.owner == nil || remote.server.owner.interact == nil {
		return nil, errors.New("MCP user interaction is unavailable")
	}
	return remote.server.owner.interact(ctx, model.AgentIDFromContext(ctx), remote.serverName, request)
}
