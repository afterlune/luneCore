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
		"runtime":{"contextWindow":64000,"reserveOutputTokens":4096,"autoCompact":true,"autoCompactThreshold":0.7},
		"attachments":{"maxFiles":3,"maxFileBytes":1048576,"maxTotalBytes":2097152},
		"mcpServers":{"docs":{"transport":"stdio","command":"docs-mcp"}}
	}`)
	supplied := &publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Profiles:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model", ContextWindow: 128000}},
		DefaultProfile: "main",
	}

	loaded, err := LoadForRuntime(filepath.Dir(path), supplied, nil, nil, 2)
	if err != nil {
		t.Fatalf("LoadForRuntime: %v", err)
	}
	if loaded.Runtime.ContextWindow != 64000 || loaded.Runtime.ReserveOutputTokens != 4096 || !loaded.Runtime.AutoCompact || loaded.Runtime.AutoCompactThreshold != 0.7 {
		t.Fatalf("runtime settings = %#v", loaded.Runtime)
	}
	if loaded.Attachments != (AttachmentLimits{MaxFiles: 3, MaxFileBytes: 1048576, MaxTotalBytes: 2097152}) {
		t.Fatalf("attachment settings = %#v", loaded.Attachments)
	}
	if len(loaded.MCPServers) != 1 || loaded.MCPServers["docs"].Command != "docs-mcp" {
		t.Fatalf("MCP settings = %#v", loaded.MCPServers)
	}
	if loaded.Models.DefaultProfile != "main" || loaded.Runtime.MaxParallelAgents != 2 || loaded.Models.Profiles["main"].Model != "host-model" {
		t.Fatalf("host model settings were not merged: %#v", loaded.ModelConfig())
	}
	if loaded.Models.Profiles["main"].ContextWindow != 128000 {
		t.Fatalf("host model profile context window was not merged: %d", loaded.Models.Profiles["main"].ContextWindow)
	}
}

func TestLoadForRuntimeReadsAndValidatesTeamTreeLimits(t *testing.T) {
	path := writeConfig(t, `{"runtime":{"maxTeamChildrenPerAgent":7,"maxTeamDepth":3}}`)
	supplied := &publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Profiles:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
		DefaultProfile: "main",
	}
	loaded, err := LoadForRuntime(filepath.Dir(path), supplied, nil, nil, 0)
	if err != nil {
		t.Fatalf("LoadForRuntime: %v", err)
	}
	if loaded.Runtime.MaxTeamChildrenPerAgent != 7 || loaded.Runtime.MaxTeamDepth != 3 {
		t.Fatalf("team limits = (%d, %d)", loaded.Runtime.MaxTeamChildrenPerAgent, loaded.Runtime.MaxTeamDepth)
	}

	for _, field := range []string{"maxTeamChildrenPerAgent", "maxTeamDepth"} {
		t.Run("negative "+field, func(t *testing.T) {
			negativePath := writeConfig(t, `{"runtime":{"`+field+`":-1}}`)
			if _, err := LoadForRuntime(filepath.Dir(negativePath), supplied, nil, nil, 0); err == nil {
				t.Fatalf("negative %s was accepted", field)
			}
		})
	}
}

func TestLoadForRuntimeHostSettingsOverrideMatchingProjectProfiles(t *testing.T) {
	path := writeConfig(t, `{
		"models":{"providers":{"vendor":{"protocol":"responses","baseURL":"https://project.example.test/v1","apiKey":"project-key"}},"profiles":{"main":{"provider":"vendor","model":"project-model"}},"defaultProfile":"main"},
		"runtime":{"contextWindow":32000}
	}`)
	supplied := &publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Profiles:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
		DefaultProfile: "main",
	}

	loaded, err := LoadForRuntime(filepath.Dir(path), supplied, nil, nil, 0)
	if err != nil {
		t.Fatalf("LoadForRuntime: %v", err)
	}
	if got := loaded.Models.Providers["vendor"]; got.BaseURL != "https://host.example.test/v1" || got.APIKey != "host-key" {
		t.Fatalf("provider override = %#v", got)
	}
	if got := loaded.Models.Profiles["main"].Model; got != "host-model" {
		t.Fatalf("model override = %q", got)
	}
	if loaded.Runtime.ContextWindow != 32000 {
		t.Fatalf("project runtime setting was lost: contextWindow=%d", loaded.Runtime.ContextWindow)
	}
}

func TestLoadForRuntimeHostProviderCanCompleteProjectCatalog(t *testing.T) {
	path := writeConfig(t, `{
		"models":{"profiles":{"main":{"provider":"host","model":"host-model"}},"defaultProfile":"main"},
		"runtime":{"contextWindow":64000}
	}`)
	provider := configTestHostProvider{name: "host"}

	loaded, err := LoadForRuntime(filepath.Dir(path), nil, map[string]publicmodel.Provider{"host": provider}, nil, 0)
	if err != nil {
		t.Fatalf("LoadForRuntime: %v", err)
	}
	if loaded.Models.Profiles["main"].Provider != "host" || loaded.Models.Providers["host"].APIKey != hostProviderPlaceholder {
		t.Fatalf("host provider config = %#v", loaded.Models.Providers["host"])
	}
}

func TestLoadForRuntimeStillRequiresProjectModelsWithoutHostSettings(t *testing.T) {
	path := writeConfig(t, `{"runtime":{"contextWindow":64000}}`)
	_, err := LoadForRuntime(filepath.Dir(path), nil, nil, nil, 0)
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
		{name: "unknown field", data: `{"runtime":{},"surprise":true}`, want: "unknown field"},
		{name: "invalid MCP server", data: `{"runtime":{},"mcpServers":{"docs":{"transport":"stdio"}}}`, want: "requires a non-empty command"},
		{name: "invalid attachment limit", data: `{"runtime":{},"attachments":{"maxFiles":-1}}`, want: "attachments.maxFiles must not be negative"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.data)
			supplied := &publicmodel.Config{
				Providers: map[string]publicmodel.ProviderConfig{
					"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
				},
				Profiles:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
				DefaultProfile: "main",
			}
			_, err := LoadForRuntime(filepath.Dir(path), supplied, nil, nil, 0)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadForRuntime error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadForRuntimeUsesHostProjectConfigWithoutReadingFile(t *testing.T) {
	path := writeConfig(t, `this is deliberately not JSON`)
	project := DefaultProjectConfig()
	project.Models = publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Profiles:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
		DefaultProfile: "main",
	}
	project.Runtime.ContextWindow = 64000
	project.Runtime.MaxTeamChildrenPerAgent = 8
	project.Runtime.MaxTeamDepth = 3
	project.Runtime.ReserveOutputTokens = 4096
	project.Attachments = AttachmentLimits{MaxFiles: 3}
	project.MCPServers = map[string]MCPServerConfig{
		"docs": {Transport: MCPTransportStdio, Command: "docs-mcp"},
	}

	loaded, err := LoadForRuntime(filepath.Dir(path), nil, nil, &project, 0)
	if err != nil {
		t.Fatalf("LoadForRuntime with host project config: %v", err)
	}
	if loaded.ModelConfig().DefaultModel != "main" || loaded.Runtime.ContextWindow != 64000 || loaded.Runtime.ReserveOutputTokens != 4096 || loaded.Runtime.MaxTeamChildrenPerAgent != 8 || loaded.Runtime.MaxTeamDepth != 3 {
		t.Fatalf("host project config was not applied: %#v", loaded)
	}
	if loaded.Attachments.MaxFiles != 3 || loaded.MCPServers["docs"].Command != "docs-mcp" {
		t.Fatalf("host runtime settings were not applied: %#v", loaded)
	}
}

func TestLoadForRuntimeValidatesHostProjectConfig(t *testing.T) {
	path := writeConfig(t, `{}`)
	project := DefaultProjectConfig()
	project.Models = publicmodel.Config{
		Providers: map[string]publicmodel.ProviderConfig{
			"vendor": {Protocol: publicmodel.ProtocolResponses, BaseURL: "https://host.example.test/v1", APIKey: "host-key"},
		},
		Profiles:       map[string]publicmodel.ModelProfile{"main": {Provider: "vendor", Model: "host-model"}},
		DefaultProfile: "main",
	}
	project.Runtime.ContextWindow = -1
	if _, err := LoadForRuntime(filepath.Dir(path), nil, nil, &project, 0); err == nil || !strings.Contains(err.Error(), "runtime.contextWindow must not be negative") {
		t.Fatalf("LoadForRuntime error = %v, want project runtime validation", err)
	}
}
