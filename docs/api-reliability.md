# API execution and recovery

`apiapp` composes the API module's `Middleware.Protect`; CORS and request logging
remain outer wrappers. It preserves the server configuration schema. See the
launcher `docs/API.md` for product routes and the API module's
`docs/reliability.md` for transport limits and journal compatibility.

## HTTP chat

`POST /api/v1/chat/complete` asks the current chat provider once and returns
`{"response":"..."}`. It does not create a session, append messages, or send a
tool schema. An empty message is 400. A provider error is 500. A tool call or
blank completion is 502. This is not a chat turn and has no `session_id`.

Both chat endpoints reject an explicit unknown session with 404 before invoking
the Agent. Omitting a session ID creates a session, and a successful synchronous
response belongs to that actual saved session. JSON is limited to 2 MiB and
must contain one complete value. Explicit `max_iterations` accepts 0 through
300, with zero selecting the configured default.

SSE retains existing event type and content fields, adding `run_id`, `event_id`,
the SSE `id:` line, creation time and usage when present. Errors contain a
string plus `error_code` and `status=failed`; an error is never followed by a
successful `complete`. Cancellation produces `type=cancelled,status=cancelled`
when the connection is still writable. A channel closing without a done event
is a failure. Heartbeats, rolling write deadlines and disconnect cancellation
remain enabled.

These SSE IDs identify a live response; this HTTP endpoint does not persist an
SSE event replay cursor. Reposting a chat request can execute another turn.
Use WebSocket request IDs and reconnect cursors when retryable durable delivery
is required. A client must distinguish completed, failed and cancelled states.

## WebSocket chat

The client chooses a stable `id` for each chat request. The same session and ID
with the same input reuses the accepted run and its stored events; it never
enqueues another execution. Changing input under that ID returns
`REQUEST_CONFLICT`. Failed initial durable acceptance returns `PERSIST_FAILED`
without enqueueing execution. A replay load error returns `RECOVERY_FAILED`.

Execution also requires the running-state write to succeed. An event write
failure marks the accepted run as failed, cancels active execution and suppresses
later output. `PERSIST_FAILED` is sent directly because the unavailable store
cannot persist that notification; it has no durable replay cursor. Storage
failure takes precedence over cancellation in the final run state. This also
prevents startup metadata scans from executing a run whose full history fails
validation when loaded lazily.

Reconnect uses the last event actually received, not the last event attempted
by the server. The handler serializes replay per connection and uses bounded,
cancellable reliable sends. It buffers live events during the snapshot and
deduplicates them afterward. A slow client can be disconnected explicitly and
resume from its cursor; clients should deduplicate `event_id` as well.

`max_iterations` is applied as an actual LoopConfig for the public Agent. A
legacy injected runtime unable to apply it rejects the parameter. `profile_id`
is rejected as `UNSUPPORTED_PARAMETER`, because the current Agent has no
isolated profile-selection entrypoint; it never changes the global model as
a side effect of one request. Use the model profile administration API for a
deliberate default change.

A stream must receive Done to be completed. Partial or empty EOF and an error
event without an error cause fail the run. Retrying does not regress a replayed
completed result to an old running status. This request deduplication does not
promise transactional execution of arbitrary external tools after a crash.

### OpenUI actions

`ui_delta` trees are retained in memory per session while they contain a
catalog-approved `Actions` node. A `ui_action` must name the connected session,
an existing tree, and one of that tree's action items. Streaming, unknown, or
already-consumed trees are rejected with `UI_ACTION_REJECTED`; a process restart
expires outstanding controls. A valid action is consumed once and queued as a
normal streaming Agent turn with its selected label and structured values. The
handler does not execute a tool from the widget payload: read/write routing and
the existing approval policy remain in the Agent loop.

## gRPC library

`lh serve` does not start a gRPC listener. Embedders can use `NewGRPCServer`,
which inherits the Agent's server keys and rate setting, or
`NewGRPCServerWithOptions` to supply transport configuration and a temporary
pairing-key validator. Authentication, admission and credential quotas apply
to unary and streaming methods, including health and reflection. Without a
key, only a loopback peer without forwarding metadata is accepted.

Chat and ChatStream use real sessions and LoopConfig, reject unknown IDs, and
return actual available execution statistics. ChatStream emits content as the
Agent produces it, with one terminal Done and no byte slicing of UTF-8. Model
errors and cancellation end with gRPC status rather than a success chunk.

`max_iterations` accepts 0 through 300. Nonempty metadata is explicitly rejected
because it has no defined execution semantics. The existing proto3 bool has no
presence bit: omitted `auto_approve` is false, while explicit true follows the
authenticated owner's HTTP override semantics. The current protobuf stream
contains content/session/done, not intermediate tool or approval events.
Missing optional backends return Unavailable; unexpected handler panics become
a generic Internal error. The wrapper owns listener and limiter shutdown.

## Validation

Tests use temporary runtime homes and local model servers. The RAG stream
handler tests also use isolated watch directories. Run each module's race suite
and vet, then validate the launcher against published dependency versions with
`GOWORK=off`. Slow-client replay, duplicate request IDs, input conflicts, unknown
sessions, failed/partial/cancelled streams and shutdown are regression cases.
