package agentgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/afterlune/luneCore/internal/model"
)

func requestFingerprint(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}
func traceRequest(state State, request model.ModelRequest, id, mode string) model.RequestTrace {
	var items []model.InputItem
	if request.Input != nil {
		items = request.Input.Items
	}
	trace := model.RequestTrace{ID: id, Instructions: requestFingerprint(request.Instructions), Tools: requestFingerprint(request.Tools), History: requestFingerprint(request.Input), Items: len(items), Context: requestFingerprint(state.HostContext), Provider: state.Provider, Mode: mode, Model: request.Model, Protocol: state.Protocol}
	previous := state.LastRequest
	if previous.ID == "" {
		trace.Changes = append(trace.Changes, "first_request")
		return trace
	}
	if trace.Instructions != previous.Instructions {
		trace.Changes = append(trace.Changes, "instructions_changed")
	}
	if trace.Tools != previous.Tools {
		trace.Changes = append(trace.Changes, "tools_changed")
	}
	if trace.Mode != previous.Mode {
		trace.Changes = append(trace.Changes, "permissions_changed")
	}
	if trace.Provider != previous.Provider || trace.Model != previous.Model || trace.Protocol != previous.Protocol {
		trace.Changes = append(trace.Changes, "model_changed")
	}
	if trace.Context != previous.Context {
		trace.Changes = append(trace.Changes, "context_updated")
	}
	if previous.Items > trace.Items || requestFingerprint(model.NewResponseInputItems(items[:min(previous.Items, trace.Items)]...)) != previous.History {
		trace.Changes = append(trace.Changes, "history_rebuilt")
	}
	return trace
}
