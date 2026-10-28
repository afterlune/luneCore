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

The workspace may supply `config.json` for model profiles and project runtime settings. Provider credentials and profiles live under `models`; execution defaults live under `runtime`:

```json
{
  "models": {
    "providers": {
      "primary": {
        "protocol": "responses",
        "baseURL": "https://api.example.com",
        "apiKey": "your-api-key"
      }
    },
    "profiles": {
      "main": {
        "provider": "primary",
        "model": "model-id",
        "contextWindow": 1000000
      }
    },
    "defaultProfile": "main"
  },
  "runtime": {
    "contextWindow": 0,
    "maxParallelAgents": 4,
    "maxTeamChildrenPerAgent": 0,
    "maxTeamDepth": 2,
    "autoCompact": true,
    "autoCompactThreshold": 0.8,
    "reserveOutputTokens": 0
  },
  "attachments": {
    "maxFiles": 0,
    "maxFileBytes": 0,
    "maxTotalBytes": 0
  },
  "mcpServers": {}
}
```

This grouped shape is required. Core rejects the previous flat config format and points to `config.example.json`; rewrite an existing file manually before upgrading.

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
	// result.Response is the raw model response.
}
```

MCP prompts and resources are host-controlled: use `runtime.ListMCPPrompts`, `GetMCPPrompt`, `ListMCPResources`, `ListMCPResourceTemplates`, and `ReadMCPResource` to fetch them explicitly. `GetMCPPrompt` and `ReadMCPResource` return `model.ResponseInput`; pass it to `core.NewTaskInput` and submit it only when appropriate. These calls do not add model tools or inject content into a task. MCP servers that request user input use the same `PendingRequests` and `ResolveUserInteraction` flow as tool calls. An in-flight host request canceled by server disable, reconnect, or Core shutdown matches `core.ErrMCPServerLifecycleChanged`; caller-context cancellation remains `context.Canceled`.

`go test ./...` runs a live DeepSeek MCP-input smoke test when `DEEPSEEK_API_KEY` is set. It makes one request each to the Responses and Anthropic-compatible APIs using the same model ID; `DEEPSEEK_MODEL` optionally selects that ID (default `deepseek-flash`).

`RunTask`, `ResumeTask`, and `WaitTurn` return the same `TaskResult`. A normal final response completes the task. When needed, the lead agent can ask a concise clarification question; the host resolves it through the pending-request API and the same turn resumes.

Core reads workspace `AGENTS.md` as project guidance when a task starts. Built-in file tools load applicable nested `AGENTS.md` files for their target paths; when a new rule is found, the current batch of file calls is skipped so the model can reconsider it. Shell commands receive workspace-root guidance only. Instruction snapshots are saved with the execution and reused after recovery. These files cannot override the user's request, tool permissions, or safety requirements.

Hosts can pass `Config.PersonalInstructions` to provide user-level preferences shared by all sessions in that Core. Core does not discover personal instruction files; the host supplies their text at startup. New tasks snapshot it for recovery, while resumed tasks keep their saved text. Workspace `AGENTS.md` guidance takes precedence over personal preferences; neither expands the user's request. Recreate Core to apply changed personal instructions to new tasks.

`StartIfIdle` rejects work if the session is running, queued, or paused. `QueueInput` appends a durable FIFO turn. Admitted work starts without an event subscriber, and opening a session runtime starts that session's persisted, unpaused queue. Event subscriptions are optional observers; they replay the durable lifecycle log when attached. The lead agent can pause for a batch of clarifications with optional choices. The `ask_user` arguments are `{"questions":[{"id":"target","question":"Which target?","options":[{"label":"Local","description":"Use the workspace"}]}]}`. Each question requires a unique nonempty ID and question text; an empty options array permits a free-text answer. `runtime.PendingRequests(ctx)` returns the full pending question and its `ExecutionID` and stable `RunID`, `InvocationID`, and `CallID`. Resolve it with `runtime.ResolveUserQuestion(ctx, core.UserQuestionAnswer{ExecutionID: question.ExecutionID, RunID: question.RunID, InvocationID: question.InvocationID, CallID: question.CallID, Answers: answers})`; where `answers` is an ordered `[]core.UserQuestionResponse` containing one nonempty answer per question ID; the same turn resumes automatically only after the entire batch is complete. Use `runtime.SaveUserQuestionDraft(ctx, wait, draft)` to save unsubmitted choices, notes, custom answers and the current question without resuming the model. `PendingRequests` restores this draft after restart; completion or discarding the wait clears it. Identical answer retries are accepted, while a different answer for the same wait is rejected. The question and answer remain in the checkpoint/input receipt as needed for recovery, but the durable event projection omits question text, options, and answers. After a host restart, the same question and wait IDs are available through `PendingRequests`; answering resumes the original continuation rather than asking again. The submission context controls admission only; admitted work continues until completion, failure, interruption, or pause. Canceling a `WaitTurn` context stops only that wait. A paused result returns promptly. Use `Recover` for budget, tool, or cancellation pauses. After resuming, call `WaitTurn` again with the same execution ID. Results remain available until the session is deleted. A failed task may have no response. `RunTask` and `ResumeTask` are blocking wrappers. A task waiting for host input returns `ErrTaskPaused`; use `SessionRuntime` to resolve the request and continue it. The durable event log omits model deltas, tool arguments, approval arguments, interaction payloads, clarification text/options, and free-form errors. If approval or an enabled MCP interaction is needed without an event subscriber, the request remains pending until the host resolves it; full requests are available through `runtime.PendingRequests(ctx)` while the Core instance is alive. MCP elicitation from lead and team agents uses the same host interaction flow. Both `EventUserInteractionRequired.AgentID` and `Interaction.AgentID` identify the requester; resolve pending requests with `runtime.ResolveUserInteraction`. The full request remains process-local and requires `runtime.SetUserInteractionAvailable(true)`.

`SteerInput` is a separate submission mode for hosts that want a follow-up to continue the active execution. Core injects accepted messages after the current tool-call round and before the next model request; when the model is already returning a final answer, Core checks for steering before finalizing that answer. `EventTaskInputInjected` identifies both the active parent execution and the consumed input receipt. The input remains cancelable while queued and is consumed only when Core acknowledges it for injection. If the active execution has already closed its steering boundary, the submission remains a normal queued turn. `QueueInput` retains its independent FIFO-turn behavior.

Multiple Core instances may share a `StateDir` and its SQLite stores. Each opened session is exclusively owned across process boundaries: opening the same session in another Core returns an error matching `core.ErrSessionInUse`. Close a `SessionRuntime` when leaving a session; `Core.Close` releases any remaining session locks.

Question options may include `recommended: true` on at most one option per question. Clients should display that option as the suggested default and may preselect it, while submitting the original `label` unchanged. A question with no recommendation remains valid.

## Host extensions

Hosts can provide a model catalog through `core.Config.ModelConfig`, custom implementations through `core.Config.ModelProviders`, and custom tools through `core.Config.Tools`. These are programmatic equivalents of the corresponding `config.json` model fields; explicit Go entries override file entries with the same provider or profile name, and the file supplies entries not given in Go. A programmatic model config can be used without a `config.json` file.

Hosts that own project settings storage can instead pass a complete `core.ProjectConfig` through `core.Config.ProjectConfig`. When this field is non-nil, Core does not read the workspace `config.json`; the supplied value includes the model catalog, context and attachment limits, auto-compaction options, and MCP server definitions. Start with `core.DefaultProjectConfig()` to apply Core's defaults, then set `ModelConfig` and the project settings. Attachment limits use `core.ProjectAttachmentLimits`. When `ProjectConfig` is nil, Core keeps its `config.json` behavior for hosts such as the TUI and CLI. Core does not persist the host-supplied configuration.

Set `core.Config.Logger` to route Core-owned structured diagnostics to a host `*slog.Logger`. If omitted, Core captures `slog.Default()` when `core.New` is called and does not replace the process default. Host-provided providers, tools, and command executors remain responsible for their own logs.

Workspace memory is enabled by default. After a task, Core makes a separate model call to extract a few reusable preferences, decisions, conventions, or verified facts. Future tasks receive a small summary of important preferences and locally matched memories, and can call the read-only `search_memory` tool for details. Memories are scoped to the workspace and stored with the session database. Deleting a source session also deletes memories extracted from it; manually added memories remain. Use `ListMemories`, `SearchMemories`, `AddMemory`, `UpdateMemory`, `DeleteMemory`, and `ClearMemories` to manage them. Set `Config.Memory.Disabled` to opt out of automatic memory use and extraction.

Extraction uses an instructed tool submission without forced tool choice, supporting reasoning models that reject `required`. Strict schema and local evidence validation still apply. A `memory_failed` event retains only the failure phase and safe model error metadata; logs omit provider bodies and memory contents. Extraction failure leaves the main task completed.

To explicitly run the live memory regression, set `LUNE_MEMORY_SMOKE_CONFIG` to a private luneAgent config path and run `go test -trimpath ./internal/runtime -run '^TestMemoryRealAPISmoke$' -count=1 -v`. It uses a temporary workspace and database, reads a file through a tool, and checks that an explicit preference is saved. It allows at most three HTTP requests, disables model retries, and sets a 10,000-token budget. Normal test runs skip this API test.

Each `UsageRecord` includes a `Purpose` (`main`, `compaction`, `memory`, or `team`) so hosts can see which model stages consume tokens. Provider-measured totals are authoritative; estimated input remains separate. Cached token counts are reported separately from total input where the protocol defines them as a subset.

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

The `delegate_team` tool runs member agents as `luneGraph` fan-out branches in the same run. Delegation is disabled by default. Set a positive `runtime.maxTeamChildrenPerAgent` in workspace `config.json`, `Config.Execution`, or `TaskRequest.Execution`/`TurnRequest.Execution` to enable it; each agent's child limit is cumulative across repeated delegations. Set `runtime.maxTeamDepth` to cap recursion (root is depth zero; the default is two). Task-level zero inherits the host default. Precedence is task override, host execution defaults, workspace config, then built-in defaults. `runtime.maxParallelAgents` and `MaxConcurrency` only control scheduling concurrency. By default, results join in assignment order after every member finishes. Set `Config.Execution.TeamQuorum` for a host default, or pass `minimum_successes` for one delegation; `null` inherits the host default and `0` waits for every member. Reaching the threshold cancels unfinished branches, and their results are marked incomplete for the lead. Model profiles may set `contextWindow`; session/task/project context settings take precedence for the lead, while a worker's selected profile window takes precedence for that worker and an unset profile window inherits the effective task context.

Each `delegate_team` call may choose `mode: "task"` or `mode: "fork"`. `task` is the default and gives each member only its assigned task. `fork` copies the input sent to the parent model before its current response, including completed tool results, then appends the member's task; the pending assistant response and its delegation call are excluded. Only members with the same provider and model inherit the forked context. Other model profiles receive task-only context; their prompt cache remains isolated. Prompt-cache hints are scoped by dispatch, provider, and actual model: OpenAI Responses receives a cache key, Anthropic Messages receives a breakpoint at the shared prefix, and DeepSeek uses its automatic cache without an unsupported key. Providers decide whether a cache hit occurs.

`Config.Execution` sets host defaults for `MaxSteps`, `MaxInvocations`, `TokenBudget`, `MaxConcurrency`, `ModelTimeout`, `ToolTimeout`, `JoinTimeout`, `TeamEntryTimeout`, `MaxTeamChildrenPerAgent`, and `MaxTeamDepth`; `TaskRequest.Execution` and `TurnRequest.Execution` can override non-zero values for one task. A zero task override inherits the host default; an effective `TokenBudget` of zero is unlimited. `TokenBudget` caps combined input and output tokens across the lead agent, team members, retries, and memory extraction. Requests reserve estimated input plus output capacity before sending; provider usage reconciles the reservation, while missing usage or an uncertain request outcome keeps the full reservation charged. Input estimates can be inaccurate, so reported overrun stops later requests but cannot undo an in-flight response. A token budget pause reports `BudgetMaxTokens`; pass replacement execution options with a higher absolute `TokenBudget` to `SessionRuntime.Recover` or `ResumeTask` to continue under the same execution ID and prior usage. `Config.MaxConcurrentCallbacks` optionally caps callback work across all sessions. `TeamEntryTimeout` only bounds a worker's first model callback. Other budget pauses report their limit through `TaskResult.BudgetReason`.

Commands run through the `lune-sandbox` companion executable. Windows workspace mode uses AppContainer; Windows full access uses a restricted current-user token with privileges removed; Linux uses Landlock and seccomp. Build the helper with `go build -trimpath ./cmd/lune-sandbox` and place it beside the host executable, on `PATH`, or set `Config.SandboxHelperPath`. If the helper or kernel feature is unavailable, commands fail closed. `Config.CommandExecutor` remains an explicit trusted host override and must enforce `CommandRequest.Sandbox` itself. `AllowLocalCommand` is deprecated and no longer enables an unsandboxed fallback.

On Linux, the built-in sandbox keeps Go build, module, and XDG caches in a per-workspace directory under the current user's cache root. `TMPDIR` and `GOTMPDIR` remain command-scoped; the helper creates the Go temporary directory before starting the shell. Host-shell exports are not inherited by sandbox commands; set command-specific environment variables in that invocation and configure extra binary directories through `CommandToolchainPaths`.

Permission modes apply to every registered tool: `read-only` permits workspace reads and prompts for external-directory reads or web access, while blocking writes and commands; `workspace` permits workspace reads and writes while prompting for external filesystem paths, commands, and web access; `auto` adds single-invocation model review to workspace permissions; `full-access` permits registered tools without prompts. External paths used by built-in file tools can be granted per session, for a directory and its descendants, as `read` or `read_write` (`read_write` also allows reads). Grants survive reopening that session, are not inherited by new or forked sessions, and are removed with the session. They do not change command sandbox grants. Full access exposes the broadest current-user resources the host OS can provide while dropping elevation: Linux keeps normal filesystem permissions and supports current-user root access in containers; Windows removes token privileges, disables the Administrators group, and lowers elevated tokens to medium integrity.

The built-in `read_image` tool sends one PNG, JPEG, or WebP image to the model as multimodal tool output. It uses the lower of the configured per-file and total attachment-size limits.

Interactive command approvals can permit ordinary high-risk commands. A narrow denylist blocks known spellings of destructive disk/partition operations, boot and security control destruction, privilege elevation, and recursive deletion of critical OS directories in every mode. This is an additional safeguard, not a complete shell security policy: expansion or arbitrary programs can evade string matching. OS sandbox capabilities and explicit authorization define the isolation boundary. Non-interactive plan execution still requires exact entries in `ApprovedHighRiskCommands`, plus the relevant `PlanAuthorization` capabilities. `SessionRuntime.ImplementPlan` accepts the host-confirmed authorization snapshot and disables execution-time questions; a permission blocker pauses the turn for re-planning instead of opening an execution-time approval popup.

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

Worker failures remain branch failures. Model request failures pause for explicit
recovery. Memory extraction failures leave the completed response intact. Manual
compaction retries and returns an error when attempts are exhausted.

`model_retry`, `model_recovery_required` and `model_recovery_resumed` events contain
safe failure metadata, never provider error bodies or causes. Task checkpoints
use settings version 10; older execution checkpoints must be discarded. Existing
session histories and completed results remain readable.

### Long-running commands

Rebuild `lune-sandbox` alongside Core. Helper protocol version 2 is required;
older binaries fail closed rather than running without process supervision.

`command` runs non-interactive commands in the existing kernel sandbox. It waits
up to one second by default, then returns JSON containing the command ID, state,
and bounded output. Use `command_status` with `command_id`, a byte `cursor`, and
optional `wait_ms` (0–5000) to read more output; use `command_stop` to stop it.
`yield_time_ms` (0–5000) controls the initial wait. An optional `timeout_ms` can
set a bounded timeout. A start receipt does not prove successful completion.
Set `background: true` for a workspace service the user wants to keep running
after the task. Keep the service in the foreground rather than shell-detaching
it with `&` or `nohup`; Core retains and supervises the process, and its status
and output remain available by command ID. Background services have no default
timeout and stop when explicitly stopped, their session is deleted, or Core
closes.

`Config.Commands.Timeout` defaults to ten minutes for ordinary commands and
bounds any explicit background timeout. `Config.Commands.MaxConcurrent`
defaults to four running commands across Core.
Ordinary commands belong to the current execution and agent and stop when the
task ends. Background services belong to their session and can be inspected or
stopped by later turns in that session; they cannot be controlled from another
session. Controlling an already-authorized command requires no additional
approval. Existing high-risk command approval and sandbox policies apply to
every new launch. Windows workspace commands use a PowerShell filesystem
drive rooted at the workspace; native programs use its actual directory. Commands
continue across model requests. Ordinary commands stop when the task pauses,
ends, is interrupted, or is discarded. Team branches also clean up their
ordinary commands when they finish or are cancelled by an early join.

If an agent submits a final answer while its commands still run, Core first
reminds it to inspect or stop them. A second consecutive submission pauses the
lead and stops its commands; `Recover` or `ResumeTask` can then continue. Workers
instead report a branch failure. The model can inspect command results directly
through the saved `command_status` tool output.

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

## Token benchmark

`cmd/lune-tokenbench` runs the synthetic request corpus against a configured
model profile in temporary workspaces and writes per-case token, duration,
task status, and objective-check results. Reports include final answers for
manual side-by-side quality review, but omit prompts and tool transcripts. Live
runs call the configured provider and incur its normal usage charges.

```sh
go run ./cmd/lune-tokenbench run --config ./config.json --profile main --repeat 3 --out baseline.json
go run ./cmd/lune-tokenbench run --config ./config.json --profile main --repeat 3 --out candidate.json
go run ./cmd/lune-tokenbench compare --baseline baseline.json --candidate candidate.json
```

Run one report from the unmodified revision and the other from the candidate
revision, using the same configuration and profile. If the baseline predates
this command, copy `cmd/lune-tokenbench` and `testdata/tokenbench` into a
temporary baseline worktree and run it there; the runner uses the existing Core
API and accepts usage records without the newer purpose field. Review each
report's `finalText` fields before treating the automated token, request-count,
status, and p95 checks as a quality result.


### Host execution details

`EventToolDetails` is a live-only companion to durable tool lifecycle events. It carries transient arguments and `ToolCall.Details`: bounded textual output, command snapshots (status, output and exit code), or structured file changes. Hosts merge records by execution and invocation identity; command status polls refer to the original command invocation. These bodies are excluded from JSON logging and durable runtime events. File changes are captured by the tool after successful writes, not inferred by the host from a later filesystem read.

Clarification options contain `label` and `description`. `UserQuestionRequest.Questions` contains the ordered batch; `UserQuestionAnswer.Answers` contains `{question_id, answer}` responses in the same order. `UserQuestionAnswer.ExecutionID` is required along with the graph wait identity, so a stale UI cannot answer a question from a later execution that reused the graph counters. Starting a new execution clears earlier graph input receipts. The batch representation is a breaking checkpoint format change for pending questions; discard older paused-question checkpoints before starting a fresh task.


### Prompt stability and diagnostics

Tool categories are discovered cumulatively within each session. Selecting a category adds its tools; it does not replace previously discovered categories. Discovery survives task completion and restart. Permission and Plan-mode metadata still filter every request and every execution. If a model calls a registered, discoverable tool before selecting its category, Core discovers it and executes the original call through the normal instruction, permission, approval and sandbox checks. Unknown or prohibited tools remain rejected. Independent team tasks start with empty discovery; same-model forks inherit the parent's discoveries.

Instructions contain fixed host rules. Changing task goals, Plan state, personal/project guidance and memory notes adds a model-only runtime context snapshot to history, rather than rewriting the fixed prompt prefix. Identical snapshots are not repeated; memory retrieval is snapshotted once per task. These messages are not part of the user-visible transcript. `Session.AgentContext` stores the associated opaque host metadata atomically with the conversation. Replacing canonical history clears it; older sessions bootstrap it on their next task.

Delegation limits are validated at execution, not encoded as a shrinking schema. Ordinary cache hints remain stable across task boundaries for the same session, agent and provider/model identity. DeepSeek still uses automatic caching and receives no unsupported cache-key parameter.

`UsageRecord` and usage events expose `RequestID`, `RequestAttempt` and `RequestChanges`. Request-attempt logs include instruction, tool and history fingerprints, never prompt text or tool arguments. Local changes describe what Core changed, not why the provider did or did not return a cache hit. Cache hits remain provider-dependent.

中文：工具类别在会话内累计发现，跨任务和重启保留；发现不等于授权，自动纠正未发现类别的调用仍需经过项目指令、权限、授权与沙箱检查。动态任务信息以模型专用上下文快照追加，固定指令不随运行状态重写；会话元数据与对话原子保存。委派额度由执行层校验，不再修改工具声明。诊断记录请求身份、重试次数、本地变化与内容指纹，不记录正文或参数，也不把本地变化冒充服务端缓存失效原因。

An opt-in real API check is available in `internal/runtime`: set `LUNE_CACHE_SMOKE_CONFIG` to a private config.json, optionally set `LUNE_CACHE_SMOKE_DIR` and `LUNE_CACHE_SMOKE_HELPER`, then run `go test ./internal/runtime -run '^TestCacheRealAPISmoke$' -count=1 -v`. Each invocation is limited to 12 API requests, 250,000 estimated/counted input tokens and 10,000 output tokens, with at most 2,048 output tokens per response and no automatic model retries. A budget stop fails the smoke test and must be reported as incomplete validation, even if tools already produced the expected file. It uses a separate temporary workspace and does not alter the source configuration.

中文：真实 API 验证需显式设置配置路径，每次最多调用 12 次，并受输入、输出预算限制。到达上限即停止，不自动增加费用；工具结果正确但最后回复尚未完成时，也必须记录为未完成验证。测试使用独立临时目录，不修改源配置。

### Permission API changes

The supported modes are read-only, workspace, auto and full-access; the obsolete PermissionAsk constant has been removed. ResolveApproval(id, true) permits only one invocation, including external-directory access. Hosts must call ResolveApprovalDecision with ApprovalAllowSession to remember a session grant. command_status is read-only; command_stop is a command permission operation and is unavailable in Plan/read-only mode.
