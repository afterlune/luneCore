package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/afterlune/luneCore/internal/model"
)

// PermissionMode controls how registered tools are authorized.
type PermissionMode string

const (
	PermissionModeReadOnly   PermissionMode = "read-only"
	PermissionModeWorkspace  PermissionMode = "workspace"
	PermissionModeAsk        PermissionMode = "ask"
	PermissionModeFullAccess PermissionMode = "full-access"
)

// PermissionModes returns the supported permission modes in UI order.
func PermissionModes() []PermissionMode {
	return []PermissionMode{
		PermissionModeReadOnly,
		PermissionModeWorkspace,
		PermissionModeAsk,
		PermissionModeFullAccess,
	}
}

// PermissionModeDescription returns the user-facing explanation for a mode.
func PermissionModeDescription(mode PermissionMode) string {
	switch mode {
	case PermissionModeReadOnly:
		return "Read workspace files; ask before web; block writes and shell"
	case PermissionModeWorkspace:
		return "Workspace file operations; commands and web need approval"
	case PermissionModeAsk:
		return "Read workspace files; Ask before every other tool"
	case PermissionModeFullAccess:
		return "Allow all tools; shell can access paths outside the workspace"
	default:
		return ""
	}
}

// PermissionRequest contains the exact tool input shown before one-time approval.
type PermissionRequest struct {
	Tool      string
	AgentID   string
	Arguments json.RawMessage
	Metadata  model.ToolMetadata
}

// PermissionApprovalHandler asks the user to allow one tool call.
type PermissionApprovalHandler func(context.Context, PermissionRequest) bool

type permissionDisposition uint8

const (
	permissionAllowed permissionDisposition = iota
	permissionNeedsApproval
	permissionDenied
)

type permissionSettings struct {
	mu      sync.RWMutex
	mode    PermissionMode
	approve PermissionApprovalHandler
}

func newPermissionSettings(mode PermissionMode) *permissionSettings {
	return &permissionSettings{mode: mode}
}

func (p *permissionSettings) modeValue() PermissionMode {
	if p == nil {
		return PermissionModeFullAccess
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode
}

func (p *permissionSettings) setMode(mode PermissionMode) error {
	if p == nil {
		return errors.New("permission settings are unavailable")
	}
	switch mode {
	case PermissionModeReadOnly, PermissionModeWorkspace, PermissionModeAsk, PermissionModeFullAccess:
	default:
		return fmt.Errorf("unknown permission mode %q", mode)
	}
	p.mu.Lock()
	p.mode = mode
	p.mu.Unlock()
	return nil
}

func (p *permissionSettings) setApprovalHandler(handler PermissionApprovalHandler) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.approve = handler
	p.mu.Unlock()
}

func (p *permissionSettings) authorize(ctx context.Context, request PermissionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mode, approve, disposition := p.policy(request.Metadata)
	switch disposition {
	case permissionAllowed:
		return ctx.Err()
	case permissionNeedsApproval:
		return requestApproval(ctx, approve, request)
	case permissionDenied:
		if mode == PermissionModeReadOnly {
			return fmt.Errorf("tool %q is disabled in read-only mode", request.Tool)
		}
		if mode == PermissionModeWorkspace && !request.Metadata.PermissionClass.Valid() {
			return fmt.Errorf("tool %q is unclassified and disabled in workspace mode", request.Tool)
		}
		return fmt.Errorf("unknown permission mode %q", mode)
	}
	return fmt.Errorf("unknown permission mode %q", mode)
}

func (p *permissionSettings) policy(metadata model.ToolMetadata) (PermissionMode, PermissionApprovalHandler, permissionDisposition) {
	if p == nil {
		return PermissionModeFullAccess, nil, permissionAllowed
	}
	p.mu.RLock()
	mode, approve := p.mode, p.approve
	p.mu.RUnlock()
	return mode, approve, permissionPolicy(mode, metadata.PermissionClass)
}

func permissionPolicy(mode PermissionMode, class model.ToolPermissionClass) permissionDisposition {
	switch mode {
	case PermissionModeReadOnly:
		switch class {
		case model.ToolPermissionWorkspaceRead:
			return permissionAllowed
		case model.ToolPermissionNetwork:
			return permissionNeedsApproval
		default:
			return permissionDenied
		}
	case PermissionModeWorkspace:
		switch class {
		case model.ToolPermissionWorkspaceRead, model.ToolPermissionWorkspaceWrite:
			return permissionAllowed
		case model.ToolPermissionCommand, model.ToolPermissionNetwork:
			return permissionNeedsApproval
		default:
			return permissionDenied
		}
	case PermissionModeAsk:
		if class == model.ToolPermissionWorkspaceRead {
			return permissionAllowed
		}
		return permissionNeedsApproval
	case PermissionModeFullAccess:
		return permissionAllowed
	default:
		return permissionDenied
	}
}

func requestApproval(ctx context.Context, approve PermissionApprovalHandler, request PermissionRequest) error {
	if approve == nil {
		return fmt.Errorf("tool %q requires approval, but no interactive approval handler is available; use /permissions full-access to allow it without prompting", request.Tool)
	}
	approved := approve(ctx, request)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !approved {
		return fmt.Errorf("permission denied for tool %q", request.Tool)
	}
	return ctx.Err()
}
