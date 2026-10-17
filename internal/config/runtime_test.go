package config

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

type configTestHostProvider struct{ name string }

func (p configTestHostProvider) Name() string { return p.name }

func (configTestHostProvider) CreateResponse(context.Context, publicmodel.ModelRequest, publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	return nil, errors.New("unused test provider")
}

func (configTestHostProvider) StreamResponse(context.Context, publicmodel.ModelRequest, publicmodel.ModelProfile, func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	return nil, errors.New("unused test provider")
}

func TestLoadForRuntimeMergesHostModelsBeforeValidation(t *testing.T) {
	path := writeConfig(t, `{
		"contextWindow":64000,
		"reserveOutputTokens":4096,
		"autoCompact":true,
		"autoCompactThreshold":0.7,
		"attachments":{"maxFiles":3,"maxFileBytes":1048576,"maxTotalBytes":2097152},
		"mcpServers":{"docs":{"transport":"stdio","command":"docs-mcp"}}
	}`)
	supplied := &publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Models:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
		DefaultModel: "main", MaxParallelAgents: 2,
	}

	loaded, err := LoadForRuntime(filepath.Dir(path), supplied, nil, nil)
	if err != nil {
		t.Fatalf("LoadForRuntime: %v", err)
	}
	if loaded.ContextWindow != 64000 || loaded.ReserveOutputTokens != 4096 || !loaded.AutoCompact || loaded.AutoCompactThreshold != 0.7 {
		t.Fatalf("runtime settings = %#v", loaded)
	}
	if loaded.Attachments != (AttachmentLimits{MaxFiles: 3, MaxFileBytes: 1048576, MaxTotalBytes: 2097152}) {
		t.Fatalf("attachment settings = %#v", loaded.Attachments)
	}
	if len(loaded.MCPServers) != 1 || loaded.MCPServers["docs"].Command != "docs-mcp" {
		t.Fatalf("MCP settings = %#v", loaded.MCPServers)
	}
	if loaded.DefaultModel != "main" || loaded.MaxParallelAgents != 2 || loaded.Models["main"].Model != "host-model" {
		t.Fatalf("host model settings were not merged: %#v", loaded.ModelConfig())
	}
}

func TestLoadForRuntimeHostSettingsOverrideMatchingProjectProfiles(t *testing.T) {
	path := writeConfig(t, `{
		"providers":{"vendor":{"protocol":"responses","baseURL":"https://project.example.test/v1","apiKey":"project-key"}},
		"models":{"main":{"provider":"vendor","model":"project-model"}},
		"defaultModel":"main",
		"contextWindow":32000
	}`)
	supplied := &publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Models:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
		DefaultModel: "main",
	}

	loaded, err := LoadForRuntime(filepath.Dir(path), supplied, nil, nil)
	if err != nil {
		t.Fatalf("LoadForRuntime: %v", err)
	}
	if got := loaded.Providers["vendor"]; got.BaseURL != "https://host.example.test/v1" || got.APIKey != "host-key" {
		t.Fatalf("provider override = %#v", got)
	}
	if got := loaded.Models["main"].Model; got != "host-model" {
		t.Fatalf("model override = %q", got)
	}
	if loaded.ContextWindow != 32000 {
		t.Fatalf("project runtime setting was lost: contextWindow=%d", loaded.ContextWindow)
	}
}

func TestLoadForRuntimeHostProviderCanCompleteProjectCatalog(t *testing.T) {
	path := writeConfig(t, `{
		"models":{"main":{"provider":"host","model":"host-model"}},
		"defaultModel":"main",
		"contextWindow":64000
	}`)
	provider := configTestHostProvider{name: "host"}

	loaded, err := LoadForRuntime(filepath.Dir(path), nil, map[string]publicmodel.Provider{"host": provider}, nil)
	if err != nil {
		t.Fatalf("LoadForRuntime: %v", err)
	}
	if loaded.Models["main"].Provider != "host" || loaded.Providers["host"].APIKey != hostProviderPlaceholder {
		t.Fatalf("host provider config = %#v", loaded.Providers["host"])
	}
}

func TestLoadForRuntimeStillRequiresProjectModelsWithoutHostSettings(t *testing.T) {
	path := writeConfig(t, `{"contextWindow":64000}`)
	_, err := LoadForRuntime(filepath.Dir(path), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "at least one provider") {
		t.Fatalf("LoadForRuntime error = %v, want missing model catalog", err)
	}
}

func TestLoadForRuntimeStillValidatesProjectRuntimeSchema(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown field", data: `{"surprise":true}`, want: "unknown config field"},
		{name: "invalid MCP server", data: `{"mcpServers":{"docs":{"transport":"stdio"}}}`, want: "requires a non-empty command"},
		{name: "invalid attachment limit", data: `{"attachments":{"maxFiles":-1}}`, want: "attachments.maxFiles must not be negative"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.data)
			supplied := &publicmodel.Config{
				Providers: map[string]publicmodel.ProviderConfig{
					"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
				},
				Models:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
				DefaultModel: "main",
			}
			_, err := LoadForRuntime(filepath.Dir(path), supplied, nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadForRuntime error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadForRuntimeUsesHostProjectConfigWithoutReadingFile(t *testing.T) {
	path := writeConfig(t, `this is deliberately not JSON`)
	project := DefaultProjectConfig()
	project.ModelConfig = publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Models:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
		DefaultModel: "main",
	}
	project.ContextWindow = 64000
	project.ReserveOutputTokens = 4096
	project.Attachments = AttachmentLimits{MaxFiles: 3}
	project.MCPServers = map[string]MCPServerConfig{
		"docs": {Transport: MCPTransportStdio, Command: "docs-mcp"},
	}

	loaded, err := LoadForRuntime(filepath.Dir(path), nil, nil, &project)
	if err != nil {
		t.Fatalf("LoadForRuntime with host project config: %v", err)
	}
	if loaded.ModelConfig().DefaultModel != "main" || loaded.ContextWindow != 64000 || loaded.ReserveOutputTokens != 4096 {
		t.Fatalf("host project config was not applied: %#v", loaded)
	}
	if loaded.Attachments.MaxFiles != 3 || loaded.MCPServers["docs"].Command != "docs-mcp" {
		t.Fatalf("host runtime settings were not applied: %#v", loaded)
	}
}

func TestLoadForRuntimeValidatesHostProjectConfig(t *testing.T) {
	path := writeConfig(t, `{}`)
	project := DefaultProjectConfig()
	project.ModelConfig = publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Models:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
		DefaultModel: "main",
	}
	project.ContextWindow = -1
	if _, err := LoadForRuntime(filepath.Dir(path), nil, nil, &project); err == nil || !strings.Contains(err.Error(), "contextWindow must not be negative") {
		t.Fatalf("LoadForRuntime error = %v, want project runtime validation", err)
	}
}
