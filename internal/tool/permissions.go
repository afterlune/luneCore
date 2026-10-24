package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
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
		return "Read workspace files; ask before external directories and web; block writes and commands"
	case PermissionModeWorkspace:
		return "Read and write workspace files; ask before external directories, commands, and web"
	case PermissionModeAsk:
		return "Read workspace files; ask before every other operation and external directory"
	case PermissionModeFullAccess:
		return "Allow registered tools and current-user files without prompts; extremely dangerous commands remain blocked"
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
	PathGrant *publictool.PathGrant
}

// PermissionApprovalHandler asks the user to allow one tool call.
type PermissionApprovalHandler func(context.Context, PermissionRequest) bool

type permissionModeContextKey struct{}

// WithPermissionMode scopes an execution-only permission override to a tool call.
// An empty mode leaves the registry's active mode in effect.
func WithPermissionMode(ctx context.Context, mode string) context.Context {
	if ctx == nil || mode == "" {
		return ctx
	}
	return context.WithValue(ctx, permissionModeContextKey{}, PermissionMode(mode))
}

func permissionModeFromContext(ctx context.Context) (PermissionMode, bool) {
	if ctx == nil {
		return "", false
	}
	mode, ok := ctx.Value(permissionModeContextKey{}).(PermissionMode)
	return mode, ok
}

func PermissionModeFromContext(ctx context.Context) PermissionMode {
	mode, _ := permissionModeFromContext(ctx)
	return mode
}

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
	if override, ok := permissionModeFromContext(ctx); ok {
		mode = override
		disposition = permissionPolicy(mode, request.Metadata.PermissionClass)
	}
	if disposition == permissionDenied {
		if publictool.NoInteractiveDecisions(ctx) {
			return &publictool.ExecutionBlockerError{Tool: request.Tool, Reason: fmt.Sprintf("%s is outside the permissions approved for this plan", request.Tool)}
		}
		if mode == PermissionModeReadOnly {
			return fmt.Errorf("tool %q is disabled in read-only mode", request.Tool)
		}
		if mode == PermissionModeWorkspace && !request.Metadata.PermissionClass.Valid() {
			return fmt.Errorf("tool %q is unclassified and disabled in workspace mode", request.Tool)
		}
		return fmt.Errorf("unknown permission mode %q", mode)
	}

	operationNeedsApproval := disposition == permissionNeedsApproval && !planAuthorizes(ctx, request)
	pathNeedsApproval := request.PathGrant != nil && mode != PermissionModeFullAccess &&
		!PathGrantAllows(publictool.PlanAuthorizationFromContext(ctx).Paths, *request.PathGrant) &&
		!SessionPathGrantStoreFromContext(ctx).Allows(*request.PathGrant)
	if !operationNeedsApproval && !pathNeedsApproval {
		return ctx.Err()
	}
	if publictool.NoInteractiveDecisions(ctx) {
		reason := fmt.Sprintf("%s requires approval that was not included in the execution plan", request.Tool)
		if pathNeedsApproval {
			access := "read"
			if request.PathGrant.Access == publictool.PathReadWrite {
				access = "read/write"
			}
			reason = fmt.Sprintf("%s access to %q was not included in the execution plan", access, request.PathGrant.Path)
		}
		return &publictool.ExecutionBlockerError{Tool: request.Tool, Reason: reason}
	}
	approvalRequest := request
	if !pathNeedsApproval {
		approvalRequest.PathGrant = nil
	}
	return requestApproval(ctx, approve, approvalRequest)
}

func planAuthorizes(ctx context.Context, request PermissionRequest) bool {
	authorization := publictool.PlanAuthorizationFromContext(ctx)
	if slices.Contains(authorization.AllowedTools, request.Tool) {
		return true
	}
	return request.Metadata.PermissionClass == model.ToolPermissionNetwork && authorization.AllowNetwork
}

func permissionModeFromRegistry(ctx context.Context, permissions *permissionSettings, metadata model.ToolMetadata) PermissionMode {
	if mode, ok := permissionModeFromContext(ctx); ok {
		return mode
	}
	if permissions == nil {
		return PermissionModeFullAccess
	}
	mode, _, _ := permissions.policy(metadata)
	return mode
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
	if class == model.ToolPermissionSessionControl && (mode == PermissionModeReadOnly || mode == PermissionModeWorkspace || mode == PermissionModeAsk || mode == PermissionModeFullAccess) { return permissionAllowed }
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
