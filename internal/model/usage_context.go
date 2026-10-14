package model

import "context"

type UsagePurpose string

const (
	UsagePurposeMain       UsagePurpose = "main"
	UsagePurposeCriteria   UsagePurpose = "criteria"
	UsagePurposeAcceptance UsagePurpose = "acceptance"
	UsagePurposeCompaction UsagePurpose = "compaction"
	UsagePurposeMemory     UsagePurpose = "memory"
	UsagePurposeTeam       UsagePurpose = "team"
)

type usagePurposeContextKey struct{}

func WithUsagePurpose(ctx context.Context, purpose UsagePurpose) context.Context {
	return context.WithValue(ctx, usagePurposeContextKey{}, purpose)
}

func UsagePurposeFromContext(ctx context.Context) UsagePurpose {
	if ctx == nil {
		return ""
	}
	purpose, _ := ctx.Value(usagePurposeContextKey{}).(UsagePurpose)
	return purpose
}
