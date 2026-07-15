# Connector protocol v1

Status: proposed investigation result for issue #3. This protocol is not implemented.

## Decision

Replace SSH polling, HTTP plugin pushes, UDP ingestion, and direct browser-to-relay control with two Go services:

- a central control plane on the self-hosted NixOS server;
- an outbound connector running as the same OS user as each Herdr server.

Use TypeScript for the PWA, but do not use TypeScript/Node for either daemon. Keep the wire contract language-neutral JSON so a future implementation can change language without changing protocol semantics.

The connector uses Herdr's local socket API as its only control surface. The `herdr-push` plugin is not part of v1.

## Confirmed product constraints

- One OIDC-authenticated operator.
- Up to 10 hosts and tens of concurrent agents.
- Connectors make outbound connections only.
- V1 supports status, output, prompt responses, text, special keys, and interrupt.
- Session launch, termination, host power, and arbitrary shell execution are out of scope.
- Action audit metadata is durable; prompt and terminal content is transient.
- Interactive actions fail closed and are never replayed automatically.
- The PWA requires reliable Web Push, but browser and push protocols are outside this connector contract.

## Verified Herdr integration

The investigation used Herdr 0.7.3, socket protocol 16, on July 15, 2026. Source claims were checked against tag `v0.7.3`, commit `299dd4163a96381ec2d8e5bde13d7ba6d6432373`.

Verified interfaces:

| Need | Herdr interface | Notes |
| --- | --- | --- |
| Compatibility | `ping` | Returns Herdr version, protocol, and capabilities. |
| Bootstrap | `session.snapshot` | Returns workspaces, tabs, panes, agents, and stable terminal IDs. |
| Agent lookup | `agent.get` | Accepts a terminal ID and returns the current pane mapping and agent state. |
| State changes | `events.subscribe` | Supports topology, agent-detection, status, exit, and selected output events. |
| Output | `pane.read` or `agent.read` | Bounded to 1,000 lines; v1 uses text without ANSI. |
| Literal text | `agent.send` or `pane.send_text` | Sends text without Enter. |
| Text plus keys | `pane.send_input` | Sends both through one Herdr API request. |
| Special keys | `pane.send_keys` | Accepts Herdr key-combo names such as `enter`, `esc`, and `ctrl+c`. |

The raw transport is newline-delimited JSON over a Unix socket. Named Herdr sessions use separate sockets. Herdr documentation recommends the raw API for long-lived event subscribers.

The following live probes succeeded:

```json
{"id":"probe:ping","method":"ping","params":{}}
{"id":"probe:ping","result":{"type":"pong","version":"0.7.3","protocol":16,"capabilities":{"live_handoff":true,"detached_server_daemon":true}}}
```

```json
{"id":"probe:subscribe","method":"events.subscribe","params":{"subscriptions":[{"type":"pane.agent_status_changed","pane_id":"wR:p1"}]}}
{"id":"probe:subscribe","result":{"type":"subscription_started"}}
```

### Integration conclusions

- `terminal_id` is stable across pane moves during one live Herdr runtime. Restore or handoff may allocate a new terminal ID, so a new snapshot removes the old identity rather than correlating it for control. A pane ID may change when a pane moves between workspaces, so pane IDs must never be global routing keys.
- The global agent key is `(host_id, herdr_instance_id, terminal_id)`.
- Herdr events have no source sequence. Bootstrap is a subscribe-snapshot-subscribe-snapshot reconciliation: start and buffer global lifecycle subscriptions, take a snapshot to enumerate panes, start pane-specific status subscriptions, then take the authoritative snapshot published upstream.
- Newly created panes trigger status-subscription replacement followed by `agent.get`. Any subscription disconnect triggers full reconciliation. A full snapshot every 30 seconds bounds undetectable Herdr event loss.
- Status events identify panes, so the connector resolves them through its snapshot cache or `agent.get` before publishing an agent update. Connector sequence numbers protect only the upstream stream; they do not repair Herdr source gaps.
- Generic `pane.output_changed` is not a raw subscription option in protocol 16. The connector reads only actively viewed agents, coalesces unchanged output, and stops polling when the view subscription closes.
- The external `herdr-push` plugin only POSTs a partial status event. It provides no authenticated connector identity, command result, snapshot, ordering, or reconnect behavior and is not suitable for this design.
- Agent-specific Claude JSONL and OpenCode SQLite scraping is not required in v1. Herdr remains the terminal and state authority.

### Herdr safety gap

Herdr 0.7.3 input handlers resolve a pane or terminal and then write bytes to its PTY. They do not accept an expected terminal ID, expected agent label, expected state, output revision, or prompt fingerprint. `pane.read` also reports revision `0`, so that field cannot currently guard a write.

The connector must therefore re-resolve the terminal immediately before every write and reject a target that is no longer a detected agent. Prompt responses must also re-read and hash the current prompt. This narrows the race but cannot make the precondition and write atomic.

Before calling v1 fully fail-closed, add an upstream Herdr operation equivalent to:

```json
{
  "method": "agent.send_input_checked",
  "params": {
    "terminal_id": "term_...",
    "expected_input_revision": 42,
    "expected_agent": "opencode",
    "expected_status": "blocked",
    "expected_content_hash": "sha256:...",
    "text": "",
    "keys": ["enter"]
  }
}
```

Herdr must issue an `input_revision` that increments on agent identity, semantic status, or detection-buffer content changes, return terminal text and that revision from one atomic read snapshot, validate it with the other write preconditions, validate the current detection-buffer hash for prompt responses, and enqueue the complete input atomically in one server operation. In 0.7.3, reads return revision `0` and `pane.send_input` enqueues text and keys separately. Production v1 therefore disables every write operation on Herdr 0.7.3 and advertises the instance as read-only. Full interaction is blocked until Herdr provides atomic read revisions and checked atomic input; there is no unsafe compatibility mode. Any error after a checked local write begins still has outcome `unknown` unless the future Herdr contract proves otherwise.

## Architecture

```text
PWA
  | HTTPS + OIDC session
  v
reverse proxy
  | trusted identity headers
  v
control plane
  | WSS + mTLS, server-routed by certificate identity
  v
outbound connector (one OS user, one host)
  | local NDJSON socket
  v
Herdr server and managed agent PTYs
```

The browser never connects to a host connector. The control plane never opens SSH connections to a host. The connector exposes no listening network port.

## Core entities

| Entity | Durable | Identity and purpose |
| --- | --- | --- |
| Host | Yes | Server-assigned UUID bound to an enrolled connector certificate. |
| Connector credential | Yes | Certificate fingerprint, validity, rotation, and revocation state. |
| Connector connection | No | UUID for one authenticated WebSocket lifetime. |
| Herdr instance | Configuration only | `(host_id, instance_id)` where `instance_id` is `default` or a configured named session. |
| Agent session | No | `(host_id, instance_id, terminal_id)` plus mutable pane/workspace/tab routing data. |
| Prompt snapshot | No | Agent key, normalized prompt hash, excerpt, and locally derived options. |
| Output subscription | No | Connection-scoped request for bounded terminal output. |
| Action | Audit metadata only | UUID, OIDC subject, target, operation type, timestamps, and outcome. |
| Audit event | Yes | Append-only action and connector lifecycle metadata without terminal content. |

`host_id` always comes from the authenticated certificate mapping. A claimed host ID in a message is checked for equality but never trusted as identity.

## Enrollment and authentication

1. The operator creates a one-time enrollment token in the control plane. It is random, single-use, host-scoped, and expires after 10 minutes.
2. The connector generates a private key locally and submits a certificate signing request over HTTPS with the token.
3. The server returns a host ID and a 30-day client certificate issued by the private deployment CA.
4. The connector stores the key with mode `0600` and connects to the dedicated connector endpoint with mTLS.
5. The server maps the verified certificate to exactly one host and checks revocation in application state.
6. The connector rotates its certificate seven days before expiry while authenticated with the current certificate.
7. Revocation prevents new connections and closes an existing connection for that host.

OIDC authenticates the browser only. OIDC cookies, shared relay tokens, URL query tokens, and browser local storage are not connector credentials.

Only one connector connection may own a host at a time. `connector_instance_id` identifies one connector process. A second connection for an owned host is rejected with `HOST_ALREADY_CONNECTED` until the existing connection closes or misses its heartbeat lease. Certificate rotation may overlap certificate validity, but it does not permit concurrent connections. Actions are routed only to the connection that owns the current host lease.

### Operator identity boundary

- Accept exactly one configured OIDC issuer, audience, and subject allowlist entry for v1.
- Require a configured MFA assurance claim in production. An identity provider that cannot prove MFA does not meet the enterprise v1 profile.
- Limit operator sessions to 30 minutes idle and eight hours absolute before reauthentication.
- Revalidate authorization when establishing browser WebSockets and before accepting every action.
- Use secure, HTTP-only, same-site cookies, an exact Origin allowlist, and CSRF protection on state-changing HTTP requests.
- The control plane listens on loopback or a Unix socket behind the reverse proxy. Direct network access is blocked.
- The proxy removes inbound identity-header copies before setting its own values. The proxy-to-service hop is loopback, a Unix socket, or mutually authenticated TLS.
- If a bearer token reaches the service instead of trusted headers, the service validates signature, issuer, audience, expiry, and subject itself.

## Transport

- WebSocket over TLS at a dedicated connector endpoint, for example `/v1/connectors/ws`.
- TLS 1.2 minimum, TLS 1.3 preferred, with client certificate verification.
- WebSocket text frames containing one JSON message each.
- Per-message compression disabled.
- Maximum message size: 256 KiB.
- Heartbeat: WebSocket ping every 20 seconds; close after 10 seconds without pong.
- Connector reconnect: exponential backoff with full jitter, starting at 1 second and capped at 60 seconds.
- A reconnect creates a new `connection_id`, reacquires the single-host lease, creates new state epochs, and performs full reconciliation. Nothing from the previous connection is replayed.

## Common envelope

Every application message has this shape:

```json
{
  "protocol": 0,
  "message_id": "019f64ca-3000-7000-8000-000000000001",
  "type": "connector.hello",
  "sent_at": "2026-07-15T08:00:00Z",
  "body": {
    "min_protocol": 1,
    "max_protocol": 1,
    "connector_version": "0.1.0",
    "connector_instance_id": "019f64ca-3000-7000-8000-000000000101",
    "display_name": "workstation",
    "platform": "linux",
    "architecture": "amd64",
    "capabilities": []
  }
}
```

| Field | Contract |
| --- | --- |
| `protocol` | `0` for the fixed bootstrap handshake; the selected connector major for all later messages. |
| `message_id` | UUIDv7, unique for the sender. It is for tracing, not action deduplication. |
| `type` | Message discriminator from the sections below. |
| `sent_at` | UTC RFC 3339 timestamp. It is diagnostic and never used as an ordering source. |
| `body` | Type-specific object. Unknown fields are ignored within a supported protocol major. |

Malformed messages receive `protocol.error`. Three malformed messages in one minute close the connection. Unknown fields are ignored within a supported major. Unknown message types are rejected with `UNSUPPORTED_MESSAGE`; a sender may use a new message type only after negotiating its capability.

## Handshake

The connector sends `connector.hello` first using bootstrap `protocol: 0`. Bootstrap v0 is a fixed schema and is not extended with required fields:

| Body field | Type | Required | Contract |
| --- | --- | --- | --- |
| `min_protocol` | integer | Yes | Oldest connector protocol supported. |
| `max_protocol` | integer | Yes | Newest connector protocol supported. |
| `connector_version` | string | Yes | Semantic build version. |
| `connector_instance_id` | UUID | Yes | Random identity for this connector process lifetime. |
| `display_name` | string | Yes | Untrusted operator-facing host label, 1 to 80 characters. |
| `platform` | string | Yes | `linux` or `darwin` in v1. |
| `architecture` | string | Yes | For example `amd64` or `arm64`. |
| `capabilities` | string array | Yes | Supported optional operations. |

The server responds using bootstrap `protocol: 0` with `server.welcome`:

| Body field | Type | Contract |
| --- | --- | --- |
| `selected_protocol` | integer | Highest mutually supported major. |
| `server_min_protocol` | integer | Oldest major supported by the server. |
| `server_max_protocol` | integer | Newest major supported by the server. |
| `accepted_capabilities` | string array | Intersection the server may use on this connection. |
| `connection_id` | UUID | Identity of this connection only. |
| `host_id` | UUID | Host identity derived from the client certificate. |
| `heartbeat_interval_ms` | integer | Server heartbeat interval. |
| `max_message_bytes` | integer | Negotiated message limit, never above 262144 in v1. |
| `server_time` | timestamp | Diagnostic clock comparison. |

If there is no common major, the server sends bootstrap `protocol.error` with `UNSUPPORTED_PROTOCOL` and closes with WebSocket code 4406. After `server.welcome`, both peers encode all messages with `selected_protocol` and use only `accepted_capabilities`.

## State stream

After `server.welcome`, each configured Herdr instance performs reconciliation and then sends `state.snapshot`. No partial pre-reconciliation state is published.

### `state.snapshot`

| Body field | Type | Contract |
| --- | --- | --- |
| `instance_id` | string | `default` or configured named-session ID. |
| `epoch` | UUID | New random value whenever connector state is rebuilt. |
| `sequence` | integer | Always `0` for a snapshot. |
| `herdr_version` | string | Version returned by `ping`. |
| `herdr_protocol` | integer | Local Herdr socket protocol. |
| `status` | string | `online`, `degraded`, or `incompatible`. |
| `capabilities` | string array | Instance capabilities; write operations require `checked_input.v1`. |
| `agents` | array | Complete current agent set for the instance. |

An agent record contains:

| Field | Type | Contract |
| --- | --- | --- |
| `terminal_id` | string | Stable identity inside the current live Herdr runtime; it may change after restore or handoff. |
| `pane_id` | string | Current mutable route to the terminal. |
| `workspace_id` | string | Current Herdr workspace. |
| `tab_id` | string | Current Herdr tab. |
| `agent` | string | Authoritative detected or reported agent label. |
| `display_name` | string or null | Optional user-facing name. |
| `status` | string | `idle`, `working`, `blocked`, `done`, or `unknown`. |
| `project` | string or null | Redacted project label, not a required full path. |
| `generation` | integer | Connector-assigned value, starting at 1 and incremented on every material record or prompt change. |
| `herdr_input_revision` | integer | Herdr-issued identity/status/detection-content revision; `0` means unavailable and permits reads only. |

### `state.delta`

Subsequent changes send `state.delta` with the same `instance_id` and `epoch`, a sequence exactly one greater than the last accepted sequence, and a `changes` array. Every upsert carries the current exact connector `generation` and Herdr-issued `herdr_input_revision`. Connector generation increments when identity, routing, label, status, project, or prompt fingerprint changes. A change is either:

```json
{"operation":"upsert","agent":{"terminal_id":"term_...","pane_id":"w1:p1","workspace_id":"w1","tab_id":"w1:t1","agent":"opencode","display_name":null,"status":"blocked","project":"api","generation":2,"herdr_input_revision":42}}
```

or:

```json
{"operation":"remove","terminal_id":"term_...","reason":"pane_closed"}
```

A connector-to-server sequence gap, epoch mismatch, or unknown terminal removal invalidates that instance cache. The server sends `state.resync`; the connector performs Herdr reconciliation and answers with a new snapshot and epoch. Deltas from an old epoch are discarded. This sequence does not imply that Herdr itself supplies sequenced events.

`state.resync` contains `instance_id`, the server's current `expected_epoch` or null, `expected_sequence` or null, and reason `gap`, `epoch_mismatch`, `unknown_remove`, or `operator_refresh`. The connector does not attempt delta repair; it always reconciles and sends a new epoch snapshot.

## Prompt snapshots

While an agent is blocked, the connector reads and re-evaluates the prompt at least once per second. A changed source hash or adapter result increments connector generation and emits a new `prompt.snapshot`:

| Body field | Type | Contract |
| --- | --- | --- |
| `target` | target | Exact agent key. |
| `state_epoch` | UUID | Current state epoch. |
| `state_sequence` | integer | State sequence used to resolve the target. |
| `agent_generation` | integer | Exact generation of the blocked agent record. |
| `herdr_content_hash` | string | SHA-256 of the normalized detection-buffer input; future checked input validates it atomically. |
| `fingerprint` | string | SHA-256 of the canonical prompt document defined below. |
| `excerpt` | string | At most 8 KiB; transient and never written to audit logs. |
| `excerpt_truncated` | boolean | Whether the canonical prompt is longer than the excerpt. |
| `adapter_version` | string | Version of the local prompt parser. |
| `options` | array | Locally resolvable option IDs and display labels. |

Prompt extraction is deterministic:

1. Call `agent.read` for the terminal with `source: detection`, `format: text`, and `strip_ansi: true`.
2. Decode the JSON string as UTF-8, convert CRLF and bare CR to LF, and preserve every other character including trailing spaces.
3. Limit adapter input to the last 64 KiB at a UTF-8 code-point boundary and hash those exact bytes as `herdr_content_hash`.
4. Run the versioned adapter, which returns one canonical prompt string and an ordered list of option ID/label pairs.
5. Use the canonical prompt, not the full terminal read or display excerpt, for fingerprinting.

The canonical prompt document contains `v`, `host_id`, `instance_id`, `terminal_id`, `adapter_version`, `prompt`, and the ordered `options`. Serialize it with RFC 8785 JSON Canonicalization Scheme and encode the hash as `sha256:<lowercase hex>`. The hash therefore binds the prompt, target, parser, and option mapping. `excerpt` is the first 8 KiB of the canonical prompt at a UTF-8 boundary. `prompt.respond` carries both `expected.prompt_fingerprint` for adapter binding and `expected.herdr_content_hash` for atomic local validation.

If no known adapter matches, `options` is empty and semantic approve/reject is unavailable. Generic write interaction is also unavailable unless the Herdr instance advertises `checked_input.v1`.

## Output subscriptions

Terminal content is opt-in and transient.

### `output.subscribe`

Server to connector fields:

| Field | Type | Contract |
| --- | --- | --- |
| `subscription_id` | UUID | Connection-scoped ID. |
| `target` | target | Agent to read. |
| `source` | string | `visible`, `recent`, `recent_unwrapped`, or `detection`. |
| `lines` | integer | 1 to 1,000. |
| `poll_interval_ms` | integer | 500 to 5,000; server default is 1,000. |

The connector responds with `output.snapshot` containing `subscription_id`, `target`, `state_epoch`, `agent_generation`, `herdr_input_revision`, `content_revision`, complete replacement `text`, and boolean `truncated`. For a write-capable instance, Herdr must sample text and `herdr_input_revision` atomically. The connector publishes that pair only when its serialized state actor maps the same input revision to the reported agent generation; a mismatch is reconciled and read again. Revision `0` identifies an unguarded 0.7.3 read and can never authorize a write. `content_revision` is lowercase SHA-256 of the exact UTF-8 text. Text is capped at 32,768 Unicode scalar values, which is at most 128 KiB UTF-8. Unchanged text is not resent. `output.unsubscribe` contains only `subscription_id`; an unknown ID is an idempotent no-op.

The server sends `output.unsubscribe` when the PWA leaves the view. All output subscriptions end on disconnect. A connector permits at most four output subscriptions, one per terminal, and enforces an aggregate eight reads per second. Output messages are coalesced under backpressure and never persisted.

## Action protocol

Every control operation starts with `action.request`. A request that passes final validation receives `action.received` before execution. A pre-execution rejection returns `action.result` directly. Exactly one terminal result is sent when the connection survives long enough to report it.

### Target

```json
{
  "host_id": "019f64ca-1000-7000-8000-000000000001",
  "instance_id": "default",
  "terminal_id": "term_656a094363b4b4"
}
```

The connector rejects a host ID that does not match its certificate identity. It resolves the terminal ID to the current pane immediately before execution.

### `action.request`

| Body field | Type | Contract |
| --- | --- | --- |
| `action_id` | UUIDv7 | Idempotency and audit identity. |
| `target` | target | Exact host, instance, and terminal. |
| `timeout_ms` | integer | Relative timeout; values outside the operation range are rejected. |
| `expected` | object | Exact state epoch, agent generation, label, allowed statuses, and conditional prompt fingerprint. |
| `operation` | tagged object | One operation from the table below. |

The server never sends the same `action_id` twice. The connector starts a monotonic timer after reading the complete frame, keeps a bounded in-memory set of completed action IDs for the current connection, and rejects a duplicate. It does not persist or resume actions after restart. V1 has no action cancellation message.

All five body fields are required. `target.host_id` is a UUID, `instance_id` is 1 to 80 ASCII letters, digits, dots, underscores, or hyphens, and `terminal_id` is a 1 to 128 character opaque Herdr ID. `timeout_ms` is an integer from 1 through the operation maximum.

The required `expected` object is:

| Field | Type | Contract |
| --- | --- | --- |
| `state_epoch` | UUID | Must exactly equal the current instance epoch. |
| `agent_generation` | integer | Must exactly equal the current generation and be at least 1. |
| `herdr_input_revision` | integer | Required for all actions; writes require a nonzero revision that Herdr checks atomically. |
| `agent` | string | Must exactly equal the current authoritative label, 1 to 80 characters. |
| `statuses` | string array | One to five unique status values; current status must be a member. |
| `prompt_fingerprint` | string | Required only for `prompt.respond`; omitted for every other operation. |
| `herdr_content_hash` | string | Required only for `prompt.respond`; checked atomically by Herdr. |

Any status or prompt change increments `agent_generation`, including a transition from blocked to working and back to blocked. Every write binds to both the exact published generation and Herdr-issued revision. Connector generation rejects stale control-plane state; Herdr revision and content hash close the local validation-to-write race.

The normative machine-readable request, operation, receipt, and result schemas are in `protocol/connector-v1-action.schema.json`. They reject unknown operation fields, define UTF-8 byte-limit extensions, and take precedence over examples in this document.

### Operations

| Operation type | Fields | Timeout | Execution contract |
| --- | --- | --- | --- |
| `agent.read` | `source`, `lines` | 5 s | One bounded atomic text/revision snapshot when write-capable; revision `0` read-only result on 0.7.3. |
| `agent.send_text` | `text` | 3 s | 1 to 4,096 printable Unicode scalar values, at most 16 KiB UTF-8, with no implicit Enter. |
| `agent.send_keys` | `keys` | 3 s | 1 to 16 allowed Herdr key names. |
| `agent.send_input` | `text`, `keys` | 3 s | At least one non-empty field; checked atomic Herdr input only. |
| `agent.interrupt` | none | 3 s | Checked atomic Herdr input containing `ctrl+c`. |
| `prompt.respond` | `option_id` | 3 s | Re-read prompt, require blocked state and `expected.prompt_fingerprint`, then use the bound local adapter mapping. |

Text must be valid UTF-8 and contain no U+0000 through U+001F or U+007F through U+009F code points. Newline, tab, escape, and interrupt are represented only by allowed keys.

V1 allowed special keys are `enter`, `esc`, `tab`, `shift+tab`, `up`, `down`, `left`, `right`, `pageup`, `pagedown`, `home`, `end`, `backspace`, `delete`, and `ctrl+c`. Adding keys is a capability change. Arbitrary modifier chords are rejected.

All write operations require the instance capability `checked_input.v1`. Herdr 0.7.3 has no such capability, so the connector rejects these operations with `HERDR_INCOMPATIBLE` before `action.received`.

Operation-specific outcomes:

| Operation | Success result | Definitive rejection or failure before write | Unknown outcome |
| --- | --- | --- | --- |
| `agent.read` | `state_epoch`, `agent_generation`, `herdr_input_revision`, `text`, `truncated`, `content_revision` | Target, compatibility, limit, Herdr read error, or post-receipt timeout | Never side-effect unknown; connection loss is `failed/CONNECTION_LOST`, post-receipt timeout is `failed/DEADLINE_EXCEEDED`, and a new read may be issued. |
| `agent.send_text` | `herdr_acknowledged: true` | Invalid text, stale target/state, not an agent, or Herdr rejection before enqueue | Local response lost or send error after the write call begins. |
| `agent.send_keys` | `herdr_acknowledged: true` | Invalid key, stale target/state, not an agent, or Herdr rejection before enqueue | Local response lost or send error after the write call begins. |
| `agent.send_input` | `herdr_acknowledged: true` | Invalid text/key, stale target/state, not an agent, or Herdr rejection before atomic enqueue | Local response lost after Herdr may have accepted the atomic input. |
| `agent.interrupt` | `herdr_acknowledged: true` | Stale target/state, not an agent, or Herdr rejection before enqueue | Local response lost or send error after the write call begins. |
| `prompt.respond` | `herdr_acknowledged: true`, `option_id` | Prompt changed, unknown option, stale target/state, not blocked, or Herdr rejection before atomic enqueue | Local response lost after Herdr may have accepted the atomic input. |

Pre-execution code mapping is common to every operation: malformed body to `INVALID_MESSAGE`, unsupported operation to `UNSUPPORTED_OPERATION`, wrong host to `UNAUTHORIZED_HOST`, expired monotonic timer to `DEADLINE_EXCEEDED`, missing terminal to `TARGET_NOT_FOUND`, changed terminal route or identity to `STALE_TARGET`, epoch/generation/Herdr-input-revision/agent/status mismatch to `STALE_STATE`, absent active agent to `NOT_AN_AGENT`, unavailable socket to `HERDR_UNAVAILABLE`, and unsupported local API or missing checked-input capability to `HERDR_INCOMPATIBLE`. Text and key validation use `INVALID_TEXT` and `INVALID_KEYS`. Prompt fingerprint, content hash, or option mismatch uses `PROMPT_CHANGED`. Herdr errors known to occur before enqueue use `HERDR_REJECTED`; errors after execution begins use `OUTCOME_UNKNOWN`.

The operation timeout includes lock wait, final validation, local socket write, and Herdr response. Timeout is checked again after the terminal lock is acquired. A timeout before `action.received` is a rejection. A timeout after `action.received` follows the operation-specific unknown rule above. The connector does not attempt to cancel a Herdr request after writing it.

Before a write, the connector must:

1. Start the monotonic timeout and check the action ID.
2. Check the certificate-derived host ID.
3. Check the exact state epoch, connector generation, and Herdr input revision.
4. Resolve `terminal_id` with `agent.get`.
5. Require the expected agent label and one of the expected statuses.
6. Require that Herdr still reports a detected agent.
7. For `prompt.respond`, re-read and match both prompt fingerprint and Herdr content hash.
8. Wait for the per-terminal write lock within the same timeout.
9. Recheck timeout, target, state, and prompt after acquiring the lock.
10. Send `action.received`, execute once, then send `action.result`.

`action.received` means final validation completed and execution is starting. A validation timeout returns `rejected/DEADLINE_EXCEEDED` without `action.received`. A timeout or error after `action.received` is `unknown/OUTCOME_UNKNOWN` for writes. It does not prove that bytes reached the PTY or that the agent accepted them.

### `action.result`

| Field | Type | Contract |
| --- | --- | --- |
| `action_id` | UUID | Matches the request. |
| `operation_type` | string | Matches the requested operation discriminator. |
| `status` | string | `succeeded`, `rejected`, `failed`, or `unknown`. |
| `code` | string or null | Stable machine-readable result code. |
| `message` | string or null | Sanitized operator-facing text without prompt/output content. |
| `result` | object or null | Operation-specific bounded metadata; `agent.read` may contain text. |

Status meanings:

- `succeeded`: Herdr acknowledged the operation. It does not mean the coding agent accepted or completed it.
- `rejected`: no write was attempted because validation or a precondition failed.
- `failed`: a side-effect-free operation failed, or Herdr definitively rejected a write before any input enqueue was attempted.
- `unknown`: input may have been accepted but a definitive Herdr result was lost.

If the connector connection closes after the server sent a write action but before its terminal result, the server audits `unknown/OUTCOME_UNKNOWN`. A disconnected `agent.read` is audited as `failed/CONNECTION_LOST`. Neither side replays an action after reconnect. The operator must inspect fresh state and submit a new action with a new ID.

### Stable result codes

`INVALID_MESSAGE`, `UNSUPPORTED_PROTOCOL`, `UNSUPPORTED_MESSAGE`, `UNSUPPORTED_OPERATION`, `UNAUTHORIZED_HOST`, `HOST_ALREADY_CONNECTED`, `TARGET_NOT_FOUND`, `STALE_TARGET`, `STALE_STATE`, `NOT_AN_AGENT`, `PROMPT_CHANGED`, `INVALID_TEXT`, `INVALID_KEYS`, `DEADLINE_EXCEEDED`, `CONNECTION_LOST`, `HERDR_UNAVAILABLE`, `HERDR_INCOMPATIBLE`, `HERDR_REJECTED`, `OUTCOME_UNKNOWN`, `AUDIT_UNAVAILABLE`, `BUSY`, `RATE_LIMITED`, and `INTERNAL`.

Protocol-level failures use `protocol.error` with `in_reply_to` message ID or null, stable `code`, and a sanitized `message`. Valid action failures always use `action.result`.

## Backpressure and resource limits

- At most 16 Herdr instances and 256 agents per connector.
- At most 32 in-flight actions per connector and one write action per terminal.
- At most four output subscriptions, one per terminal, with an aggregate eight reads per second and burst eight.
- At most 60 action requests per host per minute with burst 10; rejected requests count toward the limit.
- High-priority state and action messages have a bounded queue of 256 messages.
- If a high-priority message cannot queue within two seconds, close the connection and require a snapshot after reconnect.
- Output has one replaceable queue slot per subscription. New output replaces older unsent output.
- Prompt excerpts are capped at 8 KiB, output at 128 KiB, action text at 16 KiB, and all frames at 256 KiB.
- Logs include IDs, sizes, durations, and codes. Logs never include text, keys, prompt excerpts, terminal output, certificates, or tokens.

## Failure and recovery table

| Failure | Required behavior |
| --- | --- |
| Control plane restart | Connector reconnects with jitter and sends full snapshots. No actions replay. |
| Connector restart | New connector instance, connection, and state epochs; server marks prior in-flight writes unknown and reads failed. |
| Herdr restart | Instance becomes degraded, socket reconnects, reconciliation produces a new snapshot, and changed terminal IDs replace old identities. |
| Network partition | Host lease expires after heartbeat timeout; in-flight writes become unknown and reads fail. No second connection owns the host before lease expiry. |
| Connector state sequence gap | Discard affected server cache and request full Herdr reconciliation. |
| Slow PWA/output consumer | Coalesce output; never delay action results or state. |
| Unsupported Herdr protocol | Report instance incompatible and reject all actions. |
| Herdr lacks checked atomic input | Advertise read-only instance capabilities and reject every write with `HERDR_INCOMPATIBLE`. |
| Expired/revoked certificate | Reject connection; no shared-token fallback. |
| Malformed connector data | Reject message, increment security metric, and close after threshold. |

## Audit contract

Persist:

- action ID and operation type;
- OIDC issuer and subject identifier;
- host, instance, and terminal IDs;
- request, receipt, completion, and timeout timestamps;
- result status and stable code;
- connector version, protocol version, and connection ID;
- text byte count or key count, but not values.

Do not persist:

- prompts, terminal output, sent text, key values, cwd paths, agent reasoning, certificates, enrollment tokens, or Web Push payload content.

Connector connect, disconnect, enrollment, rotation, revocation, incompatibility, and repeated malformed-message events also produce metadata-only audit records.

Audit intent must commit before the server dispatches a write action. If durable audit storage is unavailable, reject the action with `AUDIT_UNAVAILABLE`. Audit rows are append-only to the service account, retained for 90 days by default, readable only by the operator and backup principal, and included in encrypted backups. Completion-audit failure raises an alert and blocks subsequent writes until storage recovers. Issue #2 owns the final backup, integrity-export, and retention implementation.

Every connector-provided string is untrusted. Validate type, length, and control-character policy at ingestion, render it as text rather than HTML, and escape it in logs and metrics labels. This includes display names, agent labels, project labels, result messages, prompt excerpts, and output.

## Threat model

| Threat | Control and residual risk |
| --- | --- |
| Stolen browser session | OIDC MFA, short sessions, secure same-site cookies, origin/CSRF checks, and audit. The single operator still has full authorized control. |
| Compromised control plane | Connector accepts only the documented operation allowlist and has no explicit shell operation. A compromised server can still instruct agents to run commands and is effectively trusted with the enrolled user's agent privileges. |
| Compromised connector host | Certificate is host-scoped; server ignores claimed host identity. That host can forge its own state and output but cannot impersonate another non-compromised host. |
| Stolen connector key | File permissions, short certificate lifetime, rotation, revocation, and lifecycle audit. Hardware-backed keys are a future enhancement. |
| Network attacker | TLS plus mTLS, heartbeat timeouts, frame limits, and no credentials in URLs. |
| Replayed action | Unique action ID, connection-scoped duplicate cache, monotonic timeout, state epoch, preconditions, and no reconnect replay. |
| Duplicate connector | One active heartbeat lease per certificate-derived host; reject concurrent owners, including during certificate overlap. |
| Stale pane routing | Route by terminal ID and resolve current pane immediately before use. Pane ID is never global identity. |
| Agent exits to shell | Require checked atomic Herdr input with terminal, generation, agent, and status preconditions. Herdr 0.7.3 writes remain disabled. |
| Changed approval prompt | Connector-generated prompt hash and local option mapping plus checked atomic Herdr input. Herdr 0.7.3 writes remain disabled. |
| Sensitive output leakage | Opt-in bounded subscriptions, memory-only handling, no content logs/audit, disabled WS compression, and text-only escaped rendering for every connector-controlled field. |
| Resource exhaustion | Authentication before allocation, message/queue/subscription limits, per-host rate limits, and output coalescing. |

## Compatibility and rolling upgrades

Connector protocol and Herdr socket protocol are separate version domains.

- Connector protocol uses a fixed bootstrap v0 and a negotiated integer application major. V1 changes are additive: new optional fields, capabilities, and result codes.
- Unknown fields are ignored. New message types require a negotiated capability; otherwise they receive `UNSUPPORTED_MESSAGE`. Unknown operations are rejected, never guessed.
- Server supports the current and previous connector major for at least one release. Connector advertises a min/max range.
- Roll server first, then connectors. The server sends only capabilities accepted during handshake.
- A connector upgrade reconnects and sends new snapshots; it does not migrate in-flight actions.
- On startup the connector runs `ping`, inspects `herdr api schema --json`, and verifies required method names and response fields. Protocol 16 behavior observed in Herdr 0.7.3 is the read-only baseline. Production writes additionally require the future checked atomic input schema and `checked_input.v1` capability.
- A newer Herdr protocol is accepted only if required methods remain present. Missing or incompatible methods put the instance in `incompatible` state and disable writes.
- Protocol fixtures are versioned in the repository. Both server and connector implementations must pass the same fixture and rolling-version tests.

## Language comparison

| Criterion | Python | Go | Rust | TypeScript/Node |
| --- | --- | --- | --- | --- |
| Existing domain reuse | Best | Moderate | Low | Low |
| Reproducible Nix packaging | Adequate | Excellent | Excellent | Adequate |
| Per-host artifact | Runtime plus dependencies | Single binary | Single binary | Runtime plus dependencies |
| Linux/macOS connector delivery | Operationally heavy | Simple cross-build | More cross-build setup | Operationally heavy |
| Concurrency assurance | Async type checks are limited; logical races need explicit state machines | Race detector catches data races; logical races still need explicit state machines | Strongest memory/data-race guarantees; logical races still remain | Event-loop data races are limited; logical races need explicit state machines |
| Security posture | Memory-safe runtime, dynamic protocol typing, larger runtime/dependency closure | Memory-safe runtime, typed protocol, mature standard TLS, moderate dependency surface | Strongest compile-time memory safety, typed protocol, mature Rustls stack | Memory-safe runtime, typed source but runtime validation required, largest dependency surface |
| Observability | Mature libraries, framework choice required | Standard `slog`, mature Prometheus and OpenTelemetry | Excellent `tracing`, more integration code | Mature logging and OpenTelemetry, runtime-heavy deployment |
| Migration cost | Lowest code port, but transport/state architecture is discarded | Port only prompt behavior and fixtures from the 1,038-line relay | Same rewrite plus more type/lifetime design work | Same rewrite with no existing Node server code to reuse |
| Build and onboarding cost | Low | Low to moderate | Highest | Moderate |
| Runtime closure | Largest | Small | Smallest | Largest |
| Fit for this scale | Adequate | Best overall | More complexity than needed | Weak operational fit |

### Recommendation

Use Go for both the control plane and connector.

Reasons:

- `buildGoModule` gives reproducible Nix builds and a small runtime closure.
- `CGO_ENABLED=0` produces simple Linux and macOS connector artifacts.
- Goroutines and channels fit independent local-socket, WebSocket, heartbeat, and output loops; the race detector is a useful CI gate for data races, while protocol state machines and conformance tests address logical races.
- `slog`, Prometheus, and OpenTelemetry support structured operations without a framework-heavy runtime.
- One language allows one shared protocol package and one conformance suite.
- The current Python process architecture is being replaced, so retaining Python saves less work than it first appears.

Rust is the runner-up if future requirements prioritize compile-time memory and concurrency guarantees over delivery speed. TypeScript remains appropriate for the PWA only. Python parsing behavior can be preserved as golden tests and ported selectively.

Do not port the current SSH, UDP, mDNS, global-client-state, transcript scraping, Telegram, macOS, launch, termination, or power-control code into the new daemon boundary.

## Migration boundary

Retain as behavior, not architecture:

- prompt option detection and response mapping, after adding prompt hashes and golden tests;
- existing structured-output tests where they still describe desired PWA rendering;
- Herdr status labels and bounded pane-read behavior.

Replace:

- `relay/herdr_relay.py` transport, authentication, routing, process state, and host polling;
- `herdr-push` HTTP event ingestion;
- shared token and URL-token authentication;
- pane-ID-only routing;
- browser-owned connection credentials;
- direct command dispatch without results and preconditions.

## Verification plan

Core automated tests:

- envelope and fixture parsing;
- handshake negotiation and unsupported-major rejection;
- state snapshot/delta ordering, epoch mismatch, and resync;
- host/instance/terminal identity and pane-move routing;
- every operation's success, rejection, timeout, unknown outcome, and disconnect behavior;
- duplicate action IDs and no replay after reconnect;
- prompt parser golden cases and stale prompt rejection;
- certificate enrollment, rotation, revocation, and host scoping;
- bounded queues, output coalescing, frame limits, and redacted logs;
- server-new/connector-old and server-old/connector-new rolling compatibility;
- a fake Herdr socket implementing the protocol 16 read-only fixtures;
- a fake future Herdr socket implementing checked revision/content preconditions and atomic input.

Manual acceptance:

- connect real Herdr 0.7.3 through the local socket, verify status/output, and verify every write is rejected as incompatible;
- move an active agent between workspaces and verify terminal identity is preserved;
- with a Herdr build that implements `checked_input.v1`, approve, reject, send a prompt, send special keys, and interrupt from a mobile browser;
- race terminal output against reads and verify each returned text sample carries the revision sampled atomically with it;
- with that checked-input build, disconnect the network during each write phase and verify no action is replayed;
- restart Herdr, connector, and control plane independently and verify full resync;
- confirm audit storage and logs contain no prompt, output, text, or key values.

The checked fixture at `tests/fixtures/connector_protocol_v1.ndjson` exercises the handshake, snapshot, prompt, output, read-only write rejection, and delta envelopes. `connector_protocol_v1_operations.json` specifies future checked-input success, pre-execution failure, pre/post-receipt timeout, and disconnect outcomes for all six operations. `herdr_protocol_16.ndjson` records sanitized request/response shapes from the read-only Herdr spike. `protocol/connector-v1-action.schema.json` is the normative action schema. Full interaction remains blocked on the upstream checked atomic input capability.

## References

- Herdr socket API: <https://herdr.dev/docs/socket-api/>
- Herdr agent model: <https://herdr.dev/docs/agents/>
- Herdr source: <https://github.com/ogulcancelik/herdr>
- Existing push plugin: <https://github.com/dcolinmorgan/herdr-push>
