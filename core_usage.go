package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/afterlune/luneCore/internal/model"
)

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

func (c *Core) recordUsageForAgent(ctx context.Context, id string, usage Usage, agentID, profile, provider, modelID string, contextWindow, estimatedInputTokens int64) error {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	saved, err := c.GetSession(ctx, id)
	if err != nil {
		return err
	}
	available := usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.OutputTokensDetails.ReasoningTokens != 0 || usage.TotalTokens != 0 ||
		usage.InputTokensDetails.CachedTokens != 0 || usage.InputTokensDetails.CacheCreationTokens != 0
	record := UsageRecord{
		Request: saved.SessionUsage.Requests + 1, Usage: usage, Available: available,
		ContextWindow: contextWindow, EstimatedInputTokens: estimatedInputTokensIfMissing(usage.InputTokens, estimatedInputTokens),
		AgentID: agentID, ModelProfile: profile, Provider: provider, Model: modelID,
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
