import { describe, expect, it } from 'vitest';
import lifecycle from '../../tests/fixtures/browser_protocol_v1.ndjson?raw';
import failures from '../../tests/fixtures/browser_protocol_v1_failures.json';
import type { ActionRequestMessage, InboundMessage, OutputSubscribeMessage } from '../src/protocol/types';
import { validateInbound, validateMessage } from '../src/protocol/validate';
import { browserReducer, initialBrowserState, targetKey } from '../src/state/reducer';

const raw = lifecycle.trim().split('\n').map((line) => JSON.parse(line) as unknown);
const inbound = (index: number) => validateInbound(raw[index]);
const anyMessage = <T,>(index: number) => validateMessage(raw[index]) as T;
const receive = (state: typeof initialBrowserState, message: InboundMessage) => browserReducer(state, { type: 'message.received', message });

describe('browser state reducer', () => {
  it('replaces snapshots, prompts, and bounded output', () => {
    let state = receive(initialBrowserState, inbound(0));
    state = receive(state, inbound(1));
    const subscription = anyMessage<OutputSubscribeMessage>(2);
    state = browserReducer(state, { type: 'subscription.started', message: subscription });
    state = receive(state, inbound(3));
    expect(state.stale).toBe(false);
    expect(Object.values(state.prompts)[0]?.excerpt).toContain('Permission required');
    expect(state.outputs[subscription.body.subscription_id]?.text).toContain('run tests');

    const replacement = structuredClone(inbound(3));
    if (replacement.type !== 'output.snapshot') throw new Error('fixture mismatch');
    replacement.body.text = 'complete replacement';
    state = receive(state, replacement);
    expect(state.outputs[subscription.body.subscription_id]?.text).toBe('complete replacement');

    state = receive(state, inbound(13));
    expect(state.stateEpoch).toBe('019f64ca-3000-7000-8000-000000000107');
    expect(state.prompts).toEqual({});
    expect(state.outputs).toEqual({});
  });

  it('invalidates gaps, unknown removals, regressions, and connector epoch changes', () => {
    const snapshot = receive(initialBrowserState, inbound(0));
    const gap = structuredClone(inbound(9));
    if (gap.type !== 'state.delta') throw new Error('fixture mismatch');
    gap.body.sequence = 2;
    expect(receive(snapshot, gap).needsResync).toBe('gap');

    const unknownRemove = structuredClone(inbound(9));
    if (unknownRemove.type !== 'state.delta') throw new Error('fixture mismatch');
    unknownRemove.body.changes = [{ operation: 'agent.remove', target: { host_id: '019f64ca-1000-7000-8000-000000000002', instance_id: 'default', terminal_id: 'missing' }, reason: 'reconciled' }];
    expect(receive(snapshot, unknownRemove).needsResync).toBe('unknown_remove');

    const regression = structuredClone(inbound(9));
    if (regression.type !== 'state.delta' || regression.body.changes[0]?.operation !== 'agent.upsert') throw new Error('fixture mismatch');
    regression.body.changes[0].agent.agent_generation = 0;
    expect(receive(snapshot, regression).needsResync).toBe('epoch_mismatch');

    let changed = receive(snapshot, inbound(9));
    changed = receive(changed, inbound(11));
    expect(changed.needsResync).toBe('connector_epoch_changed');
    expect(changed.sequence).toBe(2);
    expect(changed.stale).toBe(true);
  });

  it('tracks receipt/result and retains duplicate-action rejection', () => {
    let state = receive(initialBrowserState, inbound(0));
    const request = anyMessage<ActionRequestMessage>(4);
    state = browserReducer(state, { type: 'action.sent', message: request });
    state = receive(state, inbound(5));
    expect(state.pendingActions[request.body.action_id]?.phase).toBe('received');
    state = receive(state, inbound(6));
    expect(state.pendingActions).toEqual({});
    expect(state.actionResults[request.body.action_id]?.status).toBe('succeeded');

    const duplicateScenario = failures.scenarios.find((scenario) => scenario.name === 'same_session_duplicate_action');
    if (!duplicateScenario) throw new Error('missing fixture');
    const duplicateRequest = validateMessage(duplicateScenario.messages[0]) as ActionRequestMessage;
    const duplicateResult = validateInbound(duplicateScenario.messages[1]);
    state = browserReducer(state, { type: 'action.sent', message: duplicateRequest });
    state = receive(state, duplicateResult);
    expect(state.actionResults[duplicateRequest.body.action_id]?.code).toBe('DUPLICATE_ACTION');
    expect(state.pendingActions[duplicateRequest.body.action_id]).toBeUndefined();
  });

  it('never replays and conservatively resolves disconnected writes as unknown', () => {
    let state = receive(initialBrowserState, inbound(0));
    const writeScenario = failures.scenarios.find((scenario) => scenario.name === 'disconnect_unknown_write');
    if (!writeScenario) throw new Error('missing fixture');
    const write = validateMessage(writeScenario.messages[0]) as ActionRequestMessage;
    const read = anyMessage<ActionRequestMessage>(4);
    state = browserReducer(state, { type: 'action.sent', message: write });
    state = browserReducer(state, { type: 'action.sent', message: read });
    state = browserReducer(state, { type: 'socket.closed', offline: false });
    expect(state.pendingActions).toEqual({});
    expect(state.actionResults[write.body.action_id]?.status).toBe('unknown');
    expect(state.actionResults[write.body.action_id]?.code).toBe('OUTCOME_UNKNOWN');
    expect(state.actionResults[read.body.action_id]?.code).toBe('CONNECTION_LOST');
    expect(state.sessionId).toBeNull();
  });

  it('clears prompts after generation changes and marks stale results for resync', () => {
    let state = receive(initialBrowserState, inbound(0));
    state = receive(state, inbound(1));
    expect(state.prompts[targetKey((inbound(1) as Extract<InboundMessage, { type: 'prompt.snapshot' }>).body.target)]).toBeDefined();
    state = receive(state, inbound(9));
    expect(state.prompts).toEqual({});

    const scenario = failures.scenarios.find((candidate) => candidate.name === 'stale_state');
    if (!scenario) throw new Error('missing fixture');
    const request = validateMessage(scenario.messages[0]) as ActionRequestMessage;
    state = browserReducer(state, { type: 'action.sent', message: request });
    state = receive(state, validateInbound(scenario.messages[1]));
    expect(state.stale).toBe(true);
    expect(state.needsResync).toBe('operator_refresh');
  });

  it('invalidates the actionable session before reconnecting and classifies every unresolved action', () => {
    let state = receive(initialBrowserState, inbound(0));
    const read = anyMessage<ActionRequestMessage>(4);
    const writeScenario = failures.scenarios.find((scenario) => scenario.name === 'disconnect_unknown_write');
    if (!writeScenario) throw new Error('missing fixture');
    const write = validateMessage(writeScenario.messages[0]) as ActionRequestMessage;
    state = browserReducer(state, { type: 'action.sent', message: read });
    state = browserReducer(state, { type: 'action.sent', message: write });

    state = browserReducer(state, { type: 'socket.connecting', reconnect: true });

    expect(state.stale).toBe(true);
    expect(state.sessionId).toBeNull();
    expect(state.pendingActions).toEqual({});
    expect(state.actionResults[read.body.action_id]).toMatchObject({ status: 'failed', code: 'CONNECTION_LOST' });
    expect(state.actionResults[write.body.action_id]).toMatchObject({ status: 'unknown', code: 'OUTCOME_UNKNOWN' });
  });

  it('preserves unresolved actions across a same-socket resync and accepts their later results', () => {
    let state = receive(initialBrowserState, inbound(0));
    const read = anyMessage<ActionRequestMessage>(4);
    const writeScenario = failures.scenarios.find((scenario) => scenario.name === 'disconnect_unknown_write');
    if (!writeScenario) throw new Error('missing fixture');
    const write = validateMessage(writeScenario.messages[0]) as ActionRequestMessage;
    state = browserReducer(state, { type: 'action.sent', message: read });
    state = browserReducer(state, { type: 'action.sent', message: write });

    const resnapshot = structuredClone(inbound(13));
    if (resnapshot.type !== 'session.snapshot') throw new Error('fixture mismatch');
    resnapshot.body.session_id = state.sessionId!;
    state = receive(state, resnapshot);

    expect(state.pendingActions).toHaveProperty(read.body.action_id);
    expect(state.pendingActions).toHaveProperty(write.body.action_id);
    expect(state.actionResults[read.body.action_id]).toBeUndefined();
    expect(state.actionResults[write.body.action_id]).toBeUndefined();

    const writeResult = validateInbound(writeScenario.messages[2]);
    state = receive(state, writeResult);
    expect(state.pendingActions[write.body.action_id]).toBeUndefined();
    expect(state.actionResults[write.body.action_id]).toMatchObject({ status: 'unknown', code: 'OUTCOME_UNKNOWN', local: false });

    state = receive(state, inbound(6));
    expect(state.pendingActions[read.body.action_id]).toBeUndefined();
    expect(state.actionResults[read.body.action_id]).toMatchObject({ code: 'STALE_STATE', result: null });
  });

  it('rejects changed sessions and reused state epochs on same-socket snapshots', () => {
    let state = receive(initialBrowserState, inbound(0));
    const read = anyMessage<ActionRequestMessage>(4);
    state = browserReducer(state, { type: 'action.sent', message: read });

    const changedSession = structuredClone(inbound(13));
    if (changedSession.type !== 'session.snapshot') throw new Error('fixture mismatch');
    changedSession.body.session_id = '019f64ca-3000-7000-8000-000000000199';
    const rejectedSession = receive(state, changedSession);
    expect(rejectedSession.snapshotRejected).toBe(true);
    expect(rejectedSession.sessionId).toBe(state.sessionId);
    expect(rejectedSession.pendingActions).toHaveProperty(read.body.action_id);

    const reusedCurrentEpoch = structuredClone(inbound(0));
    if (reusedCurrentEpoch.type !== 'session.snapshot') throw new Error('fixture mismatch');
    const rejectedCurrentEpoch = receive(state, reusedCurrentEpoch);
    expect(rejectedCurrentEpoch.snapshotRejected).toBe(true);
    expect(rejectedCurrentEpoch.pendingActions).toHaveProperty(read.body.action_id);

    const fresh = structuredClone(inbound(13));
    if (fresh.type !== 'session.snapshot') throw new Error('fixture mismatch');
    fresh.body.session_id = state.sessionId!;
    state = receive(state, fresh);
    expect(state.snapshotRejected).toBe(false);
    expect(state.pendingActions).toHaveProperty(read.body.action_id);

    const reusedOldEpoch = structuredClone(fresh);
    reusedOldEpoch.body.state_epoch = '019f64ca-3000-7000-8000-000000000103';
    expect(receive(state, reusedOldEpoch).snapshotRejected).toBe(true);
  });

  it('retains generation watermarks across removal and same-connector resync', () => {
    let state = receive(initialBrowserState, inbound(0));
    state = receive(state, inbound(9));

    const remove = structuredClone(inbound(9));
    if (remove.type !== 'state.delta') throw new Error('fixture mismatch');
    remove.body.sequence = 2;
    remove.body.changes = [{
      operation: 'agent.remove',
      target: { host_id: '019f64ca-1000-7000-8000-000000000002', instance_id: 'default', terminal_id: 'term_656a094363b4b4' },
      reason: 'reconciled',
    }];
    state = receive(state, remove);

    const readd = structuredClone(inbound(9));
    if (readd.type !== 'state.delta' || readd.body.changes[0]?.operation !== 'agent.upsert') throw new Error('fixture mismatch');
    readd.body.sequence = 3;
    readd.body.changes[0].agent.agent_generation = 1;
    expect(receive(state, readd).needsResync).toBe('epoch_mismatch');

    const regressedSnapshot = structuredClone(inbound(0));
    if (regressedSnapshot.type !== 'session.snapshot') throw new Error('fixture mismatch');
    regressedSnapshot.body.state_epoch = '019f64ca-3000-7000-8000-000000000130';
    const rejected = receive(state, regressedSnapshot);
    expect(rejected.snapshotRejected).toBe(true);
    expect(rejected.stale).toBe(true);

    const newEpochSnapshot = structuredClone(inbound(13));
    expect(receive(state, newEpochSnapshot).snapshotRejected).toBe(false);
  });

  it('rejects stale successful read content instead of displaying it', () => {
    let state = receive(initialBrowserState, inbound(0));
    const read = anyMessage<ActionRequestMessage>(4);
    state = browserReducer(state, { type: 'action.sent', message: read });
    const staleRead = structuredClone(inbound(6));
    if (staleRead.type !== 'action.result' || !staleRead.body.result || !('agent_generation' in staleRead.body.result)) {
      throw new Error('fixture mismatch');
    }
    staleRead.body.result.agent_generation = 2;
    state = receive(state, staleRead);
    expect(state.actionResults[read.body.action_id]).toMatchObject({ status: 'rejected', code: 'STALE_STATE', result: null });
    expect(state.stale).toBe(true);
  });

  it('keeps a newer prompt when an older prompt response succeeds late', () => {
    let state = receive(initialBrowserState, inbound(0));
    state = receive(state, inbound(1));
    const request = anyMessage<ActionRequestMessage>(7);
    state = browserReducer(state, { type: 'action.sent', message: request });

    const newerPrompt = structuredClone(inbound(1));
    if (newerPrompt.type !== 'prompt.snapshot') throw new Error('fixture mismatch');
    newerPrompt.body.fingerprint = `sha256:${'1'.repeat(64)}`;
    newerPrompt.body.herdr_content_hash = `sha256:${'2'.repeat(64)}`;
    newerPrompt.body.excerpt = 'A newer prompt';
    state = receive(state, newerPrompt);

    const success = structuredClone(inbound(8));
    if (success.type !== 'action.result') throw new Error('fixture mismatch');
    success.body.status = 'succeeded';
    success.body.code = null;
    success.body.result = { herdr_acknowledged: true, option_id: 'allow_once' };
    state = receive(state, success);

    expect(state.pendingActions[request.body.action_id]).toBeUndefined();
    expect(state.actionResults[request.body.action_id]?.status).toBe('succeeded');
    expect(state.prompts[targetKey(newerPrompt.body.target)]).toMatchObject({
      fingerprint: newerPrompt.body.fingerprint,
      herdr_content_hash: newerPrompt.body.herdr_content_hash,
      excerpt: 'A newer prompt',
    });
  });
});
