package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// MCPTransport identifies an MCP server's configured transport.
type MCPTransport string

const (
	MCPTransportStdio          MCPTransport = "stdio"
	MCPTransportStreamableHTTP MCPTransport = "streamable-http"
)

// MCPServerConfig describes a server endpoint. Env values are literal strings;
// no environment-variable expansion is performed. Loading this data never
// starts a process or connects to the endpoint; the MCP runtime consumes it later.
type MCPServerConfig struct {
	Transport         MCPTransport      `json:"transport"`
	Command           string            `json:"command,omitempty"`           // Executable for stdio transport; not a shell command.
	Args              []string          `json:"args,omitempty"`              // Literal arguments passed to the stdio executable.
	Env               map[string]string `json:"env,omitempty"`               // Literal environment values for stdio; no expansion.
	URL               string            `json:"url,omitempty"`               // HTTP endpoint for streamable-http transport.
	ClientMetadataURL string            `json:"clientMetadataURL,omitempty"` // Public OAuth Client ID Metadata Document URL.
}

func validateMCPServers(servers map[string]MCPServerConfig) error {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		server := servers[name]
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("mcpServers keys must be non-empty and unpadded")
		}
		switch server.Transport {
		case MCPTransportStdio:
			if strings.TrimSpace(server.Command) == "" {
				return fmt.Errorf("mcpServers[%q] requires a non-empty command for stdio transport", name)
			}
			if strings.TrimSpace(server.Command) != server.Command {
				return fmt.Errorf("mcpServers[%q].command must not contain surrounding whitespace", name)
			}
			if strings.ContainsRune(server.Command, '\x00') {
				return fmt.Errorf("mcpServers[%q].command must not contain NUL", name)
			}
			if server.URL != "" {
				return fmt.Errorf("mcpServers[%q].url is only valid for streamable-http transport", name)
			}
			if server.ClientMetadataURL != "" {
				return fmt.Errorf("mcpServers[%q].clientMetadataURL is only valid for streamable-http transport", name)
			}
			for index, argument := range server.Args {
				if strings.ContainsRune(argument, '\x00') {
					return fmt.Errorf("mcpServers[%q].args[%d] must not contain NUL", name, index)
				}
			}
			if err := validateMCPServerEnv(name, server.Env); err != nil {
				return err
			}
		case MCPTransportStreamableHTTP:
			if server.Command != "" || server.Args != nil || server.Env != nil {
				return fmt.Errorf("mcpServers[%q] command, args, and env are only valid for stdio transport", name)
			}
			if err := validateMCPServerURL(server.URL); err != nil {
				return fmt.Errorf("mcpServers[%q].url: %w", name, err)
			}
			if server.ClientMetadataURL != "" {
				if err := validateMCPClientMetadataURL(server.ClientMetadataURL); err != nil {
					return fmt.Errorf("mcpServers[%q].clientMetadataURL: %w", name, err)
				}
			}
		default:
			return fmt.Errorf("mcpServers[%q].transport must be %q or %q", name, MCPTransportStdio, MCPTransportStreamableHTTP)
		}
	}
	return nil
}

func validateMCPClientMetadataURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Path == "" || parsed.Path == "/" || parsed.RawQuery != "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("must be an absolute HTTPS URL with a non-root path and no query, userinfo, or fragment")
	}
	return nil
}

func validateMCPServerEnv(name string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("mcpServers[%q].env keys must be non-empty and must not contain '=' or NUL", name)
		}
		if strings.ContainsRune(env[key], '\x00') {
			return fmt.Errorf("mcpServers[%q].env[%q] must not contain NUL", name, key)
		}
		for _, previous := range keys {
			if previous == key {
				break
			}
			if strings.EqualFold(previous, key) {
				return fmt.Errorf("mcpServers[%q].env keys must be unique regardless of case", name)
			}
		}
	}
	return nil
}

func validateMCPServerURL(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("a URL is required for streamable-http transport")
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return fmt.Errorf("must be an absolute http or https URL without surrounding or embedded whitespace")
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || (!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) || parsed.Host == "" {
		return fmt.Errorf("must be an absolute http or https URL with a host")
	}
	if parsed.User != nil {
		return fmt.Errorf("must not include userinfo")
	}
	if parsed.Fragment != "" || strings.Contains(value, "#") {
		return fmt.Errorf("must not include a fragment")
	}
	return nil
}
