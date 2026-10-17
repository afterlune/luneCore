// Package config loads and validates the workspace's project configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
)

const (
	FileName = "config.json"
	// DefaultAutoCompactThreshold is used when auto compaction is enabled and no threshold is configured.
	DefaultAutoCompactThreshold          = 0.8
	DefaultAttachmentMaxFiles            = 5
	DefaultAttachmentMaxFileBytes  int64 = 10 << 20
	DefaultAttachmentMaxTotalBytes int64 = 20 << 20
)

// Config is the workspace's project configuration. Provider and model fields
// retain their existing JSON shapes; other settings are optional additions.
type Config struct {
	Providers            map[string]model.ProviderConfig `json:"providers"`
	Models               map[string]model.ModelProfile   `json:"models"`
	DefaultModel         string                          `json:"defaultModel"`
	MaxParallelAgents    int                             `json:"maxParallelAgents,omitempty"`
	ContextWindow        int64                           `json:"contextWindow,omitempty"`        // Tokens; zero means unset.
	AutoCompact          bool                            `json:"autoCompact"`                    // Defaults to true; set false to disable.
	AutoCompactThreshold float64                         `json:"autoCompactThreshold,omitempty"` // Ratio from 0 to 1; defaults to 0.8 when enabled.
	ReserveOutputTokens  int64                           `json:"reserveOutputTokens,omitempty"`  // Tokens; zero means unset.
	Attachments          AttachmentLimits                `json:"attachments,omitempty"`
	MCPServers           map[string]MCPServerConfig      `json:"mcpServers,omitempty"`
	legacyAPIKey         string
}

// ProjectConfig is the complete project configuration supplied by a host.
// Hosts may use this instead of loading config.json themselves.
type ProjectConfig struct {
	ModelConfig          publicmodel.Config         `json:"modelConfig"`
	ContextWindow        int64                      `json:"contextWindow,omitempty"`
	AutoCompact          bool                       `json:"autoCompact"`
	AutoCompactThreshold float64                    `json:"autoCompactThreshold,omitempty"`
	ReserveOutputTokens  int64                      `json:"reserveOutputTokens,omitempty"`
	Attachments          AttachmentLimits           `json:"attachments,omitempty"`
	MCPServers           map[string]MCPServerConfig `json:"mcpServers,omitempty"`
}

// DefaultProjectConfig returns the same runtime defaults used by config.json.
func DefaultProjectConfig() ProjectConfig {
	return ProjectConfig{
		ModelConfig: publicmodel.Config{
			Providers: map[string]publicmodel.ProviderConfig{},
			Models:    map[string]publicmodel.ModelProfile{},
		},
		AutoCompact: true, AutoCompactThreshold: DefaultAutoCompactThreshold,
		MCPServers: map[string]MCPServerConfig{},
	}
}

func (c ProjectConfig) toConfig() Config {
	providers := make(map[string]model.ProviderConfig, len(c.ModelConfig.Providers))
	for name, provider := range c.ModelConfig.Providers {
		providers[name] = model.ProviderConfig{
			Protocol: provider.Protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey,
			ServerCompaction: provider.ServerCompaction,
		}
	}
	profiles := make(map[string]model.ModelProfile, len(c.ModelConfig.Models))
	for name, profile := range c.ModelConfig.Models {
		profiles[name] = profile
	}
	return Config{
		Providers: providers, Models: profiles, DefaultModel: c.ModelConfig.DefaultModel,
		MaxParallelAgents: c.ModelConfig.MaxParallelAgents,
		ContextWindow:     c.ContextWindow, AutoCompact: c.AutoCompact,
		AutoCompactThreshold: c.AutoCompactThreshold, ReserveOutputTokens: c.ReserveOutputTokens,
		Attachments: c.Attachments, MCPServers: c.MCPServers,
	}
}

// Validate checks the complete host-supplied project configuration.
func (c ProjectConfig) Validate() error {
	config := c.toConfig()
	config.applyDefaults()
	return config.Validate()
}

// AttachmentLimits configures image attachments accepted by Core task input.
// Zero means there is no explicit project override; Core applies safe byte
// defaults to inline image data while leaving remote image URLs unchanged.
type AttachmentLimits struct {
	MaxFiles      int   `json:"maxFiles,omitempty"`
	MaxFileBytes  int64 `json:"maxFileBytes,omitempty"`
	MaxTotalBytes int64 `json:"maxTotalBytes,omitempty"`
}

// Load reads and validates one project config file.
func Load(path string) (Config, error) {
	return load(path, true)
}

// load decodes and validates the project configuration. Hosts that provide
// model settings or providers defer model-catalog validation until those
// settings have been merged, while still validating project runtime settings.
func load(path string, validateModelCatalog bool) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("decode %s: expected one JSON object", path)
	}
	if config.legacyAPIKey != "" {
		if len(config.Providers) != 0 || len(config.Models) != 0 || config.DefaultModel != "" {
			return Config{}, fmt.Errorf("%s: legacy apikey cannot be combined with providers or models", path)
		}
		config.Providers = map[string]model.ProviderConfig{model.ProviderDeepSeek: {
			Protocol: model.ProtocolResponses, BaseURL: model.DeepSeekResponsesBaseURL, APIKey: config.legacyAPIKey,
		}}
		config.Models = map[string]model.ModelProfile{
			model.DeepSeekFlashModel: {Provider: model.ProviderDeepSeek, Model: model.DeepSeekFlashModel},
		}
		config.DefaultModel = model.DeepSeekFlashModel
	}
	config.applyDefaults()
	var validationErr error
	if validateModelCatalog {
		validationErr = config.Validate()
	} else {
		validationErr = config.validateRuntimeSettings()
	}
	if validationErr != nil {
		return Config{}, fmt.Errorf("validate %s: %w", path, validationErr)
	}
	return config, nil
}

// UnmarshalJSON accepts the old single-provider apikey input and rejects
// unknown fields at the project and nested configuration levels.
func (c *Config) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	legacyAPIKey := ""
	if raw, exists := fields["apikey"]; exists {
		if err := json.Unmarshal(raw, &legacyAPIKey); err != nil {
			return fmt.Errorf("apikey must be a string: %w", err)
		}
		delete(fields, "apikey")
	}
	for key := range fields {
		switch key {
		case "providers", "models", "defaultModel", "maxParallelAgents", "contextWindow", "autoCompact", "autoCompactThreshold", "reserveOutputTokens", "attachments", "mcpServers":
		default:
			return fmt.Errorf("unknown config field %q", key)
		}
	}
	cleaned, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	type plain Config
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(cleaned))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*c = Config(decoded)
	if _, explicitlySet := fields["autoCompact"]; !explicitlySet {
		c.AutoCompact = true
	}
	c.legacyAPIKey = legacyAPIKey
	return nil
}

// Validate checks project settings, the model catalog, and MCP server configs.
func (c *Config) applyDefaults() {
	if c.AutoCompact && c.AutoCompactThreshold == 0 {
		c.AutoCompactThreshold = DefaultAutoCompactThreshold
	}
}

func (c Config) Validate() error {
	if err := c.ModelConfig().Validate(); err != nil {
		return err
	}
	return c.validateRuntimeSettings()
}

func (c Config) validateRuntimeSettings() error {
	if err := validateMCPServers(c.MCPServers); err != nil {
		return err
	}
	if c.ContextWindow < 0 {
		return errors.New("contextWindow must not be negative")
	}
	if c.ReserveOutputTokens < 0 {
		return errors.New("reserveOutputTokens must not be negative")
	}
	if c.ContextWindow > 0 && c.ReserveOutputTokens >= c.ContextWindow {
		return errors.New("reserveOutputTokens must be smaller than contextWindow")
	}
	if math.IsNaN(c.AutoCompactThreshold) || math.IsInf(c.AutoCompactThreshold, 0) ||
		c.AutoCompactThreshold < 0 || c.AutoCompactThreshold > 1 {
		return errors.New("autoCompactThreshold must be between 0 and 1; 0 means unset")
	}
	if c.Attachments.MaxFiles < 0 {
		return errors.New("attachments.maxFiles must not be negative")
	}
	if c.Attachments.MaxFileBytes < 0 {
		return errors.New("attachments.maxFileBytes must not be negative")
	}
	if c.Attachments.MaxTotalBytes < 0 {
		return errors.New("attachments.maxTotalBytes must not be negative")
	}
	return nil
}

// ModelConfig returns the model catalog portion consumed by model providers.
func (c Config) ModelConfig() model.Config {
	return model.Config{
		Providers:         c.Providers,
		Models:            c.Models,
		DefaultModel:      c.DefaultModel,
		MaxParallelAgents: c.MaxParallelAgents,
	}
}
