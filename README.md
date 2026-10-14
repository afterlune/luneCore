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

Core stores sessions, queued turns, the safe runtime event log, Graph checkpoints, and tool invocation records under `StateDir` (default `.luneagent`). Team branches share the agent run's Graph checkpoint. Graph format 10 checkpoints are stored in `checkpoints-v10.db`; older checkpoint files are left untouched and are not migrated. Enabled MCP-server state uses the same directory. Open a session-bound runtime for asynchronous control:

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
	// result.Response is the raw model response; result.AcceptanceReports
	// contains the criteria-by-criteria judgment and its source.
}
```

Every task prepares completion criteria from the current user turn before using business tools, then reviews the final candidate. `SessionSettings.Goal` is background guidance and does not add criteria. A plan is accepted for completeness; it is not required to have been implemented. `RunTask`, `ResumeTask`, and `WaitTurn` return the same `TaskResult`. Only an accepted result has `TaskCompleted` status.

Without a host verifier, the current model judges each criterion. Set `Config.AcceptanceVerifier` to make a host check authoritative. A verifier must return one result per criterion, honor context cancellation, and avoid changing business state. A verifier error, timeout, omitted criterion, or invalid result becomes `unknown`; it never falls back to a model pass.

An unresolved or failed acceptance pauses the turn and blocks the session queue. Read the detailed `PendingAcceptance` from `runtime.PendingRequests(ctx)`. `runtime.ResolveAcceptance` either grants two fresh correction attempts with `AcceptanceContinue`, or records an explicit host confirmation with `AcceptanceConfirm`. Generic `Recover` and `ResumeTask` cannot bypass this decision; `DiscardPaused` remains available. Criteria, candidate reports, and saved evidence are returned through `TaskResult` or `PendingAcceptance`. Durable acceptance events carry only status, source, and IDs.

Core reads workspace `AGENTS.md` as project guidance when a task starts. Built-in file tools load applicable nested `AGENTS.md` files for their target paths; when a new rule is found, the current batch of file calls is skipped so the model can reconsider it. Shell commands receive workspace-root guidance only. Instruction snapshots are saved with the execution and reused after recovery. These files cannot override the user's request, tool permissions, safety requirements, or acceptance criteria.

Hosts can pass `Config.PersonalInstructions` to provide user-level preferences shared by all sessions in that Core. Core does not discover personal instruction files; the host supplies their text at startup. New tasks snapshot it for recovery, while resumed tasks keep their saved text. Workspace `AGENTS.md` guidance takes precedence over personal preferences; neither changes acceptance criteria. Recreate Core to apply changed personal instructions to new tasks.

`StartIfIdle` rejects work if the session is running, queued, or paused. `QueueInput` appends a durable FIFO turn. Accepted work starts without an event subscriber, and opening a session runtime starts that session's persisted, unpaused queue. Event subscriptions are optional observers; they replay the durable lifecycle log when attached. The lead agent can pause for a clarification with optional choices. `runtime.PendingRequests(ctx)` returns the full pending question and its stable `RunID`, `InvocationID`, and `CallID`. Resolve it with `runtime.ResolveUserQuestion(ctx, core.UserQuestionAnswer{RunID: question.RunID, InvocationID: question.InvocationID, CallID: question.CallID, Answer: answer})`; the same turn resumes automatically. Identical answer retries are accepted, while a different answer for the same wait is rejected. The question and answer remain in the checkpoint/input receipt as needed for recovery, but the durable event projection omits question text, options, and answers. After a host restart, the same question and wait IDs are available through `PendingRequests`; answering resumes the original continuation rather than asking again. The submission context controls admission only; accepted work continues until completion, failure, interruption, or pause. Canceling a `WaitTurn` context stops only that wait. A paused result returns promptly. Use `Recover` for budget, tool, or cancellation pauses; acceptance pauses require an explicit `ResolveAcceptance` decision. After resuming, call `WaitTurn` again with the same execution ID. Results remain available until the session is deleted. A failed task may have no candidate response; acceptance details and reports are present when available. `RunTask` and `ResumeTask` are blocking wrappers. A task waiting for host input or acceptance returns `ErrTaskPaused`; use `SessionRuntime` to resolve the request and continue it. The durable event log omits model deltas, tool arguments, approval arguments, interaction payloads, clarification text/options, acceptance criteria/evidence bodies, and free-form errors. If approval or an enabled MCP interaction is needed without an event subscriber, the request remains pending until the host resolves it; full requests are available through `runtime.PendingRequests(ctx)` while the Core instance is alive. MCP interaction still requires `runtime.SetUserInteractionAvailable(true)`.

Each `StateDir` is exclusively owned by one Core instance at a time, including across process boundaries. `New` returns an error matching `core.ErrStateDirInUse` if another Core already holds the directory lock. The lock is released by `Close` and automatically by the operating system when the process exits.

## Host extensions

Hosts can provide a model catalog through `core.Config.ModelConfig`, custom implementations through `core.Config.ModelProviders`, and custom tools through `core.Config.Tools`. These are programmatic equivalents of the corresponding `config.json` model fields; explicit Go entries override file entries with the same provider or profile name, and the file supplies entries not given in Go. A programmatic model config can be used without a `config.json` file.

Set `core.Config.Logger` to route Core-owned structured diagnostics to a host `*slog.Logger`. If omitted, Core captures `slog.Default()` when `core.New` is called and does not replace the process default. Host-provided providers, tools, and command executors remain responsible for their own logs.

Workspace memory is enabled by default. After an accepted task, Core makes a separate model call to extract a few reusable preferences, decisions, conventions, or verified facts. Future tasks receive a short untrusted memory summary and can call the read-only `search_memory` tool for details. Memories are scoped to the workspace and stored with the session database. Deleting a source session also deletes memories extracted from it; manually added memories remain. Use `ListMemories`, `SearchMemories`, `AddMemory`, `UpdateMemory`, `DeleteMemory`, and `ClearMemories` to manage them. Set `Config.Memory.Disabled` to opt out of automatic memory use and extraction.

Hosts may provide an `Embedder` through `Config.Memory.Embedder` for semantic retrieval. Core stores the returned vectors and combines vector and SQLite full-text rankings; if embedding fails, full-text search remains available. Keep `Embedder.ID()` stable for one model and vector format, and call `ReindexMemories` after changing it.

```go
import (
	"context"
	"log"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/memory"
)

var myEmbedder memory.Embedder // supplied by the host application

func main() {
	ctx := context.Background()
	client, err := core.New(core.Config{
		Workspace: ".",
		Memory:    core.MemoryConfig{Embedder: myEmbedder},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	_, err = client.AddMemory(ctx, memory.Input{
		Category:   memory.CategoryPreference,
		Content:    "Prefers concise explanations.",
		Importance: 3,
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

`myEmbedder` implements `memory.Embedder` from `github.com/afterlune/luneCore/memory`. Embedding services and their credentials remain host-owned; Core persists only the vectors and embedder ID.

Implement model providers with the public `model.Provider` interface and register tools with the public `tool.Tool` interface. Provider methods receive normalized request, response, and stream types; the runtime owns history, tool execution, and checkpointing. Provider implementations must support concurrent calls from different sessions. Every registered tool must declare a category, permission class, and JSON parameter schema. The built-in lead-agent tool name `ask_user` is reserved.

The `delegate_team` tool runs member agents as `luneGraph` fan-out branches in the same run. By default, results join in assignment order after every member finishes. Set `Config.Execution.TeamQuorum` for a host default, or pass `minimum_successes` for one delegation; `null` inherits the host default and `0` waits for every member. Reaching the threshold cancels unfinished branches, and their results are marked incomplete for the lead. `maxParallelAgents` bounds branch concurrency.

`Config.Execution` sets host defaults for `MaxSteps`, `MaxInvocations`, `MaxConcurrency`, `ModelTimeout`, `ToolTimeout`, `JoinTimeout`, and `TeamEntryTimeout`; `TaskRequest.Execution` and `TurnRequest.Execution` can override non-zero values for one task. Acceptance model calls count toward usage and graph execution budgets. `Config.MaxConcurrentCallbacks` optionally caps callback work across all sessions. `TeamEntryTimeout` only bounds a worker's first model callback. A budget pause reports `TaskResult.BudgetReason`; pass replacement execution options to `SessionRuntime.Recover` or `ResumeTask` to continue with a larger budget.

Commands run through the `lune-sandbox` companion executable. Windows workspace mode uses AppContainer; Windows full access uses a restricted current-user token with privileges removed; Linux uses Landlock and seccomp. Build the helper with `go build ./cmd/lune-sandbox` and place it beside the host executable, on `PATH`, or set `Config.SandboxHelperPath`. If the helper or kernel feature is unavailable, commands fail closed. `Config.CommandExecutor` remains an explicit trusted host override and must enforce `CommandRequest.Sandbox` itself. `AllowLocalCommand` is deprecated and no longer enables an unsandboxed fallback.

Workspace mode allows sandboxed commands in the workspace; network and extra command paths must be approved in `PlanAuthorization`. Full access allows the broadest current-user resources the host OS can expose while dropping elevation: Linux keeps normal filesystem permissions and refuses full access when Core runs as root; Windows removes token privileges, disables the Administrators group, and lowers elevated tokens to medium integrity. Common destructive and opaque command forms require an exact entry in `ApprovedHighRiskCommands`. `SessionRuntime.ImplementPlan` accepts the host-confirmed authorization snapshot and disables execution-time questions; a permission blocker pauses the turn for re-planning instead of opening an execution-time approval popup.

## Concurrent sessions and tool recovery

A Core can run tasks for different sessions concurrently. A session runtime serializes turns within its session. Use `runtime.State` to inspect the active task and durable queue; `ActiveTasks` reports currently running tasks across the Core.

Call `runtime.Interrupt(executionID)` to interrupt a running turn or remove it before it starts. Cancellation is cooperative, so host providers and tools should honor their context. A turn with a saved checkpoint remains paused and blocks later queue entries until `runtime.Recover` or `runtime.DiscardPaused`. `Close` waits for queued work to finish starting, and returns an error while a task is running.

If the process stops while a tool may have caused a side effect, recovery exposes a `PendingToolRecovery` request through `runtime.PendingRequests`. Choose `ToolRecoveryRetry` or `ToolRecoverySkip` with `runtime.ResolveToolRecovery`, then call `runtime.Recover`. Completed tool results are replayed from the invocation ledger without running the tool again. Recovery covers parent and team-worker calls. `runtime.SetUserInteractionAvailable(true)` enables MCP user interaction for that session while a runtime event subscriber is attached.

Use `Core.ForkPausedTask` to clone a paused checkpoint into a new session. The clone stays paused; recover it independently with its new session runtime. Its tool and user-answer receipts are copied under the new run identity so replay does not repeat completed side effects.

Disconnected MCP stdio calls are not automatically replayed. Reconnection refreshes the tools for future calls. Canceling one request leaves the shared MCP server running; closing a stdio transport allows 1.5 seconds for graceful exit before terminating the process tree, with a total shutdown wait of at most 3 seconds.

Task concurrency is coordinated within each Core instance. The `StateDir` lock prevents another Core instance from opening the same directory concurrently.

Public consumers should import the root `core` package plus `model` and `tool` only when implementing host extensions. The `internal` packages remain implementation details.

### Model request recovery

`ExecutionOptions.ModelMaxAttempts` limits attempts for each model request, including
its first attempt. Zero defaults to three; one disables automatic retry. Host
providers should disable their own retries and return `model.RequestError` for
classified failures. Unclassified errors, invalid requests, authentication failures
and quota exhaustion are not automatically retried.

Transient rate limits, service failures, transport failures and timeouts use
exponential backoff (1–8 seconds plus up to 250 ms of jitter), honoring
`Retry-After`. Requests and waits share the model node deadline. A wait exceeding
30 seconds or the remaining deadline requires explicit recovery. Once a stream
event is delivered, a later failure also requires recovery; partial tool calls
are never executed.

The lead agent pauses with `TaskResult.ModelFailure` and a `PendingModelRecovery`
request. Check `runtime.PendingModelRecovery(ctx)` for the saved wait and
`Failure.RetryNotBefore`, then call `runtime.Recover(ctx)` or `Core.ResumeTask`.
Early recovery returns `ErrModelRetryNotReady` without queueing a request. Recovery
starts a fresh attempt cycle with the same ExecutionID, permissions, checkpoint
and completed tool receipts, and a new TaskID for stream consumers. A paused turn
blocks the session queue until recovered or discarded with `DiscardPaused`.

Worker failures remain branch failures. An acceptance model failure preserves the
host verifier's authority when one is configured; otherwise it pauses for model
recovery without spending acceptance correction attempts. Memory extraction
failures leave accepted tasks completed. Manual compaction retries and returns an
error when attempts are exhausted.

`model_retry`, `model_recovery_required` and `model_recovery_resumed` events contain
safe failure metadata, never provider error bodies or causes. Task checkpoints
use settings version 9; older execution checkpoints must be discarded. Existing
session histories and completed results remain readable.

### Long-running commands

Rebuild `lune-sandbox` alongside Core. Helper protocol version 1 is required;
older binaries fail closed rather than running without process supervision.

`command` runs non-interactive commands in the existing kernel sandbox. It waits
up to one second by default, then returns JSON containing the command ID, state,
and bounded output. Use `command_status` with `command_id`, a byte `cursor`, and
optional `wait_ms` (0–5000) to read more output; use `command_stop` to stop it.
`yield_time_ms` (0–5000) controls the initial wait. An optional `timeout_ms` can
shorten the host limit. A start receipt does not prove successful completion.

`Config.Commands.Timeout` defaults to ten minutes, and
`Config.Commands.MaxConcurrent` defaults to four running commands across Core.
Commands belong to the current execution and agent. Model tools cannot inspect
another agent's commands; controlling an already-authorized command requires no
additional approval. Existing high-risk command approval and sandbox policies
apply to every new launch. Windows workspace commands use a PowerShell filesystem
drive rooted at the workspace; native programs use its actual directory. Commands
continue across model requests, but stop when
the task pauses, ends, is interrupted, or is discarded. Team branches also clean
up their processes when they finish or are cancelled by an early join.

If an agent submits a final answer while its commands still run, Core first
reminds it to inspect or stop them. A second consecutive submission pauses the
lead and stops its commands; `Recover` or `ResumeTask` can then continue. Workers
instead report a branch failure. Command queries become saved tool evidence. The acceptance handoff also snapshots
terminal command records, with their actual command, exit code and a bounded
output excerpt, under `command:<id>` evidence references;
acceptance still judges whether that evidence satisfies the task criteria.

Hosts use `SessionRuntime.ListCommands(ctx)`,
`ReadCommand(ctx, id, core.CommandReadOptions{Cursor: cursor, Wait: wait})`, and
`StopCommand(ctx, id)`. Read cancellation cancels only the wait. Stop is
idempotent and does not relaunch the process. Exit codes are optional for host
executors that cannot supply them. Status distinguishes success, failure,
cancellation, timeout and interruption across a restart.

Each command keeps its latest 1 MiB of output. A read returns at most 64 KiB;
`Cursor`, `NextCursor` and `EndCursor` are absolute byte offsets, and `Truncated`
reports output that has fallen out of the retained buffer. Snapshots are saved
before reads return and at termination. Unread live output is not guaranteed to
survive a crash. Restart marks unfinished records interrupted without rerunning
commands. Forking a paused task copies saved command records under the new
session/execution identity, without copying processes. Deleting a session removes
its records.

The optional `tool.CommandRequest.OnOutput` callback lets host command executors
provide incremental stdout/stderr; its byte slice is transient, errors require
stopping, and the callback is excluded from JSON. Old executors still work by
returning `CommandResult.Output` at completion. Executors must honor cancellation
and terminate their child processes before returning. Command lifecycle events
contain identifiers, status and exit codes, never command text or output.
