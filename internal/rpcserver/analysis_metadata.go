package rpcserver

import (
	"strings"

	core "github.com/afterlune/luneCore"
)

// resolveAnalysisMetadata restores the durable link for new sessions and
// validates legacy fields supplied by older clients. Sessions created before
// analysis metadata was persisted remain readable through the legacy shape.
func resolveAnalysisMetadata(saved core.Session, kind string, requested core.AnalysisMetadata) (core.AnalysisMetadata, *rpcError) {
	if saved.Analysis == nil {
		if strings.TrimSpace(requested.SourceSessionID) == "" || len(requested.EvidenceDigest) != 64 {
			return core.AnalysisMetadata{}, &rpcError{Code: -32602, Message: "session has no saved analysis link; sourceSessionId and evidence digest are required"}
		}
		requested.Kind = kind
		return requested, nil
	}
	metadata := *saved.Analysis
	if metadata.Kind != kind {
		return core.AnalysisMetadata{}, &rpcError{Code: -32602, Message: "session analysis kind does not match request"}
	}
	fields := []struct {
		name      string
		requested string
		stored    string
	}{
		{"sourceSessionId", requested.SourceSessionID, metadata.SourceSessionID},
		{"sourceExecutionId", requested.SourceExecutionID, metadata.SourceExecutionID},
		{"executionId", requested.ExecutionID, metadata.ExecutionID},
		{"evidence digest", requested.EvidenceDigest, metadata.EvidenceDigest},
		{"path", requested.Path, metadata.Path},
	}
	for _, field := range fields {
		if field.requested != "" && field.requested != field.stored {
			return core.AnalysisMetadata{}, &rpcError{Code: -32602, Message: field.name + " does not match saved analysis metadata"}
		}
	}
	if metadata.SourceSessionID == "" || metadata.ExecutionID == "" || len(metadata.EvidenceDigest) != 64 {
		return core.AnalysisMetadata{}, &rpcError{Code: -32603, Message: "saved analysis metadata is incomplete"}
	}
	return metadata, nil
}
