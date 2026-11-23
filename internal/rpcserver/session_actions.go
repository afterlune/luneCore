package rpcserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	core "github.com/afterlune/luneCore"
)

type transcriptItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func historyDigest(raw json.RawMessage) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func transcript(raw json.RawMessage) []transcriptItem {
	var items []transcriptItem
	if json.Unmarshal(raw, &items) == nil {
		return items
	}
	var wrapped struct {
		Items []transcriptItem `json:"items"`
	}
	_ = json.Unmarshal(raw, &wrapped)
	return wrapped.Items
}

func chatText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Text != "" {
			builder.WriteString(part.Text)
			builder.WriteByte('\n')
		}
	}
	return strings.TrimSpace(builder.String())
}

func containsChatText(session core.Session, term string) bool {
	if strings.Contains(strings.ToLower(session.Title), term) || strings.Contains(strings.ToLower(session.ID), term) {
		return true
	}
	for _, item := range transcript(session.History) {
		if item.Type == "message" && (item.Role == "assistant" || item.Role == "user") &&
			strings.Contains(strings.ToLower(chatText(item.Content)), term) {
			return true
		}
	}
	return false
}

func (c *connection) searchSessions(raw json.RawMessage) (any, *rpcError) {
	var params struct {
		Query           string `json:"query"`
		Offset          int    `json:"offset"`
		Limit           int    `json:"limit"`
		IncludeArchived bool   `json:"includeArchived"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	term := strings.ToLower(strings.TrimSpace(params.Query))
	if term == "" || len(term) > 512 || params.Offset < 0 || params.Limit < 0 || params.Limit > 100 {
		return nil, &rpcError{Code: -32602, Message: "query, non-negative offset, and limit up to 100 are required"}
	}
	if params.Limit == 0 {
		params.Limit = 50
	}
	const pageSize = 100
	matches := make([]sessionSummary, 0, params.Limit)
	skipped := 0
	for offset := 0; ; offset += pageSize {
		page, err := c.server.core.ListSessionPage(c.ctx, offset, pageSize, params.IncludeArchived)
		if err != nil {
			return nil, applicationError(err)
		}
		for _, saved := range page {
			if !containsChatText(saved, term) {
				continue
			}
			if skipped < params.Offset {
				skipped++
				continue
			}
			matches = append(matches, summarizeSession(saved))
			if len(matches) == params.Limit {
				return matches, nil
			}
		}
		if len(page) < pageSize {
			return matches, nil
		}
	}
}

func (c *connection) manageSession(method string, raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID   string `json:"sessionId"`
		ExecutionID string `json:"executionId"`
		Turns       int    `json:"turns"`
		Digest      string `json:"digest"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	switch method {
	case "session.fork":
		saved, err := c.server.core.ForkSession(c.ctx, params.SessionID)
		if err != nil {
			return nil, applicationError(err)
		}
		return summarizeSession(saved), nil
	case "session.compact":
		result, err := c.server.core.Compact(c.ctx, params.SessionID)
		return result, applicationError(err)
	case "session.rewindPreview":
		if params.Turns <= 0 {
			return nil, &rpcError{Code: -32602, Message: "turns must be positive"}
		}
		saved, err := c.server.core.GetSession(c.ctx, params.SessionID)
		if err != nil {
			return nil, applicationError(err)
		}
		var turns []string
		for _, item := range transcript(saved.History) {
			if item.Type != "message" || item.Role != "user" {
				continue
			}
			text := chatText(item.Content)
			runes := []rune(text)
			if len(runes) > 120 {
				text = string(runes[:120]) + "…"
			}
			if text == "" {
				text = "(attachment)"
			}
			turns = append(turns, text)
		}
		count := min(len(turns), params.Turns)
		excerpts := turns[len(turns)-min(count, 10):]
		return map[string]any{"removed": count, "digest": historyDigest(saved.History), "excerpts": excerpts}, nil
	case "session.rewind":
		if params.Turns <= 0 || len(params.Digest) != 64 {
			return nil, &rpcError{Code: -32602, Message: "turns and a preview digest are required"}
		}
		removed, err := c.server.core.RewindSessionIfCurrent(c.ctx, params.SessionID, params.Turns, params.Digest)
		return map[string]int{"removed": removed}, applicationError(err)
	case "turn.cancelQueued":
		return c.withRuntime(c.ctx, params.SessionID, func(runtime *core.SessionRuntime) (any, error) {
			err := runtime.CancelQueuedTurn(c.ctx, params.ExecutionID)
			return map[string]bool{"cancelled": err == nil}, err
		})
	}
	return nil, &rpcError{Code: -32601, Message: "method not found"}
}
