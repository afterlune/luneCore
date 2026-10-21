package tool

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	publictool "github.com/afterlune/luneCore/tool"
)

type sessionPathGrantStoreKey struct{}
type approvedPathGrantKey struct{}

// SessionPathGrantStore is shared by every agent in one task. New approvals
// become visible immediately and are also persisted by the runtime.
type SessionPathGrantStore struct {
	mu     sync.RWMutex
	grants []publictool.PathGrant
}

func NewSessionPathGrantStore(grants []publictool.PathGrant) *SessionPathGrantStore {
	return &SessionPathGrantStore{grants: append([]publictool.PathGrant(nil), grants...)}
}

func (s *SessionPathGrantStore) Allows(request publictool.PathGrant) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return PathGrantAllows(s.grants, request)
}

func (s *SessionPathGrantStore) Add(grant publictool.PathGrant) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if PathGrantAllows(s.grants, grant) {
		return
	}
	// Replace a narrower grant when this grant subsumes it.
	kept := s.grants[:0]
	for _, existing := range s.grants {
		if grantCovers(grant, existing) {
			continue
		}
		kept = append(kept, existing)
	}
	s.grants = append(kept, grant)
}

func (s *SessionPathGrantStore) Snapshot() []publictool.PathGrant {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]publictool.PathGrant(nil), s.grants...)
}

func WithSessionPathGrantStore(ctx context.Context, store *SessionPathGrantStore) context.Context {
	if ctx == nil || store == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionPathGrantStoreKey{}, store)
}

func SessionPathGrantStoreFromContext(ctx context.Context) *SessionPathGrantStore {
	if ctx == nil {
		return nil
	}
	store, _ := ctx.Value(sessionPathGrantStoreKey{}).(*SessionPathGrantStore)
	return store
}

func WithApprovedPathGrant(ctx context.Context, grant publictool.PathGrant) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, approvedPathGrantKey{}, grant)
}

func ApprovedPathGrantFromContext(ctx context.Context) (publictool.PathGrant, bool) {
	if ctx == nil {
		return publictool.PathGrant{}, false
	}
	grant, ok := ctx.Value(approvedPathGrantKey{}).(publictool.PathGrant)
	return grant, ok
}

// PathGrantAllows reports whether one grant covers the requested directory and
// access level. Read-write grants also cover reads.
func PathGrantAllows(grants []publictool.PathGrant, requested publictool.PathGrant) bool {
	for _, grant := range grants {
		if grantCovers(grant, requested) {
			return true
		}
	}
	return false
}

func grantCovers(grant, requested publictool.PathGrant) bool {
	if grant.Access != publictool.PathReadWrite && grant.Access != requested.Access {
		return false
	}
	root := filepath.Clean(grant.Path)
	candidate := filepath.Clean(requested.Path)
	if filepath.Separator == '\\' {
		if !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(candidate)) {
			return false
		}
		root = strings.ToLower(root)
		candidate = strings.ToLower(candidate)
	} else if filepath.VolumeName(root) != filepath.VolumeName(candidate) {
		return false
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
