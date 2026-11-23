# Lune Core Runtime Protocol v1

The protocol connects frontends to one independently running Core process. A
Core process owns one workspace and state directory. Each session operation
names its `sessionId`, so one process can serve different sessions to different
frontends. Clients that use the same session ID share its history, queue,
pending requests, and event stream.

## Connections

Local foreign-language frontends launch `lune-core stdio`. Standard input and
standard output carry one UTF-8 JSON-RPC 2.0 message per line. Standard output
is reserved for protocol messages. Logs go to standard error. Closing standard
input shuts down the child process.

Remote frontends connect to `wss://host:port/v1/runtime`. The server requires
TLS certificate and key files and a non-empty token file. Send the token in the
HTTP upgrade request as `Authorization: Bearer <token>`. The server defaults to
`127.0.0.1:18092`. Set `--listen 0.0.0.0:18092` only when the service should
accept connections from other machines. Browser clients are not supported by
this version.

Both connections carry JSON-RPC 2.0 objects. Stdio uses newline-delimited JSON.
WebSocket uses one text message per JSON object. The maximum message size is
32 MiB. Requests may be processed concurrently, so clients match responses by
their JSON-RPC `id`. Notifications have no `id`. After connecting, call
`runtime.hello` and require `protocolVersion` to equal `1`.

## Methods

| Method | Parameters | Result |
|---|---|---|
| `runtime.hello` | optional `clientName`, `clientVersion` | protocol version and limits |
| `session.create` | optional `history` | created session |
| `session.list` | optional `offset`, `limit`, `includeArchived` | page of sessions |
| `session.get` | `sessionId` | session metadata and settings |
| `session.history` | `sessionId` | provider-neutral history JSON |
| `session.setTitle` | `sessionId`, `title` | `{ "updated": true }` |
| `session.setSettings` | `sessionId`, `settings` | `{ "updated": true }` |
| `session.setArchived` | `sessionId`, `archived` | `{ "updated": true }` |
| `session.delete` | `sessionId` | `{ "deleted": true }` |
| `model.list` | none | configured model profiles |
| `runtime.state` | `sessionId` | task, queue, and goal state |
| `runtime.pending` | `sessionId` | pending approvals and interaction requests |
| `runtime.subscribe` | `sessionId`, `subscriptionId`, `afterCursor` | subscription ID |
| `runtime.unsubscribe` | `subscriptionId` | whether it was removed |
| `turn.submit` | `sessionId`, `submissionId`, `input`, optional `mode`, `model`, `maxToolRounds`, `execution` | accepted or queued turn |
| `turn.wait` | `sessionId`, `executionId` | terminal turn result |
| `turn.interrupt` | `sessionId`, `executionId` | whether interruption was accepted |
| `turn.recover` | `sessionId`, optional `execution` | resumed turn submission |
| `turn.discardPaused` | `sessionId` | whether the paused turn was discarded |
| `approval.resolve` | `sessionId`, `id`, `decision` | resolution acknowledgement |
| `question.resolve` | `sessionId`, `answer` | resolution acknowledgement |
| `interaction.resolve` | `sessionId`, `id`, `response` | resolution acknowledgement |
| `toolRecovery.resolve` | `sessionId`, `invocationId`, `decision` | resolution acknowledgement |

Session results contain metadata and settings. Read conversation content with
`session.history`. Session setting fields use camel case, including
`modelProfile`, `contextWindow`, and `autoCompactThreshold`.

`mode` is `start_if_idle`, `queue_input`, or `steer_input`. The `execution`
object uses the Core `ExecutionOptions` JSON field names, such as `max_steps`,
`max_concurrency`, and `token_budget`. A frontend cannot set the permission
mode or change the server's workspace permissions through this protocol.

`turn.submit` requires a frontend-generated `submissionId`. The Core persists
the accepted result atomically with the queue entry and event. Repeating the
same ID with the same request returns the original queue and execution IDs.
Reusing it with different request content returns error data code
`submission_conflict`. IDs are scoped to their session.

For each subscription, the server sends a JSON-RPC notification with method
`runtime.event` and params `{ "subscriptionId": "...", "event": { ... } }`.
Events with a non-zero cursor are durable. Model stream increments are live
only and may be dropped for a slow client. On reconnect, subscribe from the
last durable cursor and refresh `runtime.state` and `runtime.pending` after
receiving the `runtime_replay_complete` event.

Errors use standard JSON-RPC codes for malformed requests, invalid parameters,
and unknown methods. Runtime errors use code `-32000` and include a stable
application code in `error.data.code`.

## Starting the service

```sh
lune-core stdio --workspace /path/to/workspace --sandbox-helper /path/to/lune-sandbox
lune-core serve --workspace /path/to/workspace --listen 0.0.0.0:18092 \
  --tls-cert /path/to/server.crt --tls-key /path/to/server.key \
  --token-file /path/to/runtime.token
```

The standalone process exclusively owns its state directory. It refuses to
start if an embedded or standalone Core already has that directory open.

## Python stdio example

Run the [Python example](../examples/runtime_stdio.py) with a configured
workspace and the built binary.

```sh
python3 examples/runtime_stdio.py ./lune-core /path/to/workspace \
  "Ask me which environment to summarize before writing the summary." --answer local
```

The example subscribes before submitting, answers a clarification, denies
tool approvals, and reads the final result. The parent closes stdin and waits
for the child to exit.

## Disconnect and restart

The subscription acknowledgement arrives before the first event for that
subscription. Store the latest durable cursor separately for each session.
On WSS reconnect, call `runtime.hello`, subscribe from that cursor, and refresh
state and pending requests after the replay marker. If a submit response was
lost, retry the same request with its original `submissionId`.

Each connection buffers up to 64 outgoing messages and 32 MiB, including
the message being written. The service closes the connection if either limit
is exceeded or a write takes longer than five seconds. Error `-32003` means
the server could not admit a request because the concurrent request limit
was reached.

A WSS client disconnect leaves admitted work running. Closing stdio input or
stopping the service stops new execution, preserves queued turns and pauses
active turns that have a resumable checkpoint. After starting Core again,
opening the session lets the queue drain. Use `turn.recover` to resume a paused
turn, or answer its pending clarification if one remains.
