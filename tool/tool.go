// Package tool exposes the extension contracts for host-provided tools and
// command execution backends.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/afterlune/luneCore/model"
)

type PermissionClass string

const (
	PermissionSessionControl PermissionClass = "session-control"
	PermissionWorkspaceRead  PermissionClass = "workspace-read"
	PermissionWorkspaceWrite PermissionClass = "workspace-write"
	PermissionCommand        PermissionClass = "command"
	PermissionNetwork        PermissionClass = "network"
)

type Category struct {
	Name        string
	Description string
}

// Metadata classifies a tool for permission modes, plan-mode filtering, and
// whether it is exposed before category discovery.
type Metadata struct {
	Category        Category
	PermissionClass PermissionClass
	ReadOnly        bool
	// AlwaysVisible exposes the tool alongside the category selector. Permission
	// and Plan mode checks still determine whether it is available.
	AlwaysVisible bool
}

func (m Metadata) Valid() bool {
	switch m.PermissionClass {
	case PermissionSessionControl, PermissionWorkspaceRead, PermissionWorkspaceWrite, PermissionCommand, PermissionNetwork:
		return true
	default:
		return false
	}
}

// Definition describes a tool to a model provider.
type Definition = model.FunctionTool

// Tool runs one model-requested operation. Its Definition is snapshotted when
// registered and must remain stable; build a new Registry to change it. The
// invocation ID is available via InvocationID(ctx) and stays stable when the
// host explicitly retries recovery. The same instance may run concurrently
// across sessions and should return promptly when ctx is canceled.
type Tool interface {
	Definition() Definition
	Run(context.Context, json.RawMessage) Result
}

type Registration struct {
	Tool     Tool
	Metadata Metadata
}

type Result struct {
	Details        *ExecutionDetails
	Output         string
	ModelOutput    *model.ToolOutput
	TranscriptOnly []model.ContentPart
	Err            error
}

func (r Result) Text() string {
	if r.Err == nil {
		return r.Output
	}
	if strings.TrimSpace(r.Output) == "" {
		return "Error: " + r.Err.Error()
	}
	return r.Output + "\nError: " + r.Err.Error()
}

type CallIdentity struct {
	SessionID    string
	TaskID       string
	RunID        string
	ExecutionID  string
	AgentID      string
	InvocationID string
}

type callIdentityKey struct{}

func WithCallIdentity(ctx context.Context, identity CallIdentity) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, callIdentityKey{}, identity)
}

func CallIdentityFromContext(ctx context.Context) CallIdentity {
	if ctx == nil {
		return CallIdentity{}
	}
	identity, _ := ctx.Value(callIdentityKey{}).(CallIdentity)
	return identity
}

func WithInvocationID(ctx context.Context, id string) context.Context {
	if ctx == nil || id == "" {
		return ctx
	}
	identity := CallIdentityFromContext(ctx)
	identity.InvocationID = id
	return WithCallIdentity(ctx, identity)
}

func InvocationID(ctx context.Context) string {
	return CallIdentityFromContext(ctx).InvocationID
}

type CommandRequest struct {
	// OnOutput receives transient stdout/stderr bytes during Execute only.
	// Implementations must stop on errors and finish callbacks before returning.
	OnOutput     func([]byte) error `json:"-"`
	Command      string
	Workspace    string
	InvocationID string
	// Background keeps the command alive after its agent task ends. The host
	// still owns it and stops it when the session or Core closes.
	Background bool `json:"background,omitempty"`
	Sandbox    SandboxPolicy
}

// PathAccess describes one path scope explicitly approved for a plan.
type PathAccess string

const (
	PathRead      PathAccess = "read"
	PathReadWrite PathAccess = "read_write"
)

// PathGrant adds an absolute directory to a permission snapshot. The snapshot
// owner determines whether the grant is session-wide or plan-scoped.
type PathGrant struct {
	Path   string     `json:"path"`
	Access PathAccess `json:"access"`
}

// ApprovalDecision represents the user's explicit decision for a permission request.
type ApprovalDecision string

const (
	ApprovalAllowOnce    ApprovalDecision = "allow_once"
	ApprovalAllowSession ApprovalDecision = "allow_session"
	ApprovalDeny         ApprovalDecision = "deny"
)

// SessionApproval records an approved tool operation for the lifetime of a session.
type SessionApproval struct {
	Tool        string `json:"tool"`
	Arguments   string `json:"arguments,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// PlanAuthorization records the host-approved capabilities for one plan run.
// Paths and high-risk commands are exact grants; an empty value grants nothing.
type PlanAuthorization struct {
	Paths                    []PathGrant `json:"paths,omitempty"`
	AllowNetwork             bool        `json:"allow_network,omitempty"`
	AllowedTools             []string    `json:"allowed_tools,omitempty"`
	ApprovedHighRiskCommands []string    `json:"approved_high_risk_commands,omitempty"`
}

// SandboxPolicy is passed to command executors for every invocation. The
// built-in executor enforces this policy with the operating system sandbox.
type SandboxPolicy struct {
	PermissionMode string      `json:"permission_mode"`
	Workspace      string      `json:"workspace"`
	HomeDir        string      `json:"home_dir,omitempty"`
	Grants         []PathGrant `json:"grants,omitempty"`
	// CommandToolchainPaths are host-authorized directories added to the child
	// command search path. They remain read and execute only under the sandbox.
	CommandToolchainPaths []string `json:"command_toolchain_paths,omitempty"`
	Network               bool     `json:"network,omitempty"`
	TempDir               string   `json:"temp_dir,omitempty"`
	// CacheDir is a host-selected, workspace-scoped persistent cache directory.
	// The Linux sandbox grants write access only to this directory.
	CacheDir string `json:"cache_dir,omitempty"`
}

type planAuthorizationContextKey struct{}
type noInteractiveDecisionsContextKey struct{}

// WithPlanAuthorization scopes pre-approved plan capabilities to a task.
func WithPlanAuthorization(ctx context.Context, authorization PlanAuthorization) context.Context {
	if ctx == nil {
		return nil
	}
	copy := authorization
	copy.Paths = append([]PathGrant(nil), authorization.Paths...)
	copy.AllowedTools = append([]string(nil), authorization.AllowedTools...)
	copy.ApprovedHighRiskCommands = append([]string(nil), authorization.ApprovedHighRiskCommands...)
	return context.WithValue(ctx, planAuthorizationContextKey{}, copy)
}

// PlanAuthorizationFromContext returns the capabilities approved for this task.
func PlanAuthorizationFromContext(ctx context.Context) PlanAuthorization {
	if ctx == nil {
		return PlanAuthorization{}
	}
	authorization, _ := ctx.Value(planAuthorizationContextKey{}).(PlanAuthorization)
	return authorization
}

// WithNoInteractiveDecisions prevents approval and question callbacks after a
// plan has started. Unapproved operations are reported as blockers.
func WithNoInteractiveDecisions(ctx context.Context, disabled bool) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, noInteractiveDecisionsContextKey{}, disabled)
}

func NoInteractiveDecisions(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	disabled, _ := ctx.Value(noInteractiveDecisionsContextKey{}).(bool)
	return disabled
}

type CommandResult struct {
	ExitCode *int
	Output   string
}

// CommandExecutor executes a shell command. The built-in executor enforces
// CommandRequest.Sandbox with the OS. A host override is trusted and must
// enforce the supplied policy itself. Implementations must support concurrent
// calls from separate sessions and should return promptly when ctx is canceled.
type CommandExecutor interface {
	Execute(context.Context, CommandRequest) (CommandResult, error)
}

var ErrInvalidTool = errors.New("invalid tool registration")

// ExecutionBlockerError means a task needs a permission decision that was not
// included in its approved plan. Hosts should report the blocker and ask the
// user to re-plan instead of opening an execution-time approval prompt.
type ExecutionBlockerError struct {
	Tool   string
	Reason string
}

func (e *ExecutionBlockerError) Error() string {
	if e == nil {
		return "execution is blocked by an unapproved action"
	}
	if e.Reason == "" {
		return "execution is blocked by an unapproved action: " + e.Tool
	}
	return e.Reason
}
