package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/workspacememory"
	"github.com/afterlune/luneCore/memory"
	publicmodel "github.com/afterlune/luneCore/model"
)

const maxMemorySummaryTokens = 800

func stateSessionID(state agentgraph.State) string {
	return strings.TrimPrefix(state.RunID, "lune-session-")
}

func (c *Core) memorySummary(ctx context.Context, state agentgraph.State) (string, error) {
	if c.memoryConfig.Disabled || c.memory == nil {
		return "", nil
	}
	return c.memory.Summary(ctx, state.TaskInput)
}

func (c *Core) memorySummaryForTask(ctx context.Context, state agentgraph.State) (string, error) {
	if state.TeamBranch {
		return "", nil
	}
	return c.memorySummary(ctx, state)
}

func memoryQueryForTask(state agentgraph.State) string {
	return workspacememory.MemoryQueryForTaskInput(state.TaskInput)
}

func (c *Core) onMemoryEvent(state agentgraph.State, status string, ids []string, err error) {
	kind := EventMemoryFailed
	switch status {
	case "started":
		kind = EventMemoryStarted
	case "saved":
		kind = EventMemorySaved
	case "skipped":
		kind = EventMemorySkipped
	}
	task, _ := c.StateForSession(stateSessionID(state))
	var summary string
	var failure *ModelFailure
	if status == "failed" {
		stage := "unknown"
		var extraction *agentgraph.MemoryExtractionError
		if errors.As(err, &extraction) {
			stage = extraction.Stage
		}
		summary = safeMemoryFailureSummary("memory extraction failed: " + stage)
		var request *publicmodel.RequestError
		if errors.As(err, &request) {
			failure = &ModelFailure{Category: request.Category, StatusCode: request.StatusCode,
				Code: memoryFailureIdentifier(request.Code), RequestID: memoryFailureIdentifier(request.RequestID)}
		}
		c.logger.Warn("workspace memory extraction failed", "session_id", task.SessionID,
			"execution_id", state.ExecutionID, "stage", stage, "model_failure", failure,
			"error_type", fmt.Sprintf("%T", err))
	}
	c.emit(Event{
		Kind: kind, SessionID: task.SessionID, TaskID: task.TaskID, ExecutionID: state.ExecutionID,
		MemoryStatus: status, MemoryIDs: append([]string(nil), ids...), Err: summary, ModelFailure: failure,
	})
}

func memoryFailureIdentifier(value string) string {
	if len(value) > 256 {
		return ""
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-.:", c) {
			continue
		}
		return ""
	}
	return value
}

// Only fixed phase labels may survive the durable event's error redaction.
func safeMemoryFailureSummary(value string) string {
	switch value {
	case "memory extraction failed: request", "memory extraction failed: response",
		"memory extraction failed: submission", "memory extraction failed: validation",
		"memory extraction failed: persistence", "memory extraction failed: unknown":
		return value
	default:
		return ""
	}
}

func (c *Core) saveExtractedMemories(ctx context.Context, state agentgraph.State, candidates []memory.Candidate) ([]string, error) {
	if c.memoryConfig.Disabled || c.memory == nil {
		return nil, nil
	}
	sessionID := stateSessionID(state)
	if sessionID == "" || state.ExecutionID == "" {
		return nil, errors.New("memory extraction is missing session or execution identity")
	}
	return c.memory.SaveExtracted(ctx, sessionID, state.ExecutionID, candidates)
}
