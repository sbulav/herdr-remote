import { useCallback, useEffect, useMemo, useReducer, useRef } from 'react';
import { makeMessage, uuidv7 } from '../protocol/messages';
import type { KeyName, Operation, OutputSource, Target } from '../protocol/types';
import {
  browserReducer,
  initialBrowserState,
  locateInstance,
  targetKey,
  type BrowserState,
  type ResyncReason,
} from '../state/reducer';
import { BrowserSocket } from './browserSocket';

export interface ControlPlane {
  state: BrowserState;
  subscribe(target: Target, source?: OutputSource): string | null;
  unsubscribe(subscriptionId: string): void;
  act(target: Target, operation: Operation): string | null;
  sendKey(target: Target, key: KeyName): string | null;
  requestResync(reason?: ResyncReason): void;
  stop(): void;
}

export function useControlPlane(enabled: boolean, onUnauthorized?: () => void, revalidateSession?: () => Promise<boolean>): ControlPlane {
  const [state, dispatch] = useReducer(browserReducer, initialBrowserState);
  const stateRef = useRef(state);
  stateRef.current = state;
  const socketRef = useRef<BrowserSocket | null>(null);
  const unauthorizedRef = useRef(onUnauthorized);
  unauthorizedRef.current = onUnauthorized;
  const revalidateRef = useRef(revalidateSession);
  revalidateRef.current = revalidateSession;
  const transition = useCallback((event: Parameters<typeof browserReducer>[1]) => {
    stateRef.current = browserReducer(stateRef.current, event);
    dispatch(event);
    return stateRef.current;
  }, []);

  useEffect(() => {
    if (!enabled) {
      socketRef.current?.stop();
      socketRef.current = null;
      transition({ type: 'session.unauthorized' });
      return;
    }
    const socket = new BrowserSocket({
      onConnecting: (reconnect) => transition({ type: 'socket.connecting', reconnect }),
      onOpen: () => transition({ type: 'socket.open' }),
      onMessage: (message) => {
        const next = transition({ type: 'message.received', message });
        if (message.type === 'protocol.error' && message.body.code === 'UNAUTHORIZED') {
          socket.stop();
          unauthorizedRef.current?.();
        }
        return !next.snapshotRejected;
      },
      onClose: (offline) => transition({ type: 'socket.closed', offline }),
      onUnauthorized: () => {
        transition({ type: 'session.unauthorized' });
        unauthorizedRef.current?.();
      },
      revalidateSession: () => revalidateRef.current?.() ?? Promise.resolve(false),
      onProtocolFailure: () => transition({ type: 'resync.requested', reason: 'epoch_mismatch' }),
    });
    socketRef.current = socket;
    socket.start();
    return () => {
      socket.stop();
      socketRef.current = null;
    };
  }, [enabled, transition]);

  useEffect(() => {
    const reason = state.needsResync;
    if (!reason || !state.sessionId) return;
    const message = makeMessage('state.resync', {
      session_id: state.sessionId,
      expected_epoch: state.stateEpoch,
      expected_sequence: state.sequence === null ? null : state.sequence + 1,
      reason,
    });
    if (socketRef.current?.send(message)) transition({ type: 'resync.sent' });
  }, [state.needsResync, state.sequence, state.sessionId, state.stateEpoch, transition]);

  useEffect(() => {
    const refresh = () => socketRef.current?.refresh();
    window.addEventListener('herdr:push-refresh', refresh);
    return () => window.removeEventListener('herdr:push-refresh', refresh);
  }, []);

  const subscribe = useCallback((target: Target, source: OutputSource = 'recent') => {
    const current = stateRef.current;
    const instance = locateInstance(current.hosts, target);
    if (
      current.stale ||
      !current.sessionId ||
      !instance?.capabilities.includes('output.subscribe.v1') ||
      !instance.agents.some((agent) => agent.terminal_id === target.terminal_id)
    ) {
      return null;
    }
    const subscriptionId = uuidv7();
    const message = makeMessage('output.subscribe', {
      session_id: current.sessionId,
      subscription_id: subscriptionId,
      target,
      source,
      lines: 200,
      poll_interval_ms: 1500,
    });
    if (!socketRef.current?.send(message)) return null;
    transition({ type: 'subscription.started', message });
    return subscriptionId;
  }, [transition]);

  const unsubscribe = useCallback((subscriptionId: string) => {
    const current = stateRef.current;
    if (current.sessionId) {
      const message = makeMessage('output.unsubscribe', { session_id: current.sessionId, subscription_id: subscriptionId });
      socketRef.current?.send(message);
    }
    transition({ type: 'subscription.ended', subscriptionId });
  }, [transition]);

  const act = useCallback((target: Target, operation: Operation) => {
    const current = stateRef.current;
    const instance = locateInstance(current.hosts, target);
    const agent = instance?.agents.find((candidate) => candidate.terminal_id === target.terminal_id);
    if (!current.sessionId || !current.stateEpoch || current.stale || !instance || !agent) return null;
    const isRead = operation.type === 'agent.read';
    const isPrompt = operation.type === 'prompt.respond';
    if (isRead && !instance.capabilities.includes('read.v1')) return null;
    if (!isRead && !instance.capabilities.includes('checked_input.v1')) return null;
    if (isPrompt && !instance.capabilities.includes('prompt.respond.v1')) return null;
    if (!isRead && agent.herdr_input_revision === 0) return null;
    const prompt = current.prompts[targetKey(target)];
    if (isPrompt && !prompt) return null;
    const actionId = uuidv7();
    const expected = {
      state_epoch: current.stateEpoch,
      connector_epoch: agent.connector_epoch,
      agent_generation: agent.agent_generation,
      herdr_input_revision: agent.herdr_input_revision,
      agent: agent.agent,
      statuses: [agent.status],
      ...(isPrompt && prompt
        ? { prompt_fingerprint: prompt.fingerprint, herdr_content_hash: prompt.herdr_content_hash }
        : {}),
    };
    const message = makeMessage('action.request', {
      session_id: current.sessionId,
      action_id: actionId,
      target,
      timeout_ms: isRead ? 5000 : 3000,
      expected,
      operation,
    });
    if (!socketRef.current?.send(message)) return null;
    transition({ type: 'action.sent', message });
    return actionId;
  }, [transition]);

  const sendKey = useCallback((target: Target, key: KeyName) => act(target, { type: 'agent.send_keys', keys: [key] }), [act]);
  const requestResync = useCallback((reason: ResyncReason = 'operator_refresh') => {
    transition({ type: 'resync.requested', reason });
  }, [transition]);
  const stop = useCallback(() => {
    socketRef.current?.stop();
    socketRef.current = null;
    transition({ type: 'session.unauthorized' });
  }, [transition]);

  return useMemo(
    () => ({ state, subscribe, unsubscribe, act, sendKey, requestResync, stop }),
    [act, requestResync, sendKey, state, stop, subscribe, unsubscribe],
  );
}
