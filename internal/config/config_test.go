package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
)

func TestValidateRejectsNonFiniteAutoCompactThreshold(t *testing.T) {
	base := Config{
		Providers: map[string]model.ProviderConfig{
			"provider": {Protocol: model.ProtocolResponses, BaseURL: "https://example.test", APIKey: "key"},
		},
		Models:       map[string]model.ModelProfile{"main": {Provider: "provider", Model: "model"}},
		DefaultModel: "main",
	}
	for _, threshold := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		config := base
		config.AutoCompactThreshold = threshold
		if err := config.Validate(); err == nil {
			t.Errorf("Validate accepted non-finite autoCompactThreshold %v", threshold)
		}
	}
}

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadProjectConfigAndApplyDefaults(t *testing.T) {
	path := writeConfig(t, `{
		"providers": {"vendor-a": {"protocol":"responses","baseURL":"https://example.com","apiKey":"key","serverCompaction":true}},
		"models": {"lead": {"provider":"vendor-a","model":"model-a"}},
		"defaultModel":"lead",
		"contextWindow": 128000,
		"autoCompact": true,
		"reserveOutputTokens": 4096,
		"attachments": {"maxFiles": 4, "maxFileBytes": 1048576, "maxTotalBytes": 4194304}
	}`)
	config, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.ContextWindow != 128000 || config.ReserveOutputTokens != 4096 {
		t.Fatalf("context settings = (%d, %d)", config.ContextWindow, config.ReserveOutputTokens)
	}
	if !config.Providers["vendor-a"].ServerCompaction {
		t.Fatal("serverCompaction capability was not loaded")
	}
	if !config.AutoCompact || config.AutoCompactThreshold != 0.8 {
		t.Fatalf("auto compact = (%v, %v), want true and 0.8", config.AutoCompact, config.AutoCompactThreshold)
	}
	if config.Attachments.MaxFiles != 4 || config.Attachments.MaxFileBytes != 1048576 || config.Attachments.MaxTotalBytes != 4194304 {
		t.Fatalf("attachment limits = %#v", config.Attachments)
	}
	modelConfig := config.ModelConfig()
	if modelConfig.DefaultModel != "lead" || modelConfig.ParallelLimit() != model.DefaultMaxParallelAgents {
		t.Fatalf("model config = %#v", modelConfig)
	}
}

func TestLoadMCPServerConfigsWithoutStartingServers(t *testing.T) {
	path := writeConfig(t, `{
		"providers":{"deepseek":{"protocol":"responses","baseURL":"https://example.com","apiKey":"key"}},
		"models":{"lead":{"provider":"deepseek","model":"model-a"}},
		"defaultModel":"lead",
		"mcpServers":{
			"local-files":{"transport":"stdio","command":"mcp-files","args":["--root","./data"],"env":{"MCP_TOKEN":"${MCP_TOKEN}"}},
			"remote-docs":{"transport":"streamable-http","url":"https://mcp.example.test/v1/mcp?region=west","clientMetadataURL":"https://client.example.test/oauth/lune.json"}
		}
	}`)
	config, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	stdio := config.MCPServers["local-files"]
	if stdio.Transport != MCPTransportStdio || stdio.Command != "mcp-files" || len(stdio.Args) != 2 || stdio.Env["MCP_TOKEN"] != "${MCP_TOKEN}" {
		t.Fatalf("stdio config = %#v; env values must remain literal", stdio)
	}
	http := config.MCPServers["remote-docs"]
	if http.Transport != MCPTransportStreamableHTTP || http.URL != "https://mcp.example.test/v1/mcp?region=west" || http.ClientMetadataURL != "https://client.example.test/oauth/lune.json" {
		t.Fatalf("streamable HTTP config = %#v", http)
	}
}

func TestLoadProjectConfigEnablesAutoCompactionByDefault(t *testing.T) {
	config, err := Load(writeConfig(t, `{
		"providers":{"deepseek":{"protocol":"responses","baseURL":"https://example.com","apiKey":"key"}},
		"models":{"lead":{"provider":"deepseek","model":"model-a"}},
		"defaultModel":"lead"
	}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.ContextWindow != 0 || config.ReserveOutputTokens != 0 || !config.AutoCompact || config.AutoCompactThreshold != 0.8 {
		t.Fatalf("project defaults = %#v, want auto compaction enabled at 0.8 with no guessed context window", config)
	}
	if config.Attachments != (AttachmentLimits{}) {
		t.Fatalf("unset attachment configuration = %#v", config.Attachments)
	}
}

func TestLoadProjectConfigPreservesExplicitAutoCompactOff(t *testing.T) {
	config, err := Load(writeConfig(t, `{
		"providers":{"deepseek":{"protocol":"responses","baseURL":"https://example.com","apiKey":"key"}},
		"models":{"lead":{"provider":"deepseek","model":"model-a"}},
		"defaultModel":"lead",
		"autoCompact":false
	}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.AutoCompact || config.AutoCompactThreshold != 0 {
		t.Fatalf("explicit autoCompact:false changed to %#v", config)
	}
}

func TestLoadMigratesLegacyAPIKeyAndAllowsProjectSettings(t *testing.T) {
	config, err := Load(writeConfig(t, `{"apikey":"old-key","contextWindow":64000,"autoCompact":true}`))
	if err != nil {
		t.Fatalf("Load legacy config: %v", err)
	}
	if config.DefaultModel != model.DeepSeekFlashModel || config.Providers[model.ProviderDeepSeek].APIKey != "old-key" {
		t.Fatalf("legacy config migration = %#v", config)
	}
	if config.ContextWindow != 64000 || config.AutoCompactThreshold != 0.8 {
		t.Fatalf("project settings after legacy migration = %#v", config)
	}
}

func TestLoadRejectsUnknownAndInvalidFields(t *testing.T) {
	base := `"providers":{"deepseek":{"protocol":"responses","baseURL":"https://example.com","apiKey":"key"}},"models":{"lead":{"provider":"deepseek","model":"m"}},"defaultModel":"lead"`
	cases := []struct {
		name string
		data string
		want string
	}{
		{"unknown top-level", `{` + base + `,"mispelled":true}`, "unknown config field"},
		{"unknown MCP server field", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":"mcp","unexpected":true}}}`, "unknown field"},
		{"unsupported MCP transport", `{` + base + `,"mcpServers":{"server":{"transport":"sse","url":"https://mcp.example.test"}}}`, "transport must be"},
		{"stdio requires command", `{` + base + `,"mcpServers":{"server":{"transport":"stdio"}}}`, "requires a non-empty command"},
		{"stdio command must not be padded", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":" mcp "}}}`, "command must not contain surrounding whitespace"},
		{"stdio rejects URL", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":"mcp","url":"https://mcp.example.test"}}}`, "only valid for streamable-http"},
		{"streamable HTTP requires URL", `{` + base + `,"mcpServers":{"server":{"transport":"streamable-http"}}}`, "a URL is required"},
		{"streamable HTTP rejects stdio fields", `{` + base + `,"mcpServers":{"server":{"transport":"streamable-http","url":"https://mcp.example.test","args":[]}}}`, "only valid for stdio"},
		{"streamable HTTP rejects relative URL", `{` + base + `,"mcpServers":{"server":{"transport":"streamable-http","url":"/mcp"}}}`, "absolute http or https URL"},
		{"streamable HTTP rejects userinfo", `{` + base + `,"mcpServers":{"server":{"transport":"streamable-http","url":"https://user:secret@mcp.example.test/mcp"}}}`, "must not include userinfo"},
		{"streamable HTTP rejects fragment", `{` + base + `,"mcpServers":{"server":{"transport":"streamable-http","url":"https://mcp.example.test/mcp#fragment"}}}`, "must not include a fragment"},
		{"stdio rejects client metadata URL", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":"mcp","clientMetadataURL":"https://client.example.test/oauth/lune.json"}}}`, "only valid for streamable-http"},
		{"client metadata URL requires HTTPS", `{` + base + `,"mcpServers":{"server":{"transport":"streamable-http","url":"https://mcp.example.test","clientMetadataURL":"http://client.example.test/lune.json"}}}`, "absolute HTTPS URL"},
		{"client metadata URL requires path", `{` + base + `,"mcpServers":{"server":{"transport":"streamable-http","url":"https://mcp.example.test","clientMetadataURL":"https://client.example.test"}}}`, "absolute HTTPS URL"},
		{"client metadata URL rejects query", `{` + base + `,"mcpServers":{"server":{"transport":"streamable-http","url":"https://mcp.example.test","clientMetadataURL":"https://client.example.test/lune.json?x=1"}}}`, "no query"},
		{"MCP server name must not be padded", `{` + base + `,"mcpServers":{" server ":{"transport":"stdio","command":"mcp"}}}`, "keys must be non-empty and unpadded"},
		{"MCP command cannot contain NUL", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":"mcp\u0000server"}}}`, "command must not contain NUL"},
		{"MCP arg cannot contain NUL", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":"mcp","args":["bad\u0000arg"]}}}`, "args[0] must not contain NUL"},
		{"MCP env key cannot contain equals", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":"mcp","env":{"BAD=KEY":"value"}}}}`, "must not contain '=' or NUL"},
		{"MCP env key cannot contain NUL", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":"mcp","env":{"BAD\u0000KEY":"value"}}}}`, "must not contain '=' or NUL"},
		{"MCP env value cannot contain NUL", `{` + base + `,"mcpServers":{"server":{"transport":"stdio","command":"mcp","env":{"KEY":"bad\u0000value"}}}}`, "must not contain NUL"},
		{"unknown provider field", `{"providers":{"deepseek":{"protocol":"responses","baseURL":"https://example.com","apiKey":"key","endpoint":"x"}},"models":{"lead":{"provider":"deepseek","model":"m"}},"defaultModel":"lead"}`, "unknown field"},
		{"unknown attachment field", `{` + base + `,"attachments":{"maxFileSize":12}}`, "unknown field"},
		{"missing provider key", `{"providers":{"deepseek":{"protocol":"responses","baseURL":"https://example.com","apiKey":" "}},"models":{"lead":{"provider":"deepseek","model":"m"}},"defaultModel":"lead"}`, "non-empty apiKey"},
		{"missing messages output limit", `{"providers":{"anthropic":{"protocol":"anthropic-messages","baseURL":"https://example.com","apiKey":"key"}},"models":{"lead":{"provider":"anthropic","model":"m"}},"defaultModel":"lead"}`, "requires maxOutputTokens"},
		{"bad default profile", `{"providers":{"deepseek":{"protocol":"responses","baseURL":"https://example.com","apiKey":"key"}},"models":{"lead":{"provider":"deepseek","model":"m"}},"defaultModel":"other"}`, "does not name"},
		{"legacy plus model catalog", `{"apikey":"key","providers":{"deepseek":{"protocol":"responses","baseURL":"https://example.com","apiKey":"key"}}}`, "cannot be combined"},
		{"negative context window", `{` + base + `,"contextWindow":-1}`, "contextWindow must not be negative"},
		{"negative reserved tokens", `{` + base + `,"reserveOutputTokens":-1}`, "reserveOutputTokens must not be negative"},
		{"reserved tokens exceed window", `{` + base + `,"contextWindow":1000,"reserveOutputTokens":1000}`, "must be smaller than contextWindow"},
		{"threshold below zero", `{` + base + `,"autoCompactThreshold":-0.1}`, "between 0 and 1"},
		{"threshold above one", `{` + base + `,"autoCompactThreshold":1.1}`, "between 0 and 1"},
		{"negative max files", `{` + base + `,"attachments":{"maxFiles":-1}}`, "attachments.maxFiles must not be negative"},
		{"negative file size", `{` + base + `,"attachments":{"maxFileBytes":-1}}`, "attachments.maxFileBytes must not be negative"},
		{"negative total size", `{` + base + `,"attachments":{"maxTotalBytes":-1}}`, "attachments.maxTotalBytes must not be negative"},
		{"wrong field type", `{` + base + `,"contextWindow":"large"}`, "cannot unmarshal string"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, test.data))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsTrailingJSON(t *testing.T) {
	data := `{"apikey":"key"} {}`
	_, err := Load(writeConfig(t, data))
	if err == nil || !strings.Contains(err.Error(), "expected one JSON object") {
		t.Fatalf("Load error = %v, want trailing JSON error", err)
	}
}
