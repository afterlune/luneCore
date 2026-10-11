package model

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	ProviderDeepSeek          = "deepseek"
	ProviderAnthropic         = "anthropic"
	ProtocolResponses         = "responses"
	ProtocolAnthropicMessages = "anthropic-messages"
	DeepSeekResponsesBaseURL  = "https://api.deepseek.com"
	DefaultMaxParallelAgents  = 4
)

// ProviderConfig configures a built-in protocol adapter. Credentials are kept
// in memory and are never included in model profiles or runtime events.
type ProviderConfig struct {
	Protocol         string `json:"protocol"`
	BaseURL          string `json:"baseURL"`
	APIKey           string `json:"apiKey"`
	ServerCompaction bool   `json:"serverCompaction,omitempty"`
}

// ModelProfile binds a stable profile name to a provider and model ID.
type ModelProfile struct {
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	MaxOutputTokens    int64  `json:"maxOutputTokens,omitempty"`
	SupportsInputAudio bool   `json:"supportsInputAudio,omitempty"`
}

// Config is the programmatic equivalent of the model section in config.json.
type Config struct {
	Providers         map[string]ProviderConfig `json:"providers"`
	Models            map[string]ModelProfile   `json:"models"`
	DefaultModel      string                    `json:"defaultModel"`
	MaxParallelAgents int                       `json:"maxParallelAgents,omitempty"`
}

// Validate checks the built-in provider configuration. A provider supplied
// through Core.Config.ModelProviders may omit APIKey, but still needs a valid
// protocol descriptor so the runtime can select compaction capabilities.
func (c Config) Validate() error {
	if len(c.Providers) == 0 || len(c.Models) == 0 {
		return errors.New("at least one provider and model profile must be configured")
	}
	if strings.TrimSpace(c.DefaultModel) == "" {
		return errors.New("defaultModel is required")
	}
	if _, ok := c.Models[c.DefaultModel]; !ok {
		return fmt.Errorf("defaultModel %q does not name a configured model profile", c.DefaultModel)
	}
	if c.MaxParallelAgents < 0 {
		return errors.New("maxParallelAgents must not be negative")
	}
	for name, provider := range c.Providers {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
			return errors.New("provider names must be non-empty and unpadded")
		}
		if provider.Protocol != ProtocolResponses && provider.Protocol != ProtocolAnthropicMessages {
			return fmt.Errorf("provider %q has unsupported protocol %q", name, provider.Protocol)
		}
		if provider.ServerCompaction && provider.Protocol != ProtocolResponses {
			return fmt.Errorf("provider %q enables serverCompaction but does not use the Responses protocol", name)
		}
		if err := validateBaseURL(provider.BaseURL); err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
	}
	for name, profile := range c.Models {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
			return errors.New("model profile names must be non-empty and unpadded")
		}
		if strings.TrimSpace(profile.Model) == "" {
			return fmt.Errorf("model profile %q requires a model ID", name)
		}
		provider, ok := c.Providers[profile.Provider]
		if !ok {
			return fmt.Errorf("model profile %q references unconfigured provider %q", name, profile.Provider)
		}
		if profile.MaxOutputTokens < 0 {
			return fmt.Errorf("model profile %q maxOutputTokens must not be negative", name)
		}
		if profile.SupportsInputAudio && provider.Protocol != ProtocolResponses {
			return fmt.Errorf("model profile %q enables supportsInputAudio but does not use the Responses protocol", name)
		}
		if provider.Protocol == ProtocolAnthropicMessages && profile.MaxOutputTokens == 0 {
			return fmt.Errorf("model profile %q using %s requires maxOutputTokens", name, ProtocolAnthropicMessages)
		}
	}
	return nil
}

func validateBaseURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("baseURL must be an http or https URL without query or fragment")
	}
	if strings.TrimSpace(value) != value {
		return errors.New("baseURL must not contain surrounding whitespace")
	}
	return nil
}

func (c Config) ParallelLimit() int {
	if c.MaxParallelAgents == 0 {
		return DefaultMaxParallelAgents
	}
	return c.MaxParallelAgents
}
