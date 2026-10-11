# Lune Core

Lune Core is the Go runtime library used by the Lune front ends. Applications construct `*core.Core` in their own process and use the public API for sessions, tasks, approvals, streaming events, usage, and tools. It does not run a daemon or expose an IPC/network service.

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
	client, err := core.New(core.Config{Workspace: "."})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

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

The workspace may supply `config.json` for model profiles and project runtime settings. Its model section has this shape:

```json
{
  "providers": {
    "primary": {
      "protocol": "responses",
      "baseURL": "https://api.example.com",
      "apiKey": "your-api-key"
    }
  },
  "models": {
    "main": { "provider": "primary", "model": "model-id" }
  },
  "defaultModel": "main"
}
```

Core stores sessions, queued turns, the safe runtime event log, checkpoints, team runs, and tool invocation records under `StateDir` (default `.luneagent`). Enabled MCP-server state uses the same directory. Open a session-bound runtime for asynchronous control:

```go
runtime, err := client.OpenSessionRuntime(ctx, session.ID)
if err != nil {
	log.Fatal(err)
}
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
	// result.Response is the same raw JSON response returned by RunTask.
}
```

`StartIfIdle` rejects work if the session is running, queued, or paused. `QueueInput` appends a durable FIFO turn. Accepted work starts without an event subscriber, and opening a session runtime starts that session's persisted, unpaused queue. Event subscriptions are optional observers; they replay the durable lifecycle log when attached. The lead agent can ask the host a clarification question with optional choices. Resolve it with `runtime.ResolveUserQuestion(id, answer)`; the same turn then continues. The live pending request is available through `runtime.PendingRequests(ctx)`. Its question and options are omitted from the durable event projection, but remain in the existing task checkpoint as part of the model tool call. After interruption or host-process restart, explicitly call `Recover`; the call is replayed and creates a fresh question request. When handling replayed question events, refresh `PendingRequests` and ignore event IDs that are no longer pending. The submission context controls admission only; accepted work continues until completion, failure, interruption, or pause. Canceling a `WaitTurn` context stops only that wait. A paused result returns promptly; after `Recover`, call `WaitTurn` again with the same execution ID. Results remain available until the session is deleted; failed results contain status only. `RunTask` and `ResumeTask` remain blocking wrappers over this runtime path. The durable event log omits model deltas, tool arguments, approval arguments, interaction payloads, clarification text/options, and free-form errors. If approval or an enabled MCP interaction is needed without an event subscriber, the request remains pending until the host resolves it; full requests are available through `runtime.PendingRequests(ctx)` while the Core instance is alive. MCP interaction still requires `runtime.SetUserInteractionAvailable(true)`.

Each `StateDir` is exclusively owned by one Core instance at a time, including across process boundaries. `New` returns an error matching `core.ErrStateDirInUse` if another Core already holds the directory lock. The lock is released by `Close` and automatically by the operating system when the process exits.

## Host extensions

Hosts can provide a model catalog through `core.Config.ModelConfig`, custom implementations through `core.Config.ModelProviders`, and custom tools through `core.Config.Tools`. These are programmatic equivalents of the corresponding `config.json` model fields; explicit Go entries override file entries with the same provider or profile name, and the file supplies entries not given in Go. A programmatic model config can be used without a `config.json` file.

Implement model providers with the public `model.Provider` interface and register tools with the public `tool.Tool` interface. Provider methods receive normalized request, response, and stream types; the runtime owns history, tool execution, and checkpointing. Provider implementations must support concurrent calls from different sessions. Every registered tool must declare a category, permission class, and JSON parameter schema. The built-in lead-agent tool name `ask_user` is reserved.

Hosts can set `core.Config.CommandExecutor` to run commands through their own sandbox or process manager. Direct local shell execution is disabled by default; set `AllowLocalCommand` only when the host intentionally permits it. The runtime applies the selected permission mode and surfaces approval requests through `SessionRuntime`.

## Concurrent sessions and tool recovery

A Core can run tasks for different sessions concurrently. A session runtime serializes turns within its session. Use `runtime.State` to inspect the active task and durable queue; `ActiveTasks` reports currently running tasks across the Core.

Call `runtime.Interrupt(executionID)` to interrupt a running turn or remove it before it starts. Cancellation is cooperative, so host providers and tools should honor their context. A turn with a saved checkpoint remains paused and blocks later queue entries until `runtime.Recover` or `runtime.DiscardPaused`. `Close` waits for queued work to finish starting, and returns an error while a task is running.

If the process stops while a tool may have caused a side effect, recovery exposes a `PendingToolRecovery` request through `runtime.PendingRequests`. Choose `ToolRecoveryRetry` or `ToolRecoverySkip` with `runtime.ResolveToolRecovery`, then call `runtime.Recover`. Completed tool results are replayed from the invocation ledger without running the tool again. Recovery covers parent and team-worker calls. `runtime.SetUserInteractionAvailable(true)` enables MCP user interaction for that session while a runtime event subscriber is attached.

Task concurrency is coordinated within each Core instance. The `StateDir` lock prevents another Core instance from opening the same directory concurrently.

Public consumers should import the root `core` package plus `model` and `tool` only when implementing host extensions. The `internal` packages remain implementation details.
