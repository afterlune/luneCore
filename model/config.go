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
	DisplayName        string `json:"displayName,omitempty"`
	ContextWindow      int64  `json:"contextWindow,omitempty"`
	MaxOutputTokens    int64  `json:"maxOutputTokens,omitempty"`
	SupportsInputAudio bool   `json:"supportsInputAudio,omitempty"`
}

// EffectiveDisplayName returns the friendly display name, falling back to Model.
func (p ModelProfile) EffectiveDisplayName() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Model
}

// Config is the model catalog nested under ProjectConfig.models.
type Config struct {
	Providers      map[string]ProviderConfig `json:"providers"`
	Profiles       map[string]ModelProfile   `json:"profiles"`
	DefaultProfile string                    `json:"defaultProfile"`
}

// Validate checks the built-in provider configuration. A provider supplied
// through Core.Config.ModelProviders may omit APIKey, but still needs a valid
// protocol descriptor so the runtime can select compaction capabilities.
func (c Config) Validate() error {
	if len(c.Providers) == 0 || len(c.Profiles) == 0 {
		return errors.New("at least one provider and model profile must be configured")
	}
	if strings.TrimSpace(c.DefaultProfile) == "" {
		return errors.New("defaultProfile is required")
	}
	if _, ok := c.Profiles[c.DefaultProfile]; !ok {
		return fmt.Errorf("defaultProfile %q does not name a configured model profile", c.DefaultProfile)
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
	for name, profile := range c.Profiles {
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
		if profile.ContextWindow < 0 {
			return fmt.Errorf("model profile %q contextWindow must not be negative", name)
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
