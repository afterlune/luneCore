package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/afterlune/luneCore/internal/session"
	graph "github.com/afterlune/luneGraph"
	"time"

	"github.com/afterlune/luneCore/internal/agentgraph"
	commandruntime "github.com/afterlune/luneCore/internal/command"
	"github.com/afterlune/luneCore/internal/tool"
	publictool "github.com/afterlune/luneCore/tool"
)

type CommandOptions = publictool.CommandOptions
type CommandInfo = publictool.CommandInfo
type CommandStatus = publictool.CommandStatus
type CommandOutput = publictool.CommandOutput
type CommandReadOptions = publictool.CommandReadOptions

const (
	CommandRunning     = publictool.CommandRunning
	CommandStopping    = publictool.CommandStopping
	CommandSucceeded   = publictool.CommandSucceeded
	CommandFailed      = publictool.CommandFailed
	CommandCancelled   = publictool.CommandCancelled
	CommandTimedOut    = publictool.CommandTimedOut
	CommandInterrupted = publictool.CommandInterrupted
)

func (c *Core) configureCommands(config Config) error {
	executor := config.CommandExecutor
	if executor == nil {
		executor = tool.SandboxedCommandExecutor{HelperPath: config.SandboxHelperPath}
	}
	manager, err := commandruntime.New(context.Background(), c.sessions, executor, config.Commands, func(info publictool.CommandInfo) {
		kind := EventCommandStarted
		if !info.Status.Active() {
			kind = EventCommandFinished
		}
		c.emit(Event{Kind: kind, SessionID: info.SessionID, ExecutionID: info.ExecutionID, TaskID: info.TaskID, AgentID: info.AgentID, CommandID: info.ID, CommandStatus: info.Status, CommandExitCode: info.ExitCode})
	})
	if err != nil {
		return err
	}
	c.commands = manager
	return c.registry.SetCommandManager(manager)
}

func (r *SessionRuntime) ListCommands(ctx context.Context) ([]CommandInfo, error) {
	if r == nil || r.core == nil || ctx == nil {
		return nil, errors.New("session runtime and context are required")
	}
	return r.core.commands.List(ctx, r.sessionID, "")
}

func (r *SessionRuntime) ReadCommand(ctx context.Context, id string, options CommandReadOptions) (CommandOutput, error) {
	if r == nil || r.core == nil || ctx == nil {
		return CommandOutput{}, errors.New("session runtime and context are required")
	}
	return r.core.commands.Read(ctx, r.sessionID, id, "", "", options)
}

func (r *SessionRuntime) StopCommand(ctx context.Context, id string) (CommandOutput, error) {
	if r == nil || r.core == nil || ctx == nil {
		return CommandOutput{}, errors.New("session runtime and context are required")
	}
	return r.core.commands.Stop(ctx, r.sessionID, id, "", "")
}

func (c *Core) stopCommands(executionID, agentID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	return c.commands.StopScope(ctx, executionID, agentID)
}

func (c *Core) commandHooks() (func(agentgraph.State) bool, func(agentgraph.State) error) {
	return func(state agentgraph.State) bool { return c.commands.Active(state.ExecutionID, state.AgentID) }, func(state agentgraph.State) error { return c.stopCommands(state.ExecutionID, state.AgentID) }
}

func (c *Core) commandSummary(ctx context.Context, state agentgraph.State) (string, error) {
	infos, err := c.commands.List(ctx, publictool.CallIdentityFromContext(ctx).SessionID, state.ExecutionID)
	if err != nil {
		return "", err
	}
	var owned []CommandInfo
	for _, info := range infos {
		if info.AgentID == state.AgentID {
			owned = append(owned, info)
		}
	}
	if len(owned) == 0 {
		return "", nil
	}
	if len(owned) > 16 {
		owned = owned[len(owned)-16:]
	}
	data, err := json.Marshal(owned)
	return "Runtime command states (start receipts do not prove successful completion; use command_status for saved output): " + string(data), err
}

func (c *Core) admitCommandCompletionRecovery(ctx context.Context, sessionID string) error {
	cp, err := c.runtime.CheckpointForSession(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, invocation := range cp.Invocations {
		if invocation.Status == graph.InvocationWaiting && invocation.Continuation == agentgraph.ExecutionBlockerContinuation && agentgraph.IsCommandCompletionWait(invocation.State) {
			_, err = c.sessions.PutGraphResumeInput(ctx, sessionID, session.GraphResumeInput{RunID: cp.RunID, InvocationID: invocation.ID, CallID: invocation.CallID, Payload: json.RawMessage(`""`)})
			return err
		}
	}
	return errors.New("command completion wait is missing")
}
