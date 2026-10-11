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
	PermissionWorkspaceRead  PermissionClass = "workspace-read"
	PermissionWorkspaceWrite PermissionClass = "workspace-write"
	PermissionCommand        PermissionClass = "command"
	PermissionNetwork        PermissionClass = "network"
)

type Category struct {
	Name        string
	Description string
}

// Metadata classifies a tool for permission modes and plan-mode filtering.
type Metadata struct {
	Category        Category
	PermissionClass PermissionClass
	ReadOnly        bool
}

func (m Metadata) Valid() bool {
	switch m.PermissionClass {
	case PermissionWorkspaceRead, PermissionWorkspaceWrite, PermissionCommand, PermissionNetwork:
		return true
	default:
		return false
	}
}

// Definition describes a tool to a model provider.
type Definition = model.FunctionTool

// Tool runs one model-requested operation. The invocation ID is available via
// InvocationID(ctx) and stays stable when the host explicitly retries recovery.
// The same registered instance may run concurrently for different sessions.
type Tool interface {
	Definition() Definition
	Run(context.Context, json.RawMessage) Result
}

type Registration struct {
	Tool     Tool
	Metadata Metadata
}

type Result struct {
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
	Command      string
	Workspace    string
	InvocationID string
}

type CommandResult struct {
	Output string
}

// CommandExecutor executes a shell command. Hosts may bind this to an OS-level
// sandbox. Direct local execution is available only when explicitly enabled.
// Implementations must support concurrent calls from separate sessions.
type CommandExecutor interface {
	Execute(context.Context, CommandRequest) (CommandResult, error)
}

var ErrInvalidTool = errors.New("invalid tool registration")
