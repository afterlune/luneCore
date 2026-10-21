package agentgraph

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	graph "github.com/afterlune/luneGraph"
)

var benchmarkToolSchema []byte

func BenchmarkAgentConstruction(b *testing.B) {
	requester := benchmarkRequester{}
	tools, err := tool.NewRegistry()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := NewWithOptions(requester, tools, AgentOptions{ID: "lead", Model: "bench"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRuntimeToolDefinitionBudget(b *testing.B) {
	registry, err := tool.NewDefaultRegistry(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer registry.Close()
	team := benchmarkTeamExecutor{}
	bench := func(name string, includeTeam bool) {
		b.Run(name, func(b *testing.B) {
			definitions := registry.Definitions()
			definitions = append(definitions, askUserDefinition())
			if includeTeam {
				definitions = append(definitions, teamDispatchDefinition(team))
			}
			request := model.ModelRequest{Tools: definitions}
			encoded, err := json.Marshal(definitions)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				current := registry.Definitions()
				current = append(current, askUserDefinition())
				if includeTeam {
					current = append(current, teamDispatchDefinition(team))
				}
				_ = current
			}
			b.StopTimer()
			b.ReportMetric(float64(model.EstimateInputTokens(request)), "tokens/request")
			b.ReportMetric(float64(len(encoded)), "schema-bytes")
		})
	}
	bench("clarification", false)
	bench("team", true)
}

func BenchmarkRuntimeToolExposureBudget(b *testing.B) {
	registry, err := tool.NewDefaultRegistry(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer registry.Close()
	agent, err := NewWithOptions(benchmarkRequester{}, registry, AgentOptions{
		ToolVisibleForMode: registry.ToolVisibleForMode,
		ToolReadOnly: func(name string) bool {
			metadata, ok := registry.ToolMetadata(name)
			return ok && metadata.ReadOnly
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	legacySelector, err := model.CategorySelectorDefinition(registry.Categories())
	if err != nil {
		b.Fatal(err)
	}
	legacyFiles := append([]model.ResponseTool{legacySelector}, registry.DefinitionsForCategory("files")...)
	legacyWeb := append([]model.ResponseTool{legacySelector}, registry.DefinitionsForCategory("web")...)
	for _, benchmark := range []struct {
		name        string
		state       State
		definitions []model.ResponseTool
		current     bool
	}{
		{name: "previous_selector_only", definitions: []model.ResponseTool{legacySelector}},
		{name: "previous_files_selected", definitions: legacyFiles},
		{name: "common_initial", state: State{AgentID: "lead"}, current: true},
		{name: "previous_web_selected", definitions: legacyWeb},
		{name: "common_plus_web", state: State{AgentID: "lead", SelectedCategories: []string{"web"}}, current: true},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			definitions := benchmark.definitions
			if benchmark.current {
				definitions, _, err = agent.visibleTools(benchmark.state, string(tool.PermissionModeWorkspace))
				if err != nil {
					b.Fatal(err)
				}
			}
			request := model.ModelRequest{Tools: definitions}
			encoded, err := json.Marshal(definitions)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				current := definitions
				if benchmark.current {
					current, _, err = agent.visibleTools(benchmark.state, string(tool.PermissionModeWorkspace))
					if err != nil {
						b.Fatal(err)
					}
				}
				benchmarkToolSchema, err = json.Marshal(current)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(model.EstimateInputTokens(request)), "tokens/request")
			b.ReportMetric(float64(len(encoded)), "schema-bytes")
		})
	}
}

func BenchmarkStateClone(b *testing.B) {
	state := benchmarkState()
	encoded, err := json.Marshal(state)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("json", func(b *testing.B) {
		clone := graph.JSONClone[State]
		b.ReportMetric(float64(len(encoded)), "state-bytes")
		b.ReportAllocs()
		for range b.N {
			if _, err := clone(state); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("manual", func(b *testing.B) {
		b.ReportMetric(float64(len(encoded)), "state-bytes")
		b.ReportAllocs()
		for range b.N {
			if _, err := cloneState(state); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func benchmarkState() State {
	var history []model.InputItem
	for range 100 {
		text := "A representative conversation turn with tool output and workspace context. " + strings.Repeat("result text ", 24)
		history = append(history, model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(text)})
	}
	return State{
		RunID:        "benchmark-run",
		History:      *model.NewResponseInputItems(history...),
		Transcript:   *model.NewResponseInputItems(history...),
		PendingCalls: []model.OutputFunctionCall{{Name: "read_file", Arguments: `{"path":"main.go"}`}},
	}
}

type benchmarkRequester struct{}

func (benchmarkRequester) CreateResponse(context.Context, model.ModelRequest) (*model.ModelResponse, error) {
	return &model.ModelResponse{}, nil
}

type benchmarkTeamExecutor struct{}

func (benchmarkTeamExecutor) Profiles() map[string]model.ModelProfile {
	return map[string]model.ModelProfile{
		"fast":  {Provider: model.ProviderDeepSeek, Model: "deepseek-flash"},
		"smart": {Provider: model.ProviderAnthropic, Model: "claude-sonnet"},
	}
}

func (benchmarkTeamExecutor) MaxParallelAgents() int { return 4 }

func (benchmarkTeamExecutor) PrepareBranch(_ context.Context, state State, _ TeamAssignment, _ TaskOptions) (State, error) {
	return state, nil
}

func (benchmarkTeamExecutor) Requester(context.Context, State) (Requester, error) {
	return benchmarkRequester{}, nil
}
