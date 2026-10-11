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

Core stores sessions, checkpoints, team runs, and tool invocation records under `StateDir` (default `.luneagent`). Enabled MCP-server state uses the same directory. Front ends receive runtime events through `SetEventHandler` and resolve tool approvals or user interactions through the matching Core methods.

## Host extensions

Hosts can provide a model catalog through `core.Config.ModelConfig`, custom implementations through `core.Config.ModelProviders`, and custom tools through `core.Config.Tools`. These are programmatic equivalents of the corresponding `config.json` model fields; explicit Go entries override file entries with the same provider or profile name, and the file supplies entries not given in Go. A programmatic model config can be used without a `config.json` file.

Implement model providers with the public `model.Provider` interface and register tools with the public `tool.Tool` interface. Provider methods receive normalized request, response, and stream types; the runtime owns history, tool execution, and checkpointing. Provider implementations must support concurrent calls from different sessions. Every registered tool must declare a category, permission class, and JSON parameter schema.

Hosts can set `core.Config.CommandExecutor` to run commands through their own sandbox or process manager. Direct local shell execution is disabled by default; set `AllowLocalCommand` only when the host intentionally permits it. The runtime still applies the selected permission mode and approval handler.

## Concurrent sessions and tool recovery

A Core can run tasks for different sessions concurrently. It rejects a second task for a session that is already running. Use `StateForSession` for one session and `ActiveTasks` for the set of currently running tasks; `State` remains as a compatibility view of the most recently changed task.

If the process stops while a tool may have caused a side effect, `ResumeTask` pauses and emits `EventToolRecoveryRequired`. The host can inspect one or more requests with `PendingToolRecovery` or `PendingToolRecoveries`, choose `ToolRecoveryRetry` or `ToolRecoverySkip` with `ResolveToolRecovery`, then call `ResumeTask` again. Completed tool results are replayed from the invocation ledger without running the tool again. Recovery covers parent and team-worker calls.

Use one Core process for a given `StateDir`; task concurrency is coordinated within each Core instance.

Public consumers should import the root `core` package plus `model` and `tool` only when implementing host extensions. The `internal` packages remain implementation details.
