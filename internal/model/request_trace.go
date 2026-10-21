package model

import "context"

// RequestTrace contains only local request identity and content fingerprints.
// Changes describe local mutations, not provider cache eviction reasons.
type RequestTrace struct {
	ID           string   `json:"id,omitempty"`
	Attempt      int      `json:"attempt,omitempty"`
	Changes      []string `json:"changes,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Tools        string   `json:"tools,omitempty"`
	History      string   `json:"history,omitempty"`
	Items        int      `json:"items,omitempty"`
	Context      string   `json:"context,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	Mode         string   `json:"mode,omitempty"`
	Model        string   `json:"model,omitempty"`
	Protocol     string   `json:"protocol,omitempty"`
}
type requestTraceKey struct{}

func WithRequestTrace(ctx context.Context, trace RequestTrace) context.Context {
	return context.WithValue(ctx, requestTraceKey{}, trace)
}
func RequestTraceFromContext(ctx context.Context) RequestTrace {
	value, _ := ctx.Value(requestTraceKey{}).(RequestTrace)
	return value
}
