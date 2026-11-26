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
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Endpoint  string `json:"endpoint,omitempty"`
	Command   string `json:"command,omitempty"`
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	ToolCount int    `json:"toolCount"`
	LastError string `json:"lastError,omitempty"`
}

// ModelOption is a model profile exposed to clients without leaking the
// internal model package through Core's public API.
type ModelOption struct {
	Profile             string `json:"profile"`
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	DisplayName         string `json:"displayName,omitempty"`
	ContextWindow       int64  `json:"contextWindow,omitempty"`
	SupportsInputImages bool   `json:"supportsInputImages,omitempty"`
}

type SessionApproval = publictool.SessionApproval

// Session contains durable conversation state, not TUI transcript caches.
type Session struct {
	ID          string          `json:"id"`
	Workspace   string          `json:"workspace"`
	UpdatedAt   time.Time       `json:"updated_at"`
	DataVersion int             `json:"data_version"`
	Title       string          `json:"title"`
	Archived    bool            `json:"archived"`
	Settings    SessionSettings `json:"settings"`
	// Analysis links an independent review or diagnosis session to its source.
	// It is committed together with the analysis turn entering the durable queue.
	Analysis *AnalysisMetadata `json:"analysis,omitempty"`
	// PathGrants are explicit, session-scoped grants for built-in filesystem tools.
	PathGrants []publictool.PathGrant `json:"path_grants,omitempty"`
	// Approvals are explicit, session-scoped approvals for tool executions.
	Approvals []publictool.SessionApproval `json:"approvals,omitempty"`
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

// SessionIndexEntry is the lightweight metadata used to enumerate a workspace's
// sessions without loading conversation histories.
type SessionIndexEntry struct {
	ID        string    `json:"id"`
	UpdatedAt time.Time `json:"updated_at"`
	Title     string    `json:"title"`
	Archived  bool      `json:"archived"`
}

// AnalysisMetadata identifies the source evidence and original execution for
// an independent analysis session. It is durable session data so clients can
// restore the relationship from session.get without browser-local state.
type AnalysisMetadata struct {
	Kind              string `json:"kind"`
	SourceSessionID   string `json:"sourceSessionId"`
	SourceExecutionID string `json:"sourceExecutionId,omitempty"`
	ExecutionID       string `json:"executionId"`
	EvidenceDigest    string `json:"evidenceDigest"`
	Path              string `json:"path,omitempty"`
}
