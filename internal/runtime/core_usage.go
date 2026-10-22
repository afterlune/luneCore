package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/afterlune/luneCore/internal/model"
)

type Usage struct {
	InputTokens         int64              `json:"input_tokens"`
	InputTokensDetails  TokenInputDetails  `json:"input_tokens_details"`
	OutputTokens        int64              `json:"output_tokens"`
	OutputTokensDetails TokenOutputDetails `json:"output_tokens_details"`
	TotalTokens         int64              `json:"total_tokens"`
}

type TokenInputDetails struct {
	CachedTokens        int64 `json:"cached_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
}

type TokenOutputDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type UsagePurpose = model.UsagePurpose

const (
	UsagePurposeMain       = model.UsagePurposeMain
	UsagePurposeCompaction = model.UsagePurposeCompaction
	UsagePurposeMemory     = model.UsagePurposeMemory
	UsagePurposeTeam       = model.UsagePurposeTeam
)

type UsageRecord struct {
	RequestID            string       `json:"request_id,omitempty"`
	RequestAttempt       int          `json:"request_attempt,omitempty"`
	RequestChanges       []string     `json:"request_changes,omitempty"`
	Request              int          `json:"request"`
	Usage                Usage        `json:"usage"`
	EstimatedInputTokens int64        `json:"estimated_input_tokens,omitempty"`
	Available            bool         `json:"available"`
	ContextWindow        int64        `json:"context_window,omitempty"`
	AgentID              string       `json:"agent_id,omitempty"`
	Purpose              UsagePurpose `json:"purpose,omitempty"`
	Provider             string       `json:"provider,omitempty"`
	ModelProfile         string       `json:"model_profile,omitempty"`
	Model                string       `json:"model,omitempty"`
}

type UsageTotals struct {
	Requests       int   `json:"requests"`
	Measured       int   `json:"measured"`
	Input          int64 `json:"input"`
	EstimatedInput int64 `json:"estimated_input,omitempty"`
	Output         int64 `json:"output"`
	Reasoning      int64 `json:"reasoning"`
	Total          int64 `json:"total"`
	Cached         int64 `json:"cached"`
	CacheCreation  int64 `json:"cache_creation,omitempty"`
}

func modelContextKey(providerName, effectiveModel string, providerConfig model.ProviderConfig) string {
	identity, _ := json.Marshal(struct {
		Provider         string `json:"provider"`
		Protocol         string `json:"protocol"`
		BaseURL          string `json:"base_url"`
		Model            string `json:"model"`
		ServerCompaction bool   `json:"server_compaction"`
	}{
		Provider: providerName, Protocol: providerConfig.Protocol, BaseURL: providerConfig.BaseURL,
		Model: effectiveModel, ServerCompaction: providerConfig.ServerCompaction,
	})
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:])
}

func (c *Core) recordUsage(ctx context.Context, id string, usage Usage) error {
	return c.recordUsageForAgent(ctx, id, usage, "", "", "", "", 0, 0)
}

func (c *Core) recordUsageForAgent(ctx context.Context, id string, usage Usage, agentID, profile, provider, modelID string, contextWindow, estimatedInputTokens int64, purposes ...UsagePurpose) error {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	if usage.TotalTokens <= 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	saved, err := c.GetSession(ctx, id)
	if err != nil {
		return err
	}
	available := usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.OutputTokensDetails.ReasoningTokens != 0 || usage.TotalTokens != 0 ||
		usage.InputTokensDetails.CachedTokens != 0 || usage.InputTokensDetails.CacheCreationTokens != 0
	var purpose UsagePurpose
	if len(purposes) > 0 {
		purpose = purposes[0]
	}
	trace := model.RequestTraceFromContext(ctx)
	record := UsageRecord{
		RequestID: trace.ID, RequestAttempt: trace.Attempt, RequestChanges: trace.Changes,
		Request: saved.SessionUsage.Requests + 1, Usage: usage, Available: available,
		ContextWindow: contextWindow, EstimatedInputTokens: estimatedInputTokensIfMissing(usage.InputTokens, estimatedInputTokens),
		AgentID: agentID, Purpose: purpose, ModelProfile: profile, Provider: provider, Model: modelID,
	}
	saved.UsageRequests = append(saved.UsageRequests, record)
	if len(saved.UsageRequests) > 200 {
		saved.UsageRequests = append([]UsageRecord(nil), saved.UsageRequests[len(saved.UsageRequests)-200:]...)
	}
	if saved.AgentUsage == nil {
		saved.AgentUsage = make(map[string]UsageTotals)
	}
	agentTotals := saved.AgentUsage[agentID]
	addUsage(&agentTotals, usage, available)
	estimatedInput := estimatedInputTokensIfMissing(usage.InputTokens, estimatedInputTokens)
	agentTotals.EstimatedInput += estimatedInput
	saved.AgentUsage[agentID] = agentTotals
	addUsage(&saved.SessionUsage, usage, available)
	addUsage(&saved.TaskUsage, usage, available)
	saved.SessionUsage.EstimatedInput += estimatedInput
	saved.TaskUsage.EstimatedInput += estimatedInput
	saved.UpdatedAt = time.Now()
	return c.saveSession(ctx, saved)
}

func estimatedInputTokensIfMissing(inputTokens, estimatedInputTokens int64) int64 {
	if inputTokens > 0 || estimatedInputTokens <= 0 {
		return 0
	}
	return estimatedInputTokens
}

func addUsage(totals *UsageTotals, usage Usage, available bool) {
	totals.Requests++
	if !available {
		return
	}
	totals.Measured++
	totals.Input += usage.InputTokens
	totals.Output += usage.OutputTokens
	totals.Reasoning += usage.OutputTokensDetails.ReasoningTokens
	totals.Total += usage.TotalTokens
	totals.Cached += usage.InputTokensDetails.CachedTokens
	totals.CacheCreation += usage.InputTokensDetails.CacheCreationTokens
}

// latestLeadInputTokens returns the provider-measured input size of the latest
// lead/main request so a new task's compaction trigger cannot under-estimate a
// session that is already close to its window.
func latestLeadInputTokens(records []UsageRecord, provider, profile string) int64 {
	for index := len(records) - 1; index >= 0; index-- {
		record := records[index]
		if record.Usage.InputTokens <= 0 {
			continue
		}
		if record.AgentID != "" && record.AgentID != "lead" {
			continue
		}
		if record.Purpose != "" && record.Purpose != UsagePurposeMain {
			continue
		}
		// A provider or model switch rebuilds the context from the transcript;
		// the previous provider's measured size no longer describes this request.
		if record.Provider != "" && provider != "" && record.Provider != provider {
			continue
		}
		if record.ModelProfile != "" && profile != "" && record.ModelProfile != profile {
			continue
		}
		return record.Usage.InputTokens
	}
	return 0
}
