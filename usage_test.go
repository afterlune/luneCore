package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
)

func TestAgentUsageTotalsOutliveRecentRequestLimit(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	usage := Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, InputTokensDetails: TokenInputDetails{CachedTokens: 4}}
	for request := 0; request < 205; request++ {
		agentID := "lead"
		if request%2 == 1 {
			agentID = "research"
		}
		if err := client.recordUsageForAgent(context.Background(), saved.ID, usage, agentID, agentID, "provider", agentID+"-model", 64000, 0); err != nil {
			t.Fatalf("record usage request %d: %v", request+1, err)
		}
	}

	restored, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(restored.UsageRequests) != 200 || restored.SessionUsage.Requests != 205 {
		t.Fatalf("recent requests=%d session requests=%d, want 200 and 205", len(restored.UsageRequests), restored.SessionUsage.Requests)
	}
	if !restored.AgentUsageComplete || restored.AgentUsage["lead"].Requests != 103 || restored.AgentUsage["research"].Requests != 102 {
		t.Fatalf("agent usage totals = %#v complete=%v", restored.AgentUsage, restored.AgentUsageComplete)
	}
	for agentID, wantRequests := range map[string]int{"lead": 103, "research": 102} {
		totals := restored.AgentUsage[agentID]
		if totals.Measured != wantRequests || totals.Input != int64(wantRequests*10) || totals.Output != int64(wantRequests*2) || totals.Cached != int64(wantRequests*4) {
			t.Errorf("%s totals = %#v, want %d measured requests", agentID, totals, wantRequests)
		}
	}
}

func TestForkSessionStartsFreshAgentUsageTotals(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	parent, err := client.CreateSession(context.Background(), json.RawMessage(`[{"type":"message","role":"user","content":"prior conversation"}]`))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := client.recordUsageForAgent(context.Background(), parent.ID, Usage{InputTokens: 12, TotalTokens: 12}, "lead", "primary", model.ProviderDeepSeek, "deepseek-main", 64000, 0); err != nil {
		t.Fatalf("record parent usage: %v", err)
	}

	branch, err := client.ForkSession(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if !branch.AgentUsageComplete || len(branch.AgentUsage) != 0 || branch.SessionUsage.Requests != 0 {
		t.Fatalf("fork usage = agent %#v complete=%v session %#v", branch.AgentUsage, branch.AgentUsageComplete, branch.SessionUsage)
	}
	if branch.History == nil || string(branch.History) == "null" {
		t.Fatalf("fork history was not preserved: %s", branch.History)
	}
}

func TestUsageRequestUsesEstimateOnlyWhenInputUsageIsMissing(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeSettingsConfig(t, workspace, server.URL)
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := client.recordUsageForAgent(context.Background(), saved.ID,
		Usage{OutputTokens: 4, TotalTokens: 4}, "lead", "primary", "provider-a", "model-a", 64000, 123); err != nil {
		t.Fatalf("record missing-input usage: %v", err)
	}
	if err := client.recordUsageForAgent(context.Background(), saved.ID,
		Usage{InputTokens: 100, OutputTokens: 4, TotalTokens: 104}, "lead", "primary", "provider-a", "model-a", 64000, 999); err != nil {
		t.Fatalf("record measured-input usage: %v", err)
	}
	restored, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(restored.UsageRequests) != 2 || restored.UsageRequests[0].EstimatedInputTokens != 123 ||
		restored.UsageRequests[1].EstimatedInputTokens != 0 || restored.UsageRequests[1].Usage.InputTokens != 100 {
		t.Fatalf("usage requests = %#v; server input should win and estimates stay separate", restored.UsageRequests)
	}
	if restored.SessionUsage.Input != 100 || restored.SessionUsage.EstimatedInput != 123 || restored.SessionUsage.Measured != 2 {
		t.Fatalf("session measured totals = %#v; estimates must not enter totals", restored.SessionUsage)
	}
}
