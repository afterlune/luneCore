# Lune Core

Lune Core is the Go runtime library used by the Lune front ends (TUI, CLI, desktop, and A2A service). Applications construct `*core.Core` in their own process and use the public API for sessions, queued turns, persistent goals, approvals, streaming events, usage tracking, MCP integrations, and tools. It does not run an external background daemon or expose an IPC/network service.

## Install

```sh
go get github.com/afterlune/luneCore@latest
```

## Quick start

```go
package main

import (
	"context"
	"encoding/json"
	"log"

	core "github.com/afterlune/luneCore"
)

func main() {
	ctx := context.Background()
	// Core loads config.json from the workspace by default, or accepts
	// programmatic model/project settings via core.Config.
	client, err := core.New(core.Config{Workspace: "."})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close(context.Background())

	session, found, err := client.LatestSession(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if !found {
		session, err = client.CreateSession(ctx, json.RawMessage("null"))
		if err != nil {
			log.Fatal(err)
		}
	}

	input, err := core.NewTaskInput("Review this workspace.")
	if err != nil {
		log.Fatal(err)
	}
	_, err = client.RunTask(ctx, core.TaskRequest{SessionID: session.ID, Input: input})
	if err != nil {
		log.Fatal(err)
	}
}
```

The workspace supplies `config.json` for model profiles, project runtime settings, and tool configurations. Provider credentials and model profiles live under `models`; execution defaults live under `runtime`; attachment limits live under `attachments`; external tool servers live under `mcpServers`:

```json
{
  "models": {
    "providers": {
      "deepseek": {
        "protocol": "responses",
        "baseURL": "https://api.deepseek.com",
        "apiKey": "your-api-key",
        "serverCompaction": true
      },
      "primary": {
        "protocol": "responses",
        "baseURL": "https://api.example.com",
        "apiKey": "your-api-key",
        "serverCompaction": true
      },
      "anthropic": {
        "protocol": "anthropic-messages",
        "baseURL": "https://api.anthropic.com",
        "apiKey": "your-api-key"
      }
    },
    "profiles": {
      "main": {
        "provider": "primary",
        "model": "model-id",
        "displayName": "Primary Model",
        "contextWindow": 1000000,
        "maxOutputTokens": 8192,
        "supportsInputAudio": false
      }
    },
    "defaultProfile": "main"
  },
  "runtime": {
    "contextWindow": 0,
    "maxParallelAgents": 4,
    "maxTeamChildrenPerAgent": 4,
    "maxTeamDepth": 2,
    "autoCompact": true,
    "autoCompactThreshold": 0.8,
    "reserveOutputTokens": 0
  },
  "attachments": {
    "maxFiles": 5,
    "maxFileBytes": 10485760,
    "maxTotalBytes": 20971520
  },
  "mcpServers": {
    "local-tools": {
      "transport": "stdio",
      "command": "tool-binary",
      "args": ["--mode", "server"],
      "env": {
        "SERVER_TOKEN": "secret-value"
      }
    },
    "remote-tools": {
      "transport": "streamable-http",
      "url": "https://mcp.example.com/api",
      "clientMetadataURL": "https://example.com/oauth/client.json"
    }
  }
}
```

This grouped shape is required. Core rejects the previous flat config format and points to `config.example.json`; update existing files before upgrading. Providers with the `responses` protocol may enable `serverCompaction: true` to request server-side context compaction.

## Storage and Session Runtime

Core stores sessions, queued turns, the durable safe runtime event log, Graph checkpoints, and tool invocation records under `StateDir` (default `.luneagent`). Team branches share the agent run's Graph checkpoint. Execution format 2 uses `executions-v2.db` for Graph format 10 checkpoints. Legacy checkpoint files are left untouched. An old state directory is rejected until explicitly upgraded offline (see [Offline execution storage upgrade](#offline-execution-storage-upgrade)). Enabled MCP-server state uses the same directory.

Open a session-bound runtime for asynchronous control:

```go
runtime, err := client.OpenSessionRuntime(ctx, session.ID)
if err != nil {
	log.Fatal(err)
}
defer runtime.Close(ctx)

events, err := runtime.SubscribeEvents(ctx, 0)
if err != nil {
	log.Fatal(err)
}
go func() {
	for event := range events {
		// Handle durable lifecycle events and best-effort live model deltas.
	}
}()

submission, err := runtime.SubmitTurn(ctx, core.TurnRequest{Input: input}, core.StartIfIdle)
if err != nil {
	log.Fatal(err)
}
// submission.ExecutionID can be passed to runtime.Interrupt and WaitTurn.
result, err := runtime.WaitTurn(ctx, submission.ExecutionID)
if err != nil {
	log.Fatal(err)
}
if result.Status == core.TaskCompleted {
	// result.Response is the raw model response.
}
```

### Event subscriptions and replay boundary

`runtime.SubscribeEvents(ctx, afterCursor)` replays durable events strictly after `afterCursor`, followed by live notifications and model stream events.

To distinguish between historical replay and subsequent real-time execution, use `runtime.SubscribeEventsWithReplayComplete(ctx, afterCursor)`. It emits a transient `EventRuntimeReplayComplete` once durable replay catches up, before delivering any new live events. The marker is purely transient and is never stored in the database.

## Submission modes and queue control

`SessionRuntime` provides three turn submission modes:

- `core.StartIfIdle`: Rejects work immediately with `core.ErrSessionBusy` if the session has an active running turn, queued turns, or a paused execution.
- `core.QueueInput`: Appends a durable FIFO turn to the session queue. Admitted work starts as soon as earlier turns complete.
- `core.SteerInput`: Injects an instruction into the active execution without waiting for it to finish. Core injects steering messages after the current tool-call round and before the next model request; if the model is already streaming its final answer, Core checks for steering before finalizing that answer. Emits `EventTaskInputInjected` containing `InputExecutionID` and `ParentExecutionID`. If the active turn has already passed its steering boundary, the submission becomes a normal queued turn.

### Queue inspection and manipulation

Hosts can inspect and modify queued turns without stopping the runtime:

- `runtime.CancelQueuedTurn(ctx, executionID)`: Removes one turn that has not yet started. Returns `core.ErrTurnNotFound` if the turn does not exist, or `core.ErrQueuedTurnStarted` if it has already been claimed by the worker.
- `runtime.CancelPendingTurns(ctx)`: Atomically cancels every unstarted turn in the session queue and interrupts the currently running execution. It waits for active cleanup to complete before returning, returning the slice of cancelled `ExecutionID`s.
- `runtime.ReplaceQueuedTurnInput(ctx, executionID, newInput)`: Replaces the prompt/input of an unstarted queued turn in-place, preserving its queue position and atomically cancelling any subsequent turns.

## Persistent Cross-Turn Goals

Core provides persistent, multi-turn user goals that remain active across repeated task executions until completed, paused, blocked, or cleared:

```go
// Create a persistent goal with an optional token budget.
budget := int64(100000)
goal, err := runtime.CreateGoal(ctx, core.GoalCreateOptions{
	Objective:              "Implement OAuth2 PKCE login flow and verify with tests",
	TokenBudget:            &budget,
	NoInteractiveDecisions: false,
})

// Query goal status and progress.
currentGoal, err := runtime.GetGoal(ctx)
```

### Goal lifecycle and statuses

A goal's lifecycle is represented by `core.GoalStatus`:

- `core.GoalActive`: The goal is active. As long as a goal is active, the runtime automatically enqueues continuation turns (`enqueueGoalContinuation`) once earlier turns finish, prompting the model to verify and complete the objective.
- `core.GoalPaused`: The goal is temporarily suspended by user request (`runtime.PauseGoal`) or when a task pauses. Automatic continuations cease.
- `core.GoalBlocked`: Progress cannot continue without user intervention (e.g. 3 consecutive turns without text output or tool progress, unhandled task failure, or explicit blocking).
- `core.GoalBudgetLimited`: Total uncached token usage has reached or exceeded `goal.TokenBudget`.
- `core.GoalComplete`: Objective verified and completed.

### Goal runtime APIs

`SessionRuntime` exposes full management methods for goals:

- `CreateGoal(ctx, options core.GoalCreateOptions) (*core.Goal, error)`: Creates or replaces the active session goal.
- `GetGoal(ctx) (*core.Goal, error)`: Fetches the current goal, status, token usage, and stop reason.
- `PauseGoal(ctx) (*core.Goal, error)`: Pauses an active goal and interrupts any running automatic continuation.
- `ResumeGoal(ctx, options ...core.GoalResumeOptions) (*core.Goal, error)`: Resumes a paused, blocked, or budget-limited goal, optionally updating its budget or interaction mode.
- `EditGoal(ctx, objective string) (*core.Goal, error)`: Updates the objective text without clearing token usage.
- `SetGoalBudget(ctx, budget *int64) (*core.Goal, error)`: Adjusts the token budget cap (`nil` removes the limit).
- `ClearGoal(ctx) error`: Discards the active goal and releases any paused user queue turns.

Changes to goals emit `core.EventGoalUpdated` and `core.EventGoalCleared`.

### Built-in goal tools

Core automatically registers two session-control tools for the model:

- `get_goal`: Available to any agent. Reads the current goal objective, status, uncached token usage, optional token budget, and stop reason.
- `update_goal`: Reserved for the **lead agent** only. Marks the goal `complete` (only after verifying the full objective) or `blocked` (with a required concrete explanation). Creation, pausing, editing, and budget changes remain reserved for the host and user.

## Clarifications and User Questions

The lead agent can pause execution to ask a batch of structured clarification questions using the built-in `ask_user` tool.

The question arguments follow:
```json
{
  "questions": [
    {
      "id": "target",
      "question": "Which backend database should be targeted?",
      "options": [
        {"label": "PostgreSQL", "description": "Production PostgreSQL cluster", "recommended": true},
        {"label": "SQLite", "description": "Local embedded file database"}
      ]
    }
  ]
}
```

Each question requires a unique nonempty ID and question text; an empty options array permits free-text input. At most one option per question may include `recommended: true`.

### Answering questions

1. `runtime.PendingRequests(ctx)` returns pending questions along with their `ExecutionID`, `RunID`, `InvocationID`, and `CallID`.
2. Hosts submit answers with:
   ```go
   err := runtime.ResolveUserQuestion(ctx, core.UserQuestionAnswer{
       ExecutionID:  question.ExecutionID,
       RunID:        question.RunID,
       InvocationID: question.InvocationID,
       CallID:       question.CallID,
       Answers: []core.UserQuestionResponse{
           {QuestionID: "target", Answer: "PostgreSQL"},
       },
   })
   ```
3. To save unsubmitted user choices, notes, or custom form input before final submission without resuming the model, call:
   ```go
   err := runtime.SaveUserQuestionDraft(ctx, wait, draft)
   // Or save an arbitrary opaque frontend payload:
   err := runtime.SaveUserQuestionDraftPayload(ctx, wait, rawJSONBytes)
   ```
   `UserQuestionDraft.RawPayload` retains frontend view-model state across restarts.

## Plan Mode and Plan Implementation

Core supports separating read-only architectural planning from plan execution:

1. During planning, the model outputs an approved plan wrapped in `<proposed_plan>...</proposed_plan>` (or `<proposed_plan>...<proposed_plan_end/>`).
2. When the turn completes, `TaskResult.ProposedPlan` contains `*core.ProposedPlan{Text: "..."}`.
3. The host presents the plan for review and executes it via `runtime.ImplementPlan`:

```go
implementation, err := runtime.ImplementPlan(ctx, result.ExecutionID, core.PlanImplementationOptions{
	Mode:           core.PlanImplementSameContext, // or core.PlanImplementNewContext
	PermissionMode: core.PermissionWorkspace,
	Authorization: core.PlanAuthorization{
		Paths:                    []tool.PathGrant{{Path: "/path/to/dir", Access: tool.PathReadWrite}},
		AllowNetwork:             true,
		AllowedTools:             []string{"read_file", "write_file", "edit_file", "command"},
		ApprovedHighRiskCommands: []string{"npm install"},
	},
})
```

- `PlanImplementSameContext`: Continues executing in the existing session context.
- `PlanImplementNewContext`: Forks execution into a fresh sibling session containing only the plan text as initial context.

### Execution blockers

During non-interactive plan execution or automated goals (`NoInteractiveDecisions = true`), interactive permission prompts and questions are suppressed. If an operation requires ungranted authorization, the task is immediately paused, emits `core.EventExecutionBlocked`, and surfaces a `core.PendingExecutionBlocker` request in `runtime.PendingRequests(ctx)`. Hosts should notify the user to re-plan rather than presenting an execution-time popup.

## Tool Permissions, Approvals, and Toolchains

Every tool belongs to a permission class:
- `workspace-read`: Local reading (e.g. `read_file`, `list_dir`, `glob`, `grep`, `read_image`).
- `workspace-write`: Modifying workspace files (e.g. `write_file`, `edit_file`).
- `command`: Running processes (e.g. `command`, `command_stop`).
- `network`: External HTTP/network access (e.g. `web_search`, `web_fetch`).
- `session-control`: Session lifecycle tools (e.g. `get_goal`, `update_goal`).

### Permission modes

Set via `Core.SetPermissionMode(mode)`:
- `read-only`: Reads workspace files; prompts before external directories, web, or unclassified tools; blocks writes and commands.
- `workspace`: Reads and writes workspace files; prompts before external filesystem paths, commands, web access, and unclassified tools.
- `auto`: Reads and writes workspace files; uses a structured model review (`AutoReviewApprove` / `AutoReviewAskUser`) to automatically evaluate commands and external operations, asking the user only when risky.
- `full-access`: Allows registered tools and current-user files without interactive prompts; narrow denylist for destructive commands still applies.

### Explicit approval decisions and session memory

When approval is required (`EventApprovalRequired`), resolve it with:
```go
err := runtime.ResolveApprovalDecision(ctx, request.ApprovalID, tool.ApprovalAllowOnce)
```
Supported decisions:
- `tool.ApprovalAllowOnce`: Allows this single invocation.
- `tool.ApprovalAllowSession`: Permanently approves the tool call for the remainder of the session. Persisted as a `SessionApproval` keyed by tool name, canonical normalized arguments, and tool invocation fingerprint.
- `tool.ApprovalDeny`: Rejects the invocation, returning an error to the model.

### External path grants

Filesystem access outside the workspace can be granted on a per-session basis via `core.SessionRuntime`:
- `tool.PathRead`: Read-only access to an external directory and its subdirectories.
- `tool.PathReadWrite`: Read and write access to the external directory tree.

### Command toolchain paths

To grant sandbox commands read and execute access to external toolchains (e.g. Go, Node.js, Rust binaries):
- Configure `core.Config.CommandToolchainPaths` at startup.
- Dynamically query or update paths via `client.CommandToolchainPaths()` and `client.SetCommandToolchainPaths(paths)`.

## Built-in Tools Reference

| Category | Tool | Parameters | Permission Class | Always Visible | Description |
| --- | --- | --- | --- | --- | --- |
| `files` | `read_file` | `path`, `start_line` (default 1), `limit` (default 200, max 500) | `workspace-read` | Yes | Read lines with 1-based numbering |
| `files` | `write_file` | `path`, `content` | `workspace-write` | Yes | Write entire file contents |
| `files` | `edit_file` | `path`, `old_text`, `new_text` | `workspace-write` | Yes | Exact text replacement |
| `files` | `read_image` | `path` | `workspace-read` | Yes | Visual input for PNG, JPEG, or WebP |
| `files` | `list_dir` | `path`, `limit` (max 500) | `workspace-read` | Yes | List directory entries |
| `files` | `glob` | `pattern` | `workspace-read` | Yes | Fast file glob matching |
| `files` | `grep` | `query`, `path` | `workspace-read` | Yes | Search file contents |
| `command` | `command` | `command`, `background` (bool), `yield_time_ms` (0-5000), `timeout_ms` | `command` | Yes | Run shell command in kernel sandbox |
| `command` | `command_status` | `command_id`, `cursor` (offset), `wait_ms` (0-5000) | `workspace-read` | Yes | Read incremental output buffer |
| `command` | `command_stop` | `command_id` | `command` | Yes | Terminate command process |
| `web` | `web_search` | `query`, `limit` | `network` | No | Search internet resources |
| `web` | `web_fetch` | `url` | `network` | No | Fetch webpage content |
| `memory` | `search_memory` | `query`, `limit` | `workspace-read` | No | Search long-term workspace memories |
| `team` | `delegate_team` | `mode` (`"task"` or `"fork"`), `teammates`, `minimum_successes` | `session-control` | No | Spawn worker agents in team graph |
| `goal` | `get_goal` | _(none)_ | `session-control` | Yes | Read current goal and token budget |
| `goal` | `update_goal` | `status` (`"complete"` or `"blocked"`), `reason` | `session-control` | Yes | Update persistent goal (lead agent only) |
| _lead_ | `ask_user` | `questions: [{id, question, options}]` | _(reserved)_ | Yes | Ask clarification questions |

## Model Request Recovery and Backoff

`ExecutionOptions.ModelMaxAttempts` limits attempts for each model request (default 3; 1 disables retries).

- Host providers should classify errors via `model.RequestError`. Unclassified errors, authentication failures, and quota exhaustion are not retried.
- Transient rate limits and server errors use exponential backoff (1–8 seconds plus up to 250 ms jitter), honoring `Retry-After`.
- When retries are exhausted or wait exceeds 30 seconds, the lead agent pauses with `TaskResult.ModelFailure` and a `PendingModelRecovery` request.
- Resume with `runtime.Recover(ctx)` or `client.ResumeTask`. If called before the backoff timer elapses, Core returns `core.ErrModelRetryNotReady`.

## Long-Running Commands and Sandbox

Build the companion sandbox binary:
```sh
go build -trimpath ./cmd/lune-sandbox
```
Place it beside the host executable, on `PATH`, or set `core.Config.SandboxHelperPath`. Helper protocol version 2 is required.
- Linux uses Landlock and seccomp. Go build, module, and XDG caches are placed in a per-workspace persistent cache directory.
- Windows workspace mode uses AppContainer; Windows full access drops privileges to a medium integrity token.
- `command` returns output up to 1 MiB. Poll output with `command_status` and stop with `command_stop`.
- Passing `background: true` keeps services running across turns. Background services are owned by the session and stop when the session is closed or explicitly stopped.
- Host inspection methods: `runtime.ListCommands(ctx)`, `runtime.ReadCommand(ctx, id, options)`, and `runtime.StopCommand(ctx, id)`.

## Model Context Protocol (MCP)

Core supports both standard stdio and remote streamable HTTP transports for MCP servers:

### Transport configuration

```json
"mcpServers": {
  "cli-tools": {
    "transport": "stdio",
    "command": "/usr/local/bin/tools-server",
    "args": ["--listen"],
    "env": {"DEBUG": "0"}
  },
  "remote-service": {
    "transport": "streamable-http",
    "url": "https://mcp.service.com/mcp",
    "clientMetadataURL": "https://service.com/oauth/metadata.json"
  }
}
```

### OAuth authentication and browser customization

When a streamable HTTP server requires OAuth authorization:
- `client.AuthenticateMCPServer(ctx, serverName)` initiates the authorization workflow.
- `core.Config.BrowserOpener` configures URL handling: defaults to `core.SystemBrowserOpener{}`, or can be set to `core.HeadlessBrowserOpener{}` or a custom `core.BrowserOpener`.
- `core.Config.CredentialStorage` configures OAuth token persistence: defaults to `core.KeyringStorage{}` (OS keyring), or `core.NewMemoryCredentialStorage()` for testing and headless usage, or a custom `core.CredentialStorage`.
- `client.OpenMCPURL(serverName, target)` opens elicitation URLs.

### Host-controlled prompts and resources

MCP prompts and resources are host-driven and never injected into model context automatically:
- `runtime.ListMCPPrompts`, `runtime.GetMCPPrompt`
- `runtime.ListMCPResources`, `runtime.ListMCPResourceTemplates`, `runtime.ReadMCPResource`
- `GetMCPPrompt` and `ReadMCPResource` return `*model.ResponseInput`, which hosts may pass to `core.NewTaskInput` if desired.

## Workspace Memory

Workspace memory is enabled by default. After each task, Core performs an auxiliary instructed model call to extract reusable facts, preferences, conventions, and decisions.

- Stored in SQLite alongside sessions. Deleting a session cleans up memories extracted from it; manually added memories persist.
- Manage memories via `client.ListMemories`, `client.SearchMemories`, `client.AddMemory`, `client.UpdateMemory`, `client.DeleteMemory`, `client.ClearMemories`.
- Hosts can provide semantic retrieval by implementing `memory.Embedder` and passing it to `core.Config.Memory.Embedder`. Call `client.ReindexMemories(ctx)` if embedder parameters change.
- Disable automatic memory extraction with `core.Config.Memory.Disabled = true`.

## Offline Execution Storage Upgrade

When upgrading state directories containing earlier execution formats:

```go
report, err := core.UpgradeState(ctx, stateDir)
```
Or via CLI: `lune migrate --state-dir <directory>`.

The upgrader takes an exclusive lock, backs up `sessions.db` via SQLite `VACUUM INTO` into an `upgrade-backup-*` folder, and upgrades checkpoints to execution format 2 / Graph format 10. History, settings, metadata, and recorded usage are retained. Old active goals are paused and require explicit resume.

If queue drain or SQLite persistence encounters a storage error during execution, the drain worker stops safely instead of triggering an infinite retry loop. Re-opening the session runtime or retrying `WaitTurn` triggers safe reconciliation.
