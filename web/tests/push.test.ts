import { describe, expect, it, vi } from 'vitest';
import { EventDeduplicator, parsePushPayload } from '../src/push/payload';
import { applicationServerKeyMatches, reconcilePush, subscribePush, synchronizeCurrentPushSubscription, testPush, unsubscribePush } from '../src/api/push';
import { MAX_PENDING_PUSH_SOURCES, PendingReplacementOverflowError } from '../src/push/pendingReplacement';
import fixture from '../../tests/fixtures/http_api_v1.json';
import { httpValidator, schema } from './http-schema';

function memoryPendingReplacement(initial: string[] = [], optedOut = false) {
  let state = { source_endpoints: [...initial], opted_out: optedOut };
  const clone = () => structuredClone(state);
  return {
    load: vi.fn(async () => clone()),
    append: vi.fn(async (sourceEndpoint: string) => {
      if (state.source_endpoints.includes(sourceEndpoint)) return;
      if (state.source_endpoints.length >= MAX_PENDING_PUSH_SOURCES) throw new PendingReplacementOverflowError();
      state.source_endpoints.push(sourceEndpoint);
    }),
    remove: vi.fn(async (sourceEndpoints: readonly string[]) => {
      const submitted = new Set(sourceEndpoints);
      state.source_endpoints = state.source_endpoints.filter((endpoint) => !submitted.has(endpoint));
    }),
    setOptedOut: vi.fn(async (value: boolean) => { state.opted_out = value; }),
    state: () => clone(),
  };
}

function subscription(endpoint: string, key: number, unsubscribe = vi.fn().mockResolvedValue(true)): PushSubscription {
  return {
    endpoint,
    options: { applicationServerKey: new Uint8Array([key]).buffer },
    unsubscribe,
    toJSON: () => ({ endpoint, keys: { p256dh: `${endpoint}-key`, auth: `${endpoint}-auth` } }),
  } as unknown as PushSubscription;
}

function disablePushSynchronization(): () => void {
  const descriptor = Object.getOwnPropertyDescriptor(navigator, 'locks');
  Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined });
  return () => {
    if (descriptor) Object.defineProperty(navigator, 'locks', descriptor);
  };
}

describe('service-worker push payload handling', () => {
  const valid = { event_id: '019f64ca-3000-7000-8000-000000000001', kind: 'agent_state_changed' };

  it('accepts only generic exact payloads', () => {
    expect(parsePushPayload(valid)).toEqual(valid);
    expect(parsePushPayload({ ...valid, kind: 'test' })).toEqual({ ...valid, kind: 'test' });
    expect(parsePushPayload({ ...valid, terminal_id: 'secret' })).toBeNull();
    expect(parsePushPayload({ ...valid, kind: 'terminal_output' })).toBeNull();
    expect(parsePushPayload({ ...valid, event_id: 'not-a-uuid' })).toBeNull();
  });

  it('deduplicates event IDs with bounded memory', () => {
    const dedupe = new EventDeduplicator(2);
    expect(dedupe.accept('one')).toBe(true);
    expect(dedupe.accept('one')).toBe(false);
    expect(dedupe.accept('two')).toBe(true);
    expect(dedupe.accept('three')).toBe(true);
    expect(dedupe.accept('one')).toBe(true);
  });

  it('registers a changed subscription using CSRF and metadata only', async () => {
    const local = subscription('https://push.example/random', 2);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;

    await synchronizeCurrentPushSubscription(registration, local, 'Ag', memoryPendingReplacement(), { createIfMissing: false, registerIfUnsynchronized: true });

    const [path, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(path).toBe(schema['x-endpoints'].push_register.path);
    expect(init.method).toBe(schema['x-endpoints'].push_register.method);
    expect(httpValidator('#/$defs/pushSubscription')(JSON.parse(String(init.body)))).toBe(true);
    expect(init.body).not.toMatch(/terminal|prompt|output|input/i);
  });

  it('reconciles only the current local endpoint without exposing other devices', async () => {
    const local = subscription('https://push.example/current-device', 1);
    const reconciliationResponse = { subscribed: false };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf-1' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify(reconciliationResponse), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf-2' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;

    await expect(reconcilePush(registration, 'AQ', memoryPendingReplacement())).resolves.toEqual({ subscribed: true });

    expect(fetchMock.mock.calls[1]?.[0]).toBe(schema['x-endpoints'].push_reconcile.path);
    expect(fetchMock.mock.calls[1]?.[1]?.body).toBe(JSON.stringify({ endpoint: local.endpoint }));
    expect(httpValidator('#/$defs/endpointRequest')(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body)))).toBe(true);
    expect(fetchMock.mock.calls[3]?.[0]).toBe(schema['x-endpoints'].push_register.path);
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'DELETE')).toBe(false);
  });

  it('persists the source before rotating a VAPID key', async () => {
    const unsubscribeOld = vi.fn().mockResolvedValue(true);
    const oldSubscription = subscription('https://push.example/old', 1, unsubscribeOld);
    const replacement = subscription('https://push.example/new', 2);
    const pending = memoryPendingReplacement();
    const subscribe = vi.fn().mockResolvedValue(replacement);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(oldSubscription), subscribe } } as unknown as ServiceWorkerRegistration;
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);

    expect(applicationServerKeyMatches(oldSubscription, 'Ag')).toBe(false);
    await expect(reconcilePush(registration, 'Ag', pending)).resolves.toEqual({ subscribed: true });

    expect(pending.append).toHaveBeenCalledWith(oldSubscription.endpoint);
    expect(pending.append.mock.invocationCallOrder[0]!).toBeLessThan(unsubscribeOld.mock.invocationCallOrder[0]!);
    const body = JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body));
    expect(body.source_endpoints).toEqual([oldSubscription.endpoint]);
    expect(body.subscription.endpoint).toBe(replacement.endpoint);
    expect(httpValidator('#/$defs/replaceRequest')(body)).toBe(true);
    expect(pending.remove).toHaveBeenCalledWith([oldSubscription.endpoint]);
  });

  it('settles a same-endpoint refresh without unsubscribing it', async () => {
    const endpoint = 'https://push.example/same';
    const local = subscription(endpoint, 2);
    const pending = memoryPendingReplacement([endpoint]);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;

    await synchronizeCurrentPushSubscription(registration, local, 'Ag', pending);

    expect(local.unsubscribe).not.toHaveBeenCalled();
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body)).source_endpoints).toEqual([endpoint]);
    expect(pending.state().source_endpoints).toEqual([]);
  });

  it('durably opts out and deletes current plus failed-rotation sources', async () => {
    const local = subscription('https://push.example/current-device', 1);
    const pending = memoryPendingReplacement(['https://push.example/failed-rotation-old']);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;

    await unsubscribePush(registration, pending);

    expect(fetchMock.mock.calls[1]?.[0]).toBe(schema['x-endpoints'].push_delete.path);
    expect(fetchMock.mock.calls[1]?.[1]?.body).toBe(JSON.stringify({ endpoints: ['https://push.example/failed-rotation-old', local.endpoint] }));
    expect(pending.setOptedOut.mock.invocationCallOrder[0]!).toBeLessThan(fetchMock.mock.invocationCallOrder[0]!);
    expect(local.unsubscribe).toHaveBeenCalledOnce();
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });

  it('uses persisted endpoint sources to turn off push without Web Locks', async () => {
    const restoreSynchronization = disablePushSynchronization();
    const localUnsubscribe = vi.fn().mockResolvedValue(true);
    const local = subscription('https://push.example/no-lock-current', 1, localUnsubscribe);
    const pending = memoryPendingReplacement(['https://push.example/no-lock-old']);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;

    try {
      await unsubscribePush(registration, pending);
    } finally {
      restoreSynchronization();
    }

    expect(pending.setOptedOut).toHaveBeenCalledWith(true);
    expect(pending.append).toHaveBeenCalledWith(local.endpoint);
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body))).toEqual({ endpoints: ['https://push.example/no-lock-old', local.endpoint] });
    expect(local.unsubscribe).toHaveBeenCalledOnce();
    expect(pending.setOptedOut.mock.invocationCallOrder[0]!).toBeLessThan(pending.append.mock.invocationCallOrder[0]!);
    expect(pending.append.mock.invocationCallOrder[0]!).toBeLessThan(fetchMock.mock.invocationCallOrder[1]!);
    expect(fetchMock.mock.invocationCallOrder[1]!).toBeLessThan(localUnsubscribe.mock.invocationCallOrder[0]!);
    expect(localUnsubscribe.mock.invocationCallOrder[0]!).toBeLessThan(pending.remove.mock.invocationCallOrder[0]!);
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });

  it('keeps no-lock opt-out sources after delete failure and later cleans them under a lock', async () => {
    const restoreSynchronization = disablePushSynchronization();
    const local = subscription('https://push.example/no-lock-current', 2);
    let current: PushSubscription | null = local;
    const localUnsubscribe = vi.fn(async () => { current = null; return true; });
    local.unsubscribe = localUnsubscribe;
    const subscribe = vi.fn();
    const pending = memoryPendingReplacement(['https://push.example/no-lock-old']);
    const registration = { pushManager: { getSubscription: vi.fn(async () => current), subscribe } } as unknown as ServiceWorkerRegistration;
    const failedFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('unavailable', { status: 503 }));
    vi.stubGlobal('fetch', failedFetch);

    try {
      await expect(unsubscribePush(registration, pending)).rejects.toThrow('503');
    } finally {
      restoreSynchronization();
    }

    expect(local.unsubscribe).toHaveBeenCalledOnce();
    expect(pending.append.mock.invocationCallOrder[0]!).toBeLessThan(localUnsubscribe.mock.invocationCallOrder[0]!);
    expect(pending.remove).not.toHaveBeenCalled();
    expect(pending.state()).toEqual({
      source_endpoints: ['https://push.example/no-lock-old', local.endpoint],
      opted_out: true,
    });

    const recoveredFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', recoveredFetch);
    await expect(reconcilePush(registration, 'Ag', pending)).resolves.toEqual({ subscribed: false });

    expect(JSON.parse(String(recoveredFetch.mock.calls[1]?.[1]?.body))).toEqual({
      endpoints: ['https://push.example/no-lock-old', local.endpoint],
    });
    expect(subscribe).not.toHaveBeenCalled();
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });

  it('cleans pending opt-out sources without a local subscription or Web Locks', async () => {
    const restoreSynchronization = disablePushSynchronization();
    const pending = memoryPendingReplacement(['https://push.example/no-lock-pending']);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(null) } } as unknown as ServiceWorkerRegistration;

    try {
      await unsubscribePush(registration, pending);
    } finally {
      restoreSynchronization();
    }

    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body))).toEqual({ endpoints: ['https://push.example/no-lock-pending'] });
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });

  it('deletes a full source list plus the current endpoint before unsubscribing without Web Locks', async () => {
    const restoreSynchronization = disablePushSynchronization();
    const sources = Array.from({ length: MAX_PENDING_PUSH_SOURCES }, (_, index) => `https://push.example/full-${index}`);
    const local = subscription('https://push.example/full-current', 1);
    const pending = memoryPendingReplacement(sources);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;

    try {
      await unsubscribePush(registration, pending);
    } finally {
      restoreSynchronization();
    }

    const body = JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body));
    expect(body).toEqual({ endpoints: [...sources, local.endpoint] });
    expect(httpValidator('#/$defs/endpointsRequest')(body)).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(local.unsubscribe).toHaveBeenCalledOnce();
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });

  it('keeps a distinct current subscription discoverable when full-list deletion fails', async () => {
    const restoreSynchronization = disablePushSynchronization();
    const sources = Array.from({ length: MAX_PENDING_PUSH_SOURCES }, (_, index) => `https://push.example/full-${index}`);
    const local = subscription('https://push.example/full-current', 1);
    const pending = memoryPendingReplacement(sources);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('unavailable', { status: 503 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;

    try {
      await expect(unsubscribePush(registration, pending)).rejects.toThrow('503');
    } finally {
      restoreSynchronization();
    }

    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body))).toEqual({ endpoints: [...sources, local.endpoint] });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(local.unsubscribe).not.toHaveBeenCalled();
    expect(pending.remove).not.toHaveBeenCalled();
    expect(pending.state()).toEqual({ source_endpoints: sources, opted_out: true });
  });

  it('retries the same full-list cleanup without enabling push', async () => {
    const restoreSynchronization = disablePushSynchronization();
    const sources = Array.from({ length: MAX_PENDING_PUSH_SOURCES }, (_, index) => `https://push.example/full-${index}`);
    const local = subscription('https://push.example/full-current', 1);
    const pending = memoryPendingReplacement(sources);
    const subscribe = vi.fn();
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local), subscribe } } as unknown as ServiceWorkerRegistration;
    const failedFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('unavailable', { status: 503 }));
    vi.stubGlobal('fetch', failedFetch);

    try {
      await expect(unsubscribePush(registration, pending)).rejects.toThrow('503');
      const recoveredFetch = vi.fn()
        .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
        .mockResolvedValueOnce(new Response(null, { status: 204 }));
      vi.stubGlobal('fetch', recoveredFetch);
      await unsubscribePush(registration, pending);

      expect(JSON.parse(String(failedFetch.mock.calls[1]?.[1]?.body))).toEqual({ endpoints: [...sources, local.endpoint] });
      expect(JSON.parse(String(recoveredFetch.mock.calls[1]?.[1]?.body))).toEqual({ endpoints: [...sources, local.endpoint] });
      expect(failedFetch).toHaveBeenCalledTimes(2);
      expect(recoveredFetch).toHaveBeenCalledTimes(2);
    } finally {
      restoreSynchronization();
    }

    expect(local.unsubscribe).toHaveBeenCalledOnce();
    expect(subscribe).not.toHaveBeenCalled();
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });

  it('turns off after a failed VAPID rotation without leaving either endpoint registered', async () => {
    const oldSubscription = subscription('https://push.example/rotation-old', 1);
    const replacement = subscription('https://push.example/rotation-new', 2);
    let current: PushSubscription | null = oldSubscription;
    oldSubscription.unsubscribe = vi.fn(async () => { current = null; return true; });
    replacement.unsubscribe = vi.fn(async () => { current = null; return true; });
    const subscribe = vi.fn(async () => { current = replacement; return replacement; });
    const registration = { pushManager: { getSubscription: vi.fn(async () => current), subscribe } } as unknown as ServiceWorkerRegistration;
    const pending = memoryPendingReplacement();
    const rotationFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('unavailable', { status: 503 }));
    vi.stubGlobal('fetch', rotationFetch);

    await expect(reconcilePush(registration, 'Ag', pending)).rejects.toThrow('503');
    expect(pending.state()).toEqual({ source_endpoints: [oldSubscription.endpoint], opted_out: false });

    const deleteFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', deleteFetch);
    await unsubscribePush(registration, pending);

    expect(JSON.parse(String(deleteFetch.mock.calls[1]?.[1]?.body))).toEqual({ endpoints: [oldSubscription.endpoint, replacement.endpoint] });
    expect(replacement.unsubscribe).toHaveBeenCalledOnce();
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });

  it('retries failed opt-out cleanup after reload without creating or registering', async () => {
    const local = subscription('https://push.example/current-device', 2);
    const pending = memoryPendingReplacement(['https://push.example/old-device']);
    const subscribe = vi.fn();
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local), subscribe } } as unknown as ServiceWorkerRegistration;
    const failedFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('unavailable', { status: 503 }));
    vi.stubGlobal('fetch', failedFetch);

    await expect(unsubscribePush(registration, pending)).rejects.toThrow('503');
    expect(local.unsubscribe).not.toHaveBeenCalled();
    expect(pending.state()).toEqual({ source_endpoints: ['https://push.example/old-device'], opted_out: true });

    const recoveredFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', recoveredFetch);
    await expect(reconcilePush(registration, 'Ag', pending)).resolves.toEqual({ subscribed: false });

    expect(JSON.parse(String(recoveredFetch.mock.calls[1]?.[1]?.body))).toEqual({ endpoints: ['https://push.example/old-device', local.endpoint] });
    expect(local.unsubscribe).toHaveBeenCalledOnce();
    expect(subscribe).not.toHaveBeenCalled();
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });

  it('explicit subscribe clears opt-out before replacing stale sources', async () => {
    const local = subscription('https://push.example/re-enabled', 2);
    const pending = memoryPendingReplacement(['https://push.example/old-device'], true);
    const subscribe = vi.fn().mockResolvedValue(local);
    const registration = { pushManager: { subscribe } } as unknown as ServiceWorkerRegistration;
    vi.stubGlobal('Notification', { requestPermission: vi.fn().mockResolvedValue('granted') });
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);

    await subscribePush(registration, 'Ag', pending);

    expect(pending.setOptedOut).toHaveBeenCalledWith(false);
    expect(pending.setOptedOut.mock.invocationCallOrder[0]!).toBeLessThan(subscribe.mock.invocationCallOrder[0]!);
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body)).source_endpoints).toEqual(['https://push.example/old-device']);
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: false });
  });

  it('parses the explicit disabled test response', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify(fixture.cases.push_test_disabled.response), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    await expect(testPush()).resolves.toBe(false);
    expect(fetchMock.mock.calls[1]?.[0]).toBe(schema['x-endpoints'].push_test_disabled.path);
  });

  it('retries A and B as endpoint-only candidates after a committed response is lost', async () => {
    const currentB = subscription('https://push.example/current-b', 1);
    const currentC = subscription('https://push.example/current-c', 2);
    const pending = memoryPendingReplacement(['https://push.example/original-a']);
    let current: PushSubscription | null = currentB;
    currentB.unsubscribe = vi.fn(async () => { current = null; return true; });
    const subscribe = vi.fn(async () => { current = currentC; return currentC; });
    const registration = { pushManager: { getSubscription: vi.fn(async () => current), subscribe } } as unknown as ServiceWorkerRegistration;
    const failedFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockRejectedValueOnce(new TypeError('committed response lost'));
    vi.stubGlobal('fetch', failedFetch);

    await expect(reconcilePush(registration, 'Ag', pending)).rejects.toThrow('response lost');

    expect(pending.state().source_endpoints).toEqual(['https://push.example/original-a', currentB.endpoint]);
    const lostBody = failedFetch.mock.calls[1]?.[1]?.body;
    expect(JSON.parse(String(lostBody))).toEqual({
      source_endpoints: ['https://push.example/original-a', currentB.endpoint],
      subscription: currentC.toJSON(),
    });

    const recoveredFetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', recoveredFetch);
    await expect(reconcilePush(registration, 'Ag', pending)).resolves.toEqual({ subscribed: true });

    expect(recoveredFetch.mock.calls[1]?.[1]?.body).toBe(lostBody);
    expect(subscribe).toHaveBeenCalledOnce();
    expect(pending.state().source_endpoints).toEqual([]);
  });

  it('retains endpoint candidates across an uncommitted transient failure', async () => {
    const local = subscription('https://push.example/current', 2);
    const pending = memoryPendingReplacement(['https://push.example/old']);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('unavailable', { status: 503 }));
    vi.stubGlobal('fetch', fetchMock);

    await expect(reconcilePush(registration, 'Ag', pending)).rejects.toThrow('503');
    expect(pending.state().source_endpoints).toEqual(['https://push.example/old']);
  });

  it('registers the current target after every source candidate is missing', async () => {
    const local = subscription('https://push.example/current-after-cleanup', 2);
    const pending = memoryPendingReplacement(['https://push.example/removed-by-cleanup']);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf-1' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('missing', { status: 404 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf-2' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);

    await expect(reconcilePush(registration, 'Ag', pending)).resolves.toEqual({ subscribed: true });

    expect(fetchMock.mock.calls[3]?.[0]).toBe(schema['x-endpoints'].push_register.path);
    expect(pending.state().source_endpoints).toEqual([]);
  });

  it('recovers the crash window after persisting and unsubscribing but before subscribing', async () => {
    const replacement = subscription('https://push.example/recovered', 2);
    const pending = memoryPendingReplacement(['https://push.example/unsubscribed-old']);
    let current: PushSubscription | null = null;
    const subscribe = vi.fn(async (_options: PushSubscriptionOptions) => { current = replacement; return replacement; });
    const registration = { pushManager: { getSubscription: vi.fn(async () => current), subscribe } } as unknown as ServiceWorkerRegistration;
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);

    await expect(reconcilePush(registration, 'Ag', pending)).resolves.toEqual({ subscribed: true });

    expect(subscribe).toHaveBeenCalledOnce();
    expect(new Uint8Array((subscribe.mock.calls[0]?.[0] as PushSubscriptionOptions).applicationServerKey as ArrayBuffer)).toEqual(new Uint8Array([2]));
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body)).source_endpoints).toEqual(['https://push.example/unsubscribed-old']);
    expect(pending.state().source_endpoints).toEqual([]);
  });

  it('does not unsubscribe when a full source list cannot be settled', async () => {
    const local = subscription('https://push.example/not-yet-persisted', 1);
    const sources = Array.from({ length: MAX_PENDING_PUSH_SOURCES }, (_, index) => `https://push.example/source-${index}`);
    const pending = memoryPendingReplacement(sources);
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local), subscribe: vi.fn() } } as unknown as ServiceWorkerRegistration;
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('unavailable', { status: 503 }));
    vi.stubGlobal('fetch', fetchMock);

    await expect(reconcilePush(registration, 'Ag', pending)).rejects.toThrow('503');

    expect(local.unsubscribe).not.toHaveBeenCalled();
    expect(registration.pushManager.subscribe).not.toHaveBeenCalled();
    expect(pending.state().source_endpoints).toEqual(sources);
  });

  it('serializes overlapping reconciliations and finishes with the newest queued key', async () => {
    let current: PushSubscription | null;
    const created: PushSubscription[] = [];
    const makeCurrent = (endpoint: string, key: number) => {
      const value = subscription(endpoint, key);
      value.unsubscribe = vi.fn(async () => {
        if (current === value) current = null;
        return true;
      });
      return value;
    };
    current = makeCurrent('https://push.example/key-0', 0);
    const subscribe = vi.fn(async (options: PushSubscriptionOptions) => {
      const key = new Uint8Array(options.applicationServerKey as ArrayBuffer)[0]!;
      const value = makeCurrent(`https://push.example/key-${key}`, key);
      created.push(value);
      current = value;
      return value;
    });
    const registration = { pushManager: { getSubscription: vi.fn(async () => current), subscribe } } as unknown as ServiceWorkerRegistration;
    const pending = memoryPendingReplacement();
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf-1' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf-2' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);

    const stale = reconcilePush(registration, 'AQ', pending);
    const newest = reconcilePush(registration, 'Ag', pending);
    await expect(Promise.all([stale, newest])).resolves.toEqual([{ subscribed: true }, { subscribed: true }]);

    expect(subscribe).toHaveBeenCalledTimes(2);
    expect(created.map((value) => new Uint8Array(value.options.applicationServerKey as ArrayBuffer)[0])).toEqual([1, 2]);
    expect(current?.endpoint).toBe('https://push.example/key-2');
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body)).source_endpoints).toEqual(['https://push.example/key-0']);
    expect(JSON.parse(String(fetchMock.mock.calls[3]?.[1]?.body)).source_endpoints).toEqual(['https://push.example/key-1']);
    expect(pending.state().source_endpoints).toEqual([]);
  });
});
