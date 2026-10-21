package agentgraph

import (
	"testing"

	"github.com/afterlune/luneCore/internal/model"
)

func TestRequestPromptCacheHintUsesModelScopedParentLineage(t *testing.T) {
	request := model.ModelRequest{
		Model:        "shared-model",
		Instructions: "shared instructions",
		Tools:        []model.ResponseTool{model.FunctionTool{Name: "read_file", Description: "Read a file."}},
		Input: model.NewResponseInputItems(model.InputMessage{
			Role: model.RoleUser, Content: model.NewMessageText("parent context"),
		}),
	}
	parent := State{
		RunID: "run", ExecutionID: "execution", AgentID: "lead",
		Provider: model.ProviderOpenAI, Protocol: model.ProtocolResponses,
	}
	parentHint := requestPromptCacheHint(parent, request, []string{teamDispatchToolName}, true)
	if parentHint == nil || parentHint.PrefixItems != 1 {
		t.Fatalf("parent cache hint = %#v", parentHint)
	}
	nextTurn := parent
	nextTurn.ExecutionID = "another-execution"
	if hint := requestPromptCacheHint(nextTurn, request, nil, true); hint == nil || hint.Key != parentHint.Key {
		t.Fatal("task boundary changed cache key")
	}
	if hint := requestPromptCacheHint(parent, request, nil, true); hint == nil {
		t.Fatal("ordinary parent request is missing cache hint")
	}
	if hint := requestPromptCacheHint(parent, model.ModelRequest{
		Model: "shared-model", Input: request.Input,
		ContextManagement: []model.ContextManagementItem{{Type: "compaction", CompactThreshold: 1000}},
	}, []string{teamDispatchToolName}, true); hint != nil {
		t.Fatalf("server compaction request received cache hint %#v", hint)
	}

	childState := State{
		RunID: "run", ExecutionID: "execution", AgentID: "lead/worker",
		Provider: model.ProviderOpenAI, Protocol: model.ProtocolResponses,
		TeamRunID: "dispatch", TeamForked: true, TeamForkPrefixItems: 1,
		TeamForkSourceCache: teamCacheIdentity{
			Provider: model.ProviderOpenAI, Protocol: model.ProtocolResponses, Model: "shared-model", Key: parentHint.Key,
		},
	}
	childRequest := request
	childRequest.Input = model.NewResponseInputItems(
		request.Input.Items[0],
		model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText("worker assignment")},
	)
	if hint := requestPromptCacheHint(childState, childRequest, nil, true); hint == nil || hint.Key != parentHint.Key || hint.PrefixItems != 1 {
		t.Fatalf("matching fork cache hint = %#v, want parent key %q", hint, parentHint.Key)
	}

	changedTools := childRequest
	changedTools.Tools = []model.ResponseTool{model.FunctionTool{Name: "read_file", Description: "Changed schema."}}
	if hint := requestPromptCacheHint(childState, changedTools, nil, true); hint == nil || hint.Key != parentHint.Key {
		t.Fatalf("same-model cache hint should retain parent lineage; provider validates the rendered prefix: %#v", hint)
	}

	differentModel := childState
	source := differentModel.TeamForkSourceCache
	source.Model = "other-model"
	differentModel.TeamForkSourceCache = source
	if hint := requestPromptCacheHint(differentModel, childRequest, nil, true); hint == nil || hint.Key == parentHint.Key {
		t.Fatalf("different model reused parent cache: %#v", hint)
	}
}
