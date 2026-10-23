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

// RuntimeConfig contains project-level task defaults and execution limits.
type RuntimeConfig struct {
	MaxParallelAgents       int     `json:"maxParallelAgents,omitempty"`
	MaxTeamChildrenPerAgent int     `json:"maxTeamChildrenPerAgent,omitempty"`
	MaxTeamDepth            int     `json:"maxTeamDepth,omitempty"`
	ContextWindow           int64   `json:"contextWindow,omitempty"`
	AutoCompact             bool    `json:"autoCompact"`
	AutoCompactThreshold    float64 `json:"autoCompactThreshold,omitempty"`
	ReserveOutputTokens     int64   `json:"reserveOutputTokens,omitempty"`
}

// ProjectConfig is the canonical config.json and host-supplied project shape.
type ProjectConfig struct {
	Models      publicmodel.Config         `json:"models"`
	Runtime     RuntimeConfig              `json:"runtime"`
	Attachments AttachmentLimits           `json:"attachments,omitempty"`
	MCPServers  map[string]MCPServerConfig `json:"mcpServers,omitempty"`
}

// Config is the validated runtime form of ProjectConfig.
type Config struct {
	ProjectConfig
}

// DefaultProjectConfig returns the defaults used by config.json.
func DefaultProjectConfig() ProjectConfig {
	return ProjectConfig{
		Models: publicmodel.Config{
			Providers: map[string]publicmodel.ProviderConfig{},
			Profiles:  map[string]publicmodel.ModelProfile{},
		},
		Runtime:    RuntimeConfig{AutoCompact: true, AutoCompactThreshold: DefaultAutoCompactThreshold},
		MCPServers: map[string]MCPServerConfig{},
	}
}

func (c ProjectConfig) toConfig() Config {
	return Config{ProjectConfig: c}
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

// Resolved returns effective limits with default values applied.
func (l AttachmentLimits) Resolved() model.AttachmentLimits {
	maxFiles := l.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultAttachmentMaxFiles
	}
	maxFileBytes := l.MaxFileBytes
	if maxFileBytes <= 0 {
		maxFileBytes = DefaultAttachmentMaxFileBytes
	}
	maxTotalBytes := l.MaxTotalBytes
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultAttachmentMaxTotalBytes
	}
	return model.AttachmentLimits{
		MaxFiles:      maxFiles,
		MaxFileBytes:  maxFileBytes,
		MaxTotalBytes: maxTotalBytes,
		Explicit:      l.MaxFileBytes > 0 || l.MaxTotalBytes > 0,
	}
}

// ImageMaxBytes returns the maximum byte limit for a single attachment image.
func (l AttachmentLimits) ImageMaxBytes() int64 {
	resolved := l.Resolved()
	return min(resolved.MaxFileBytes, resolved.MaxTotalBytes)
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
	var topLevel map[string]json.RawMessage
	topDecoder := json.NewDecoder(bytes.NewReader(data))
	if err := topDecoder.Decode(&topLevel); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := topDecoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("decode %s: expected one JSON object", path)
	}
	if _, nested := topLevel["runtime"]; !nested {
		return Config{}, fmt.Errorf("decode %s: expected the grouped models/runtime config format; update it using config.example.json", path)
	}
	project := DefaultProjectConfig()
	project.Runtime.AutoCompactThreshold = 0
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&project); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("decode %s: expected one JSON object", path)
	}
	config := Config{ProjectConfig: project}
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

// Validate checks project settings, the model catalog, and MCP server configs.
func (c *Config) applyDefaults() {
	if c.Runtime.AutoCompact && c.Runtime.AutoCompactThreshold == 0 {
		c.Runtime.AutoCompactThreshold = DefaultAutoCompactThreshold
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
	if c.Runtime.MaxParallelAgents < 0 {
		return errors.New("runtime.maxParallelAgents must not be negative")
	}
	if c.Runtime.ContextWindow < 0 {
		return errors.New("runtime.contextWindow must not be negative")
	}
	if c.Runtime.MaxTeamChildrenPerAgent < 0 {
		return errors.New("runtime.maxTeamChildrenPerAgent must not be negative")
	}
	if c.Runtime.MaxTeamDepth < 0 {
		return errors.New("runtime.maxTeamDepth must not be negative")
	}
	if c.Runtime.ReserveOutputTokens < 0 {
		return errors.New("runtime.reserveOutputTokens must not be negative")
	}
	if c.Runtime.ContextWindow > 0 && c.Runtime.ReserveOutputTokens >= c.Runtime.ContextWindow {
		return errors.New("runtime.reserveOutputTokens must be smaller than runtime.contextWindow")
	}
	if math.IsNaN(c.Runtime.AutoCompactThreshold) || math.IsInf(c.Runtime.AutoCompactThreshold, 0) ||
		c.Runtime.AutoCompactThreshold < 0 || c.Runtime.AutoCompactThreshold > 1 {
		return errors.New("runtime.autoCompactThreshold must be between 0 and 1; 0 means unset")
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

// ModelConfig returns the internal model catalog consumed by model providers.
func (c Config) ModelConfig() model.Config {
	providers := make(map[string]model.ProviderConfig, len(c.Models.Providers))
	for name, provider := range c.Models.Providers {
		providers[name] = model.ProviderConfig{
			Protocol: provider.Protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey,
			ServerCompaction: provider.ServerCompaction,
		}
	}
	return model.Config{
		Providers:         providers,
		Models:            c.Models.Profiles,
		DefaultModel:      c.Models.DefaultProfile,
		MaxParallelAgents: c.Runtime.MaxParallelAgents,
	}
}
