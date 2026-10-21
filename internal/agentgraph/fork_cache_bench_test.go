package agentgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	runtimeTool "github.com/afterlune/luneCore/internal/tool"
)

func BenchmarkTeamForkInputPreparation(b *testing.B) {
	for _, historySize := range []int{100, 1000} {
		prefix := make([]model.InputItem, historySize)
		for index := range prefix {
			prefix[index] = model.InputMessage{
				Role:    model.RoleUser,
				Content: model.NewMessageText(fmt.Sprintf("history item %d %s", index, strings.Repeat("context ", 16))),
			}
		}
		for _, children := range []int{2, 4} {
			for _, variant := range []struct {
				name string
				make func([]model.InputItem, string) (*model.ResponseInput, int, error)
			}{
				{name: "legacy", make: benchmarkLegacyTeamAssignmentInput},
				{name: "current", make: func(prefix []model.InputItem, task string) (*model.ResponseInput, int, error) {
					return teamAssignmentInput(TeamDispatchFork, prefix, task, false)
				}},
			} {
				b.Run(fmt.Sprintf("items=%d/children=%d/%s", historySize, children, variant.name), func(b *testing.B) {
					b.ReportMetric(float64(historySize), "prefix-items")
					b.ReportMetric(float64(children), "children")
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						for child := range children {
							if _, _, err := variant.make(prefix, fmt.Sprintf("task %d", child)); err != nil {
								b.Fatal(err)
							}
						}
					}
				})
			}
		}
	}
}

func BenchmarkTeamForkCacheHint(b *testing.B) {
	registry, err := runtimeTool.NewDefaultRegistry(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer registry.Close()
	tools := append(registry.Definitions(), askUserDefinition(), teamDispatchDefinition(benchmarkTeamExecutor{}))
	state := State{
		TeamForked:          true,
		TeamForkPrefixItems: 100,
		TeamRunID:           "benchmark-dispatch",
		Provider:            model.ProviderOpenAI,
		Protocol:            model.ProtocolResponses,
		Model:               "benchmark-model",
		TeamForkSourceCache: teamCacheIdentity{
			Provider: model.ProviderOpenAI, Protocol: model.ProtocolResponses,
			Model: "benchmark-model", Key: "lune-benchmark-cache",
		},
	}
	request := model.ModelRequest{
		Model: "benchmark-model", Instructions: "representative agent instructions", Tools: tools,
		Input: model.NewResponseInputItems(model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("prefix")}),
	}
	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_ = benchmarkLegacyTeamForkCacheHint(state)
		}
	})
	b.Run("current", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_ = teamForkCacheHint(state, request)
		}
	})
}

func benchmarkLegacyTeamAssignmentInput(prefix []model.InputItem, task string) (*model.ResponseInput, int, error) {
	items := append([]model.InputItem(nil), prefix...)
	items = append(items, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(task)})
	return model.NewResponseInputItems(items...), len(prefix), nil
}

func benchmarkLegacyTeamForkCacheHint(state State) *model.PromptCacheHint {
	if !state.TeamForked || state.TeamForkPrefixItems <= 0 || state.TeamRunID == "" {
		return nil
	}
	identity := strings.Join([]string{state.TeamRunID, state.Provider, state.Model}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return &model.PromptCacheHint{Key: "lune-" + hex.EncodeToString(digest[:16]), PrefixItems: state.TeamForkPrefixItems}
}
