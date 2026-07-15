# Browser protocol v1

Status: proposed enterprise v1 foundation. This protocol is not implemented.

The browser protocol is the same-origin WebSocket contract between the control plane and the PWA. The browser never connects to a host connector. The connector contract is documented separately in [Connector protocol v1](connector-protocol-v1.md).

The normative schema is [`protocol/browser-v1.schema.json`](../protocol/browser-v1.schema.json). It uses JSON Schema draft 2020-12. The schema takes precedence over examples and rejects unknown fields at every envelope, body, and tagged operation boundary.

## Security and session boundary

The reverse proxy and control plane authenticate the operator before upgrading the browser WebSocket.

- Use a secure, HTTP-only, same-site OIDC session cookie. The PWA does not read it.
- Accept one configured OIDC issuer, audience, and subject, with the MFA and session limits from the connector design.
- Require an exact same-origin `Origin` value during the WebSocket upgrade.
- Revalidate authorization before accepting each action request.
- Do not put cookies, bearer tokens, shared tokens, credentials, or identity headers in a protocol frame or URL query.
- Do not use `session_id` as a credential. It is a UUIDv7 correlation value for one browser WebSocket lifetime.

The endpoint is an authenticated same-origin URL such as `/v1/browser/ws`. It uses one JSON object per WebSocket text frame. V1 accepts only protocol major `1`; it has no bootstrap negotiation and no backward compatibility layer. Per-message compression is disabled because prompt and terminal content may share a connection with attacker-controlled strings. The maximum decoded frame size is 256 KiB.

If authorization expires, the server stops accepting requests and closes the socket. It may first send `protocol.error` with code `UNAUTHORIZED`. Reconnecting requires a valid OIDC session and creates a new browser protocol session.

## Common envelope

Every valid message has five fields:

```json
{
  "protocol": 1,
  "message_id": "019f64ca-3000-7000-8000-000000000001",
  "type": "session.snapshot",
  "sent_at": "2026-07-15T08:00:00Z",
  "body": {}
}
```

`message_id` is a lowercase UUIDv7 unique to the sender. `sent_at` is a UTC RFC 3339 timestamp used for diagnostics, not ordering. The message `type` selects the exact body schema.

All protocol-generated identifiers are lowercase UUIDv7 values. Herdr's `instance_id` and `terminal_id` remain bounded opaque strings because they come from a different identity domain.

Message directions are fixed:

| Type | Direction |
| --- | --- |
| `session.snapshot` | Control plane to PWA |
| `state.delta` | Control plane to PWA |
| `state.resync` | PWA to control plane |
| `prompt.snapshot` | Control plane to PWA |
| `output.subscribe` | PWA to control plane |
| `output.unsubscribe` | PWA to control plane |
| `output.snapshot` | Control plane to PWA |
| `action.request` | PWA to control plane |
| `action.received` | Control plane to PWA |
| `action.result` | Control plane to PWA |
| `protocol.error` | Control plane to PWA |

## Browser-safe state

The PWA receives only fields required to identify, display, and safely control an agent. It does not receive connector certificate data, connector connection IDs, pane IDs, workspace IDs, tab IDs, local paths, credentials, or audit identities.

The global action target is unchanged from the connector protocol:

```json
{
  "host_id": "019f64ca-1000-7000-8000-000000000002",
  "instance_id": "default",
  "terminal_id": "term_656a094363b4b4"
}
```

The target is exact. A pane or workspace route is mutable and is never a browser routing key.

Project values are optional redacted labels, not filesystem paths. All display strings are untrusted text. The PWA must insert them as text, never as HTML.

### Browser and connector epochs

`state_epoch` and `connector_epoch` are separate identity domains. Both are UUIDv7 values:

- `state_epoch` identifies one browser projection of all authorized control-plane state.
- `connector_epoch` is copied from one connector instance state stream. It identifies one reconciliation lifetime for an exact host and Herdr instance.

The control plane maps connector `(host_id, instance_id, epoch)` to browser `(host_id, instance_id, connector_epoch)` without rewriting the connector epoch. Every projected instance and agent carries it. Prompt snapshots, output snapshots, successful reads, and action preconditions carry the same exact value.

An `agent_generation` is scoped to `(host_id, instance_id, connector_epoch, terminal_id)`. It is never browser-global and is never compared across connector epochs. Within one scope, the control plane must reject or resync on a generation regression instead of publishing it.

## Snapshot and delta rules

### `session.snapshot`

The first application message on a new socket is a complete `session.snapshot`. It contains:

- the browser protocol `session_id`;
- a fresh `state_epoch`;
- `sequence: 0`;
- server time;
- all authorized hosts, configured Herdr instances, connector epochs, instance capabilities, and current agents.

Each agent record repeats its instance `connector_epoch` and carries the exact connector-assigned `agent_generation` and Herdr-issued `herdr_input_revision`. A zero Herdr revision means reads only. It can never authorize a write.

Host IDs must be unique in the snapshot. Instance IDs must be unique within a host, and terminal IDs must be unique within an instance. JSON Schema `uniqueItems` rejects identical objects but cannot enforce key uniqueness when other fields differ. The control plane and PWA validator must enforce these logical keys before accepting a snapshot.

The PWA replaces its entire state cache with the snapshot. It must not merge a snapshot with state from an older session or epoch.

### `state.delta`

Each delta carries the current `session_id`, `state_epoch`, and a sequence exactly one greater than the last accepted sequence. Changes are tagged host, instance, or agent upserts and removals.

An agent upsert contains the complete browser-safe mutable agent state. Its `connector_epoch` must equal the current instance connector epoch. Its `agent_generation` is the exact connector generation, not a browser counter. The control plane preserves the epoch, generation, and associated Herdr input revision without translating them.

A connector epoch change is never projected as an ordinary `instance.upsert`. The control plane sends an `instance.epoch_changed` delta with the exact previous and replacement connector epochs. The PWA accepts the sequence number, invalidates the browser projection, disables actions, and immediately sends `state.resync`. The next message from the control plane is a full snapshot with a new browser `state_epoch`. An agent generation may restart only under the new connector epoch.

The PWA applies a delta only when all of these conditions hold:

1. `session_id` equals the current WebSocket session.
2. `state_epoch` equals the accepted snapshot epoch.
3. `sequence` equals the last accepted sequence plus one.
4. Every agent epoch matches its instance epoch and its generation does not regress within that connector epoch.
5. Every removal names a currently known entity.

Any failure invalidates the cache. The PWA stops presenting actions as available and sends `state.resync`. It does not skip a delta or attempt local repair.

### `state.resync`

`state.resync` reports the current session, accepted epoch, next expected sequence, and one reason: `gap`, `epoch_mismatch`, `unknown_remove`, `connector_epoch_changed`, or `operator_refresh`. Unknown values are `null` when the PWA has no accepted snapshot.

The control plane answers with a complete `session.snapshot` using a new state epoch and sequence zero. It does not replay missed deltas. Deltas from the invalid epoch are discarded.

## Reconnect and no replay

A WebSocket reconnect creates a new `session_id`, a new state epoch, and a full snapshot. State deltas, prompt snapshots, output subscriptions, and `action.received` messages from the old socket are not replayed.

The PWA clears all transient prompt and output state when the socket closes. It never resends an action automatically, even if it did not receive `action.received`.

No replay depends on the server, not browser behavior. Before dispatch, the control plane inserts the action audit intent in a transaction protected by a unique constraint on `action_id`. A unique-constraint conflict returns `rejected/DUPLICATE_ACTION` without `action.received` or connector dispatch. The check covers the current browser session, later browser sessions, connector reconnects, and control-plane restarts.

The durable audit store retains the action ID and first-seen timestamp as a metadata-only deduplication tombstone after detailed audit retention expires. Reuse therefore cannot become valid because a session reconnects or an old audit detail row expires.

If the browser disconnects while an action has no browser-visible terminal result, the control plane finalizes it conservatively regardless of whether `action.received` was sent:

- an unresolved read becomes `failed/CONNECTION_LOST`;
- an unresolved write becomes `unknown/OUTCOME_UNKNOWN`, including a write that had not reached the receipt boundary;
- no action is dispatched again after either browser or connector reconnects.

Late connector completion cannot change this conservative browser-disconnect classification. It may be recorded as separate metadata for diagnosis, but it cannot cause replay or replace the terminal browser status.

After reconnect, the PWA may query `GET /api/v1/actions/<action_id>` on the same origin. The endpoint uses the OIDC cookie and returns the strict metadata-only `action_status_response` definition from the schema. It returns action ID, operation type, status, code, and timestamps. It never returns input, keys, prompts, output, or a successful read result. The operator must inspect a fresh snapshot and submit any intentional retry with a new UUIDv7 action ID.

## Capabilities and Herdr 0.7.3

An instance advertises only capabilities currently usable through its connector:

| Capability | Allows |
| --- | --- |
| `read.v1` | `agent.read` |
| `output.subscribe.v1` | Output subscriptions |
| `prompt.snapshot.v1` | Transient prompt snapshots |
| `checked_input.v1` | All write operations |
| `prompt.respond.v1` | `prompt.respond`, together with `checked_input.v1` |

The PWA hides or disables an operation when its capability is absent. The control plane still enforces capability gating; client rendering is not authorization. `prompt.respond.v1` is invalid unless `checked_input.v1` is also present.

Herdr 0.7.3 reports input revision `0` and does not support checked atomic input. Its browser instance is read-only. It may advertise `read.v1`, `output.subscribe.v1`, and `prompt.snapshot.v1`, but not `checked_input.v1` or `prompt.respond.v1`. Every projected 0.7.3 agent, prompt, and output record has `herdr_input_revision: 0`. Every write request is rejected before `action.received` with `HERDR_INCOMPATIBLE`. There is no unsafe fallback.

The JSON Schema enforces the exact `0.7.3` rule and the capability implication. Version ordering and future Herdr compatibility cannot be expressed robustly in JSON Schema. For every other version, the control plane must verify the connector's checked-input capability against the inspected Herdr API before projecting `checked_input.v1` or a nonzero revision as usable for writes.

## Prompt snapshots

`prompt.snapshot` binds a transient prompt view to:

- the exact host, instance, and terminal;
- state epoch and state sequence;
- connector epoch and connector-assigned agent generation;
- Herdr input revision;
- prompt fingerprint and Herdr content hash;
- adapter version and ordered option IDs.

Prompt normalization, hashing, and option mapping are identical to the connector protocol. Prompt option IDs must be logically unique within a snapshot. The excerpt and option labels are browser display content only.

The connector excerpt may be larger than the browser limit. The control plane projects it as follows:

1. Preserve `fingerprint` and `herdr_content_hash` byte-for-byte. Never recompute either hash from the projected excerpt.
2. Keep at most the first 2,048 Unicode scalar values of `excerpt`.
3. Set browser `excerpt_truncated` to connector `excerpt_truncated OR connector_excerpt_length > 2048`.

This truncation changes display content only. Prompt response preconditions continue to use the unchanged connector hashes.

The PWA replaces an older prompt for the same target. It removes the prompt when the agent leaves `blocked`, the generation changes without a matching new prompt, the epoch changes, or the socket closes. It does not store prompts in IndexedDB, Cache Storage, local storage, service-worker caches, logs, analytics, or error reports.

## Output subscriptions

Terminal output is opt-in, bounded, and transient.

The PWA sends `output.subscribe` with a new connection-scoped subscription UUIDv7, an exact target, source, line limit, and polling interval. The control plane checks `output.subscribe.v1`, limits active subscriptions, and forwards the subscription to the current connector connection.

`output.snapshot` is a complete replacement, not an append operation. It carries the browser state epoch, connector epoch, connector generation, Herdr input revision, exact text hash, bounded text, and truncation flag. The PWA replaces the displayed value only when the subscription, target, and all state bindings still match. Unchanged text is not resent.

The PWA sends `output.unsubscribe` when the view closes or becomes hidden. Unsubscribing an unknown ID is an idempotent no-op. All subscriptions end on browser or connector disconnect. The PWA creates new IDs after reconnect and never resumes an old subscription.

The control plane coalesces output under backpressure. It may drop intermediate snapshots because each snapshot is a complete replacement. It must not delay state or action messages to deliver output.

Neither the PWA nor the control plane persists output. The service worker must not cache the WebSocket data or copy terminal text into notification payloads.

## Action requests

Every action request contains:

- the current browser `session_id` and a new UUIDv7 `action_id`;
- the exact host, instance, and terminal target;
- a bounded relative timeout;
- exact browser state epoch, connector epoch, connector generation, Herdr input revision, agent label, and allowed statuses;
- the tagged operation payload.

`prompt.respond` also requires the exact prompt fingerprint and Herdr content hash. Those fields are forbidden for every other operation.

| Operation | Required capability | Maximum timeout | Payload |
| --- | --- | --- | --- |
| `agent.read` | `read.v1` | 5 seconds | source and 1 to 1,000 lines |
| `agent.send_text` | `checked_input.v1` | 3 seconds | bounded printable text, no implicit Enter |
| `agent.send_keys` | `checked_input.v1` | 3 seconds | 1 to 16 allowed key names |
| `agent.send_input` | `checked_input.v1` | 3 seconds | non-empty text, keys, or both |
| `agent.interrupt` | `checked_input.v1` | 3 seconds | no payload fields |
| `prompt.respond` | `checked_input.v1` and `prompt.respond.v1` | 3 seconds | bound option ID |

The schema defines the complete key allowlist and standard JSON Schema code-point limits. Input has at most 4,096 code points and therefore at most 16 KiB of UTF-8. Output has at most 32,768 code points and therefore at most 128 KiB. Prompt excerpts have at most 2,048 code points and therefore at most 8 KiB. No non-standard byte-limit extension is required. Newlines, tabs, escapes, and interrupts use key names rather than control characters in text. V1 defines no launch, terminate, host power, arbitrary shell, Telegram, or macOS operation.

The control plane validates the authenticated operator and browser session, transactionally inserts the durably unique action audit intent, and then validates target, capabilities, state preconditions, timeout, audit availability, and rate limits before dispatch. The connector repeats target, capability, and state checks immediately before local execution.

## Receipt and result semantics

An action has one of two browser-visible lifecycles:

```text
action.request -> action.result
action.request -> action.received -> action.result
```

`action.received` means final connector validation completed and execution started. A rejection or timeout before that boundary returns `action.result` directly.

The result status is:

- `succeeded`: Herdr acknowledged the operation;
- `rejected`: no operation was attempted because validation failed;
- `failed`: a read failed, or a write was definitively rejected before enqueue;
- `unknown`: a write may have been accepted but no definitive result is available.

A timeout after receipt is `failed/DEADLINE_EXCEEDED` for a read and `unknown/OUTCOME_UNKNOWN` for a write. Connector disconnect follows the same side-effect boundary, using `failed/CONNECTION_LOST` for a read.

Status and code combinations are closed:

- `unknown` is valid only for a write and only with `OUTCOME_UNKNOWN`;
- read timeout and disconnect use `failed/DEADLINE_EXCEEDED` and `failed/CONNECTION_LOST`;
- validation and precondition codes, including `STALE_STATE`, `PROMPT_CHANGED`, `HERDR_INCOMPATIBLE`, and `DUPLICATE_ACTION`, use `rejected`;
- a side-effect-free internal write failure before the checked local enqueue call uses `rejected/INTERNAL`;
- an internal read failure uses `failed/INTERNAL`;
- a definitive Herdr rejection before enqueue may use `failed/HERDR_REJECTED`;
- `succeeded` requires a null code and the operation-specific result.

For an active browser connection, a write becomes `unknown/OUTCOME_UNKNOWN` only after `action.received`. The conservative browser-disconnect rule above is the exception: any write still unresolved when that browser socket closes is recorded as unknown regardless of whether receipt reached the browser.

An `agent.read` success may contain bounded terminal text. Other successful writes return only acknowledgement metadata. Failed results contain a stable code and `result: null`. They have no free-form message or content field. The PWA maps stable codes to locally owned display strings.

## Protocol errors

Malformed frames and unknown fields receive `protocol.error` with `INVALID_MESSAGE`. Unknown message types receive `UNSUPPORTED_MESSAGE`. The response includes only the browser session ID, the request message ID when available, a stable code, and a fatal flag.

Protocol errors never include peer-provided text, prompt excerpts, terminal output, sent input, keys, credentials, or stack traces. Repeated malformed messages may close the socket. Valid action failures always use `action.result`.

## Push event semantics

Web Push wakes the PWA; it does not carry protocol state. A push payload contains only a random event ID and a coarse event kind such as `agent_state_changed`. It contains no host label, project, terminal ID, prompt, option, output, sent input, or action result.

After a notification interaction, the PWA opens its same-origin page and obtains current state through an authenticated WebSocket snapshot. Push events are hints and may be delayed, duplicated, or dropped. The PWA never applies a push payload as a state delta and never uses it to authorize or replay an action.

Push subscriptions are bound to the authenticated operator on a separate same-origin HTTP endpoint. They are not protocol credentials and never appear in browser WebSocket frames.

## PWA schema consumption

The PWA treats the checked-in schema as the source of truth:

1. Generate or maintain a discriminated TypeScript union from `protocol/browser-v1.schema.json` during development.
2. Validate every inbound frame at runtime before passing it to a reducer.
3. Reject unknown fields and unsupported types rather than retaining them.
4. Use an exhaustive switch on `type`.
5. Keep session, prompt, output, subscription, and pending-action state in memory only.
6. Enforce logical host, instance, terminal, and prompt-option uniqueness after schema validation.
7. Scope generations by connector epoch and reject a regression within one scope.
8. Clear transient state on browser or connector epoch change, resync, authorization failure, or disconnect.

Outbound messages use the same schema before transmission. A schema-valid request is not automatically authorized; the control plane enforces session, capability, precondition, audit, and rate-limit checks.

The conformance fixtures are:

- [`tests/fixtures/browser_protocol_v1.ndjson`](../tests/fixtures/browser_protocol_v1.ndjson) for a complete read-only lifecycle;
- [`tests/fixtures/browser_protocol_v1_failures.json`](../tests/fixtures/browser_protocol_v1_failures.json) for operation outcomes and recovery cases;
- [`tests/test_browser_protocol.py`](../tests/test_browser_protocol.py) for stdlib-only schema and semantic checks.

## Retention and observability

Prompt excerpts and terminal output exist only in bounded memory needed to deliver the live view. The control plane does not write them to databases, audit rows, logs, traces, metrics, crash reports, or backups.

Action audit records retain identifiers, operation type, timestamps, counts, status, and stable code. The durable action-ID unique constraint and metadata-only deduplication tombstone enforce no replay across reconnects. Audit records do not retain text, keys, prompt hashes or excerpts, terminal output, cwd paths, or credentials. Logs follow the same metadata-only rule.
