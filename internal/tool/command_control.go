package tool

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/afterlune/luneCore/internal/command"
	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type CommandControlTool struct {
	Manager *command.Manager
	Stop    bool
}
type commandControlArgs struct {
	ID     string `json:"command_id"`
	Cursor int64  `json:"cursor"`
	Wait   *int64 `json:"wait_ms"`
}

func (t CommandControlTool) Definition() model.FunctionTool {
	if t.Stop {
		return functionDefinition("command_stop", "Stop a command owned by this agent, or a managed background service in this session. Stopping does not mean it succeeded.", `{"type":"object","properties":{"command_id":{"type":"string"}},"required":["command_id"],"additionalProperties":false}`)
	}
	return functionDefinition("command_status", "Read this agent's command, or a managed background service in this session, from a byte cursor; wait up to 5 seconds. A start receipt is not completion.", `{"type":"object","properties":{"command_id":{"type":"string"},"cursor":{"type":"integer","minimum":0,"description":"Byte offset into saved output; defaults to 0"},"wait_ms":{"type":"integer","minimum":0,"maximum":5000,"description":"Maximum wait; defaults to 1000 ms"}},"required":["command_id"],"additionalProperties":false}`)
}

func (t CommandControlTool) authorizeCommand(ctx context.Context, raw json.RawMessage) error {
	var args commandControlArgs
	if err := decodeArguments(raw, &args); err != nil {
		return err
	}
	identity := publictool.CallIdentityFromContext(ctx)
	if identity.SessionID == "" || identity.ExecutionID == "" || identity.AgentID == "" {
		return errors.New("command owner identity is required")
	}
	_, err := t.Manager.Read(ctx, identity.SessionID, args.ID, identity.ExecutionID, identity.AgentID, publictool.CommandReadOptions{})
	return err
}

func (t CommandControlTool) Run(ctx context.Context, raw json.RawMessage) Result {
	var args commandControlArgs
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: err}
	}
	identity := publictool.CallIdentityFromContext(ctx)
	if identity.SessionID == "" || identity.ExecutionID == "" || identity.AgentID == "" {
		return Result{Err: errors.New("command owner identity is required")}
	}
	var output publictool.CommandOutput
	var err error
	if t.Stop {
		output, err = t.Manager.Stop(ctx, identity.SessionID, args.ID, identity.ExecutionID, identity.AgentID)
	} else {
		wait := time.Second
		if args.Wait != nil {
			if *args.Wait < 0 || *args.Wait > 5000 {
				return Result{Err: errors.New("wait_ms must be between 0 and 5000")}
			}
			wait = time.Duration(*args.Wait) * time.Millisecond
		}
		output, err = t.Manager.Read(ctx, identity.SessionID, args.ID, identity.ExecutionID, identity.AgentID, publictool.CommandReadOptions{Cursor: args.Cursor, Wait: wait})
	}
	if err != nil {
		return Result{Err: err}
	}
	data, err := json.Marshal(output)
	snapshot, readErr := t.Manager.Read(ctx, identity.SessionID, args.ID, identity.ExecutionID, identity.AgentID, publictool.CommandReadOptions{})
	if readErr != nil {
		snapshot = output
	}
	return Result{Output: string(data), Err: err, Details: &publictool.ExecutionDetails{Command: &snapshot}}
}

// Controls enforce both tool permissions and ownership. Task commands are limited
// to their owner; session-managed
// background commands are limited to their originating session.
func (r *Registry) SetCommandManager(manager *command.Manager) error {
	r.mu.Lock()
	existing, ok := r.tools["command"].(CommandTool)
	if ok {
		existing.Manager = manager
		r.tools["command"] = existing
	}
	r.mu.Unlock()
	if !ok {
		return errors.New("built-in command tool is unavailable")
	}
	metadata := model.ToolMetadata{Category: model.ToolCategory{Name: "command", Description: "Run workspace commands."}, PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true}
	for _, stop := range []bool{false, true} {
		metadata.PermissionClass = model.ToolPermissionWorkspaceRead
		metadata.ReadOnly = !stop
		if stop {
			metadata.PermissionClass = model.ToolPermissionCommand
		}
		if err := r.Register(ToolRegistration{Tool: CommandControlTool{Manager: manager, Stop: stop}, Metadata: metadata}); err != nil {
			return err
		}
	}
	return nil
}
