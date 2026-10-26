package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
)

// Compact reduces the model context with the same engine as automatic
// compaction: recent turns are kept verbatim, old tool outputs are pruned first,
// and only then is a structured summary requested. It returns the measured
// before/after estimates so hosts can report the result.
func (r *Responder) Compact(ctx context.Context) (agentgraph.CompactionResult, error) {
	if ctx == nil {
		return agentgraph.CompactionResult{}, errors.New("context is nil")
	}
	if r.requester == nil {
		return agentgraph.CompactionResult{}, errors.New("conversation compaction is unavailable")
	}
	if pending, err := r.HasPending(); err != nil {
		return agentgraph.CompactionResult{}, errors.New("inspect pending task: " + err.Error())
	} else if pending {
		return agentgraph.CompactionResult{}, errors.New("a task is paused in this session; continue it or start a new session before compacting")
	}

	history := r.ModelContextHistory()
	items := responseHistoryItems(history)
	userTurns := 0
	for _, item := range items {
		if message, ok := item.(model.InputMessage); ok && message.Role == model.RoleUser && !agentgraph.IsHostContextMessage(message) {
			userTurns++
		}
	}
	if userTurns <= 1 {
		return agentgraph.CompactionResult{}, nil
	}

	request := model.ModelRequest{Model: r.modelName, Input: history}
	if request.Model == "" {
		request.Model = model.DeepSeekFlashModel
	}
	before := model.EstimateInputTokens(request)
	if r.taskOptions.Protocol == model.ProtocolResponses && r.taskOptions.ServerCompaction {
		remoteRequest := request
		remoteRequest.Instructions = "Compact this conversation into a context checkpoint for another LLM that will continue the task. Preserve the current goal, user constraints and preferences, decisions, unresolved work, important paths and IDs, and useful completed tool results. Return the compacted context for continued use."
		response, err := r.requester.CompactResponse(ctx, remoteRequest)
		if err == nil {
			result, items, convertErr := serverCompactionResult(request, response)
			if convertErr != nil {
				return agentgraph.CompactionResult{Before: before, Backend: "server"}, convertErr
			}
			r.historyMu.Lock()
			r.history = model.NewResponseInputItems(items...)
			r.historyMu.Unlock()
			return result, nil
		}
		if !errors.Is(err, model.ErrCompactionUnsupported) {
			return agentgraph.CompactionResult{Before: before, Backend: "server"}, err
		}
		result, localErr := r.compactLocally(ctx, request)
		result.Fallback = true
		result.FallbackReason = err.Error()
		return result, localErr
	}
	return r.compactLocally(ctx, request)
}

func (r *Responder) compactLocally(ctx context.Context, request model.ModelRequest) (agentgraph.CompactionResult, error) {
	options := agentgraph.CompactionOptions{
		ContextWindow:       r.taskOptions.ContextWindow,
		ReserveOutputTokens: r.taskOptions.ReserveOutputTokens,
		Ratio:               r.taskOptions.AutoCompactThreshold,
		Force:               true,
		Reason:              "manual",
	}
	summarize := func(ctx context.Context, summaryRequest model.ModelRequest) (string, error) {
		ctx = model.WithUsagePurpose(ctx, model.UsagePurposeCompaction)
		var response *model.ModelResponse
		var err error
		if r.agent != nil {
			response, err = r.agent.RetryResponse(ctx, r.requester, summaryRequest)
		} else {
			response, err = r.requester.CreateResponse(ctx, summaryRequest)
		}
		if err != nil {
			return "", err
		}
		if response == nil {
			return "", errors.New("model returned no response")
		}
		if response.Status != "" && response.Status != "completed" {
			return "", errors.New("model response status is " + response.Status)
		}
		summary := assistantResponseText(response)
		if summary == "" {
			return "", errors.New("model returned an empty summary")
		}
		return summary, nil
	}

	result, err := agentgraph.CompactHistory(ctx, request, options, summarize)
	if err != nil {
		return result, err
	}
	if result.Compacted {
		r.historyMu.Lock()
		r.history = model.NewResponseInputItems(result.Items...)
		r.historyMu.Unlock()
	}
	return result, nil
}

func serverCompactionResult(request model.ModelRequest, response *model.ModelResponse) (agentgraph.CompactionResult, []model.InputItem, error) {
	before := model.EstimateInputTokens(request)
	result := agentgraph.CompactionResult{Before: before, Backend: "server", Mode: "compact"}
	if response == nil {
		return result, nil, errors.New("provider returned no compacted response")
	}
	if response.Status != "" && response.Status != "completed" {
		return result, nil, errors.New("provider compaction status is " + response.Status)
	}
	items := make([]model.InputItem, 0, len(response.Output))
	hasCompactionState := false
	for _, output := range response.Output {
		if _, ok := output.(model.OutputCompaction); ok {
			hasCompactionState = true
		}
		encoded, err := json.Marshal(output)
		if err != nil {
			return result, nil, err
		}
		items = append(items, model.UnknownInputItem{Raw: encoded})
	}
	if !hasCompactionState {
		return result, nil, errors.New("provider compact response did not contain a compaction state item")
	}
	result.Items = items
	result.After = model.EstimateInputTokens(withHistory(request, items))
	result.Turns = max(0, userTurnCount(request.Input)-userTurnCount(model.NewResponseInputItems(items...)))
	result.Compacted = true
	return result, items, nil
}

func withHistory(request model.ModelRequest, items []model.InputItem) model.ModelRequest {
	request.Input = model.NewResponseInputItems(items...)
	return request
}

func userTurnCount(input *model.ResponseInput) int {
	if input == nil {
		return 0
	}
	count := 0
	for _, item := range input.Items {
		message, ok := item.(model.InputMessage)
		if ok && message.Role == model.RoleUser && !agentgraph.IsHostContextMessage(message) {
			count++
		}
	}
	return count
}

func responseHistoryItems(history *model.ResponseInput) []model.InputItem {
	if history == nil {
		return nil
	}
	if history.Items != nil {
		return append([]model.InputItem(nil), history.Items...)
	}
	if history.Text != nil {
		return []model.InputItem{model.InputMessage{
			Role:    model.RoleUser,
			Content: model.NewMessageText(*history.Text),
		}}
	}
	return nil
}

func assistantResponseText(response *model.Response) string {
	var output string
	for _, item := range response.Output {
		message, ok := item.(model.OutputMessage)
		if !ok {
			continue
		}
		for _, part := range message.Content {
			if text, ok := part.(model.OutputText); ok {
				output += text.Text
			}
		}
	}
	return strings.TrimSpace(output)
}
