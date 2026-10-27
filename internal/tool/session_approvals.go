package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"

	publictool "github.com/afterlune/luneCore/tool"
)

type sessionApprovalStoreKey struct{}

// SessionApprovalStore is shared across tasks within one session. Approved tool
// calls become visible immediately and are persisted by the runtime.
type SessionApprovalStore struct {
	mu        sync.RWMutex
	approvals []publictool.SessionApproval
}

// NewSessionApprovalStore creates a store with initial session approvals.
func NewSessionApprovalStore(approvals []publictool.SessionApproval) *SessionApprovalStore {
	store := &SessionApprovalStore{
		approvals: make([]publictool.SessionApproval, 0, len(approvals)),
	}
	for _, a := range approvals {
		// An approval without a fingerprint has no execution boundary to bind to,
		// so it cannot be honored safely; the user re-approves it once.
		if a.Fingerprint == "" {
			continue
		}
		store.approvals = append(store.approvals, publictool.SessionApproval{
			Tool:        a.Tool,
			Arguments:   NormalizeArguments(json.RawMessage(a.Arguments)),
			Fingerprint: a.Fingerprint,
		})
	}
	return store
}

// NormalizeArguments produces a canonical JSON representation without key-order or whitespace variance.
func NormalizeArguments(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return ""
	}
	var val any
	if err := json.Unmarshal(trimmed, &val); err != nil {
		var buf bytes.Buffer
		if err := json.Compact(&buf, trimmed); err == nil {
			return buf.String()
		}
		return strings.TrimSpace(string(trimmed))
	}
	canonical, err := json.Marshal(val)
	if err != nil {
		return strings.TrimSpace(string(trimmed))
	}
	if string(canonical) == "{}" || string(canonical) == "null" {
		return ""
	}
	return string(canonical)
}

// Allows reports whether the exact tool call was already approved inside the same
// execution boundary. A missing fingerprint never matches: approvals are scoped to
// the workspace, shell, toolchains, and sandbox state that produced them.
func (s *SessionApprovalStore) Allows(tool string, arguments json.RawMessage, fingerprint string) bool {
	if s == nil || tool == "" || fingerprint == "" {
		return false
	}
	norm := NormalizeArguments(arguments)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.approvals {
		if a.Tool == tool && a.Arguments == norm && a.Fingerprint == fingerprint {
			return true
		}
	}
	return false
}

// Add records an approved tool operation in the store.
func (s *SessionApprovalStore) Add(approval publictool.SessionApproval) {
	if s == nil || approval.Tool == "" || approval.Fingerprint == "" {
		// Nothing binds an approval without a boundary fingerprint, so storing it
		// would either leak across boundaries or never match.
		return
	}
	norm := NormalizeArguments(json.RawMessage(approval.Arguments))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.approvals {
		if a.Tool == approval.Tool && a.Arguments == norm && a.Fingerprint == approval.Fingerprint {
			return
		}
	}
	s.approvals = append(s.approvals, publictool.SessionApproval{
		Tool:        approval.Tool,
		Arguments:   norm,
		Fingerprint: approval.Fingerprint,
	})
}

// Snapshot returns a copy of all session approvals.
func (s *SessionApprovalStore) Snapshot() []publictool.SessionApproval {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]publictool.SessionApproval(nil), s.approvals...)
}

// WithSessionApprovalStore scopes the store to the context.
func WithSessionApprovalStore(ctx context.Context, store *SessionApprovalStore) context.Context {
	if ctx == nil || store == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionApprovalStoreKey{}, store)
}

// SessionApprovalStoreFromContext returns the store scoped to the context.
func SessionApprovalStoreFromContext(ctx context.Context) *SessionApprovalStore {
	if ctx == nil {
		return nil
	}
	store, _ := ctx.Value(sessionApprovalStoreKey{}).(*SessionApprovalStore)
	return store
}
