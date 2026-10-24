package runtime

import (
	"encoding/json"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

// SessionSettings contains per-session overrides. Nil context settings inherit
// the project defaults; a running task keeps its resolved values in checkpoint.
type SessionSettings struct {
	ModelProfile         string   `json:"model_profile"`
	ReasoningEffort      string   `json:"reasoning_effort,omitempty"`
	ContextWindow        *int64   `json:"context_window,omitempty"`
	ReserveOutputTokens  *int64   `json:"reserve_output_tokens,omitempty"`
	AutoCompact          *bool    `json:"auto_compact,omitempty"`
	AutoCompactThreshold *float64 `json:"auto_compact_threshold,omitempty"`
}

type taskSettings struct {
	Goal                 string
	ContextWindow        int64
	ReserveOutputTokens  int64
	AutoCompact          bool
	AutoCompactThreshold float64
}

// ContextDefaults exposes project-level context values to frontends so they
// can distinguish inherited defaults from session overrides.
type ContextDefaults struct {
	ContextWindow        int64
	ReserveOutputTokens  int64
	AutoCompact          bool
	AutoCompactThreshold float64
}

// AttachmentLimits exposes the effective project limits to frontends before
// they read or encode image data.
type AttachmentLimits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// MCPServerStatus describes one configured MCP server without exposing its
// credentials or child-process environment.
type MCPServerStatus struct {
	Name      string
	Transport string
	Endpoint  string
	Command   string
	Enabled   bool
	Connected bool
	ToolCount int
	LastError string
}

// ModelOption is a model profile exposed to clients without leaking the
// internal model package through Core's public API.
type ModelOption struct {
	Profile       string `json:"profile"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	DisplayName   string `json:"displayName,omitempty"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
}

// Session contains durable conversation state, not TUI transcript caches.
type Session struct {
	ID          string          `json:"id"`
	Workspace   string          `json:"workspace"`
	UpdatedAt   time.Time       `json:"updated_at"`
	DataVersion int             `json:"data_version"`
	Title       string          `json:"title"`
	Archived    bool            `json:"archived"`
	Settings    SessionSettings `json:"settings"`
	// PathGrants are explicit, session-scoped grants for built-in filesystem tools.
	PathGrants []publictool.PathGrant `json:"path_grants,omitempty"`
	// History is the complete provider-neutral conversation shown to clients.
	History json.RawMessage `json:"history,omitempty"`
	// ModelContext is a cached provider-specific request context when it differs
	// from History. It can contain opaque Responses state and is never rendered.
	ModelContext json.RawMessage `json:"model_context,omitempty"`
	// AgentContext stores opaque host-owned discovery and prompt context state.
	// It is not user-authored history and must be preserved when copying snapshots.
	AgentContext       json.RawMessage        `json:"agent_context,omitempty"`
	ModelContextKey    string                 `json:"model_context_key,omitempty"`
	UsageRequests      []UsageRecord          `json:"usage_requests,omitempty"`
	AgentUsage         map[string]UsageTotals `json:"agent_usage,omitempty"`
	AgentUsageComplete bool                   `json:"agent_usage_complete"`
	TaskUsage          UsageTotals            `json:"task_usage,omitempty"`
	SessionUsage       UsageTotals            `json:"session_usage,omitempty"`
}
