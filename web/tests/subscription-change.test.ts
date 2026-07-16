import { describe, expect, it, vi } from 'vitest';
import { handlePushSubscriptionChange } from '../src/push/subscriptionChange';
import { MAX_PENDING_PUSH_SOURCES, PendingReplacementOverflowError } from '../src/push/pendingReplacement';

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

describe('service-worker push subscription change recovery', () => {
  it('bootstraps the current VAPID key and never resubscribes with the old key', async () => {
    const oldSubscription = subscription('https://push.example/old', 1);
    const replacement = subscription('https://push.example/current-key', 2);
    let current: PushSubscription | null = null;
    const subscribe = vi.fn(async (_options: PushSubscriptionOptions) => { current = replacement; return replacement; });
    const registration = { pushManager: { subscribe, getSubscription: vi.fn(async () => current) } } as unknown as ServiceWorkerRegistration;
    const pending = memoryPendingReplacement();
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: 'Ag', logout_url: 'https://id.example/logout' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);

    await handlePushSubscriptionChange(registration, oldSubscription, null, pending);

    expect(fetchMock.mock.calls[0]?.[0]).toBe('/api/v1/session');
    expect(new Uint8Array((subscribe.mock.calls[0]?.[0] as PushSubscriptionOptions).applicationServerKey as ArrayBuffer)).toEqual(new Uint8Array([2]));
    expect(JSON.parse(String(fetchMock.mock.calls[2]?.[1]?.body)).source_endpoints).toEqual([oldSubscription.endpoint]);
    expect(pending.state().source_endpoints).toEqual([]);
  });

  it('replaces all offline source candidates with only the current target payload', async () => {
    const oldSubscription = subscription('https://push.example/original', 1);
    const staleNew = subscription('https://push.example/stale-new', 1);
    const replacement = subscription('https://push.example/current-key', 2);
    let current: PushSubscription | null = staleNew;
    staleNew.unsubscribe = vi.fn(async () => { current = null; return true; });
    const subscribe = vi.fn(async () => { current = replacement; return replacement; });
    const registration = { pushManager: { subscribe, getSubscription: vi.fn(async () => current) } } as unknown as ServiceWorkerRegistration;
    const pending = memoryPendingReplacement(['https://push.example/older-source']);
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: 'Ag', logout_url: 'https://id.example/logout' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);

    await handlePushSubscriptionChange(registration, oldSubscription, staleNew, pending);

    const body = JSON.parse(String(fetchMock.mock.calls[2]?.[1]?.body));
    expect(body.source_endpoints).toEqual(['https://push.example/older-source', oldSubscription.endpoint, staleNew.endpoint]);
    expect(body.subscription).toEqual(replacement.toJSON());
    expect(JSON.stringify(pending.state())).not.toMatch(/p256dh|auth|keys/);
    expect(pending.state().source_endpoints).toEqual([]);
  });

  it('retains the endpoint when background session bootstrap is unauthorized', async () => {
    const oldSubscription = subscription('https://push.example/private-old', 1);
    const pending = memoryPendingReplacement();
    const fetchMock = vi.fn().mockResolvedValue(new Response('unauthorized', { status: 401 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { subscribe: vi.fn() } } as unknown as ServiceWorkerRegistration;

    await expect(handlePushSubscriptionChange(registration, oldSubscription, null, pending)).resolves.toBeUndefined();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(pending.state().source_endpoints).toEqual([oldSubscription.endpoint]);
  });

  it('retains the endpoint when push is disabled in the current bootstrap', async () => {
    const oldSubscription = subscription('https://push.example/private-old', 1);
    const pending = memoryPendingReplacement();
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' }), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { subscribe: vi.fn() } } as unknown as ServiceWorkerRegistration;

    await expect(handlePushSubscriptionChange(registration, oldSubscription, null, pending)).resolves.toBeUndefined();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(pending.state().source_endpoints).toEqual([oldSubscription.endpoint]);
  });

  it('appends an offline rotation without storing either subscription payload', async () => {
    const oldSubscription = subscription('https://push.example/second-rotation-old', 1);
    const newSubscription = subscription('https://push.example/second-rotation-new', 2);
    const pending = memoryPendingReplacement(['https://push.example/original-pending']);
    const registration = { pushManager: {} } as unknown as ServiceWorkerRegistration;
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('unauthorized', { status: 401 })));

    await handlePushSubscriptionChange(registration, oldSubscription, newSubscription, pending);

    expect(pending.state()).toEqual({ source_endpoints: ['https://push.example/original-pending', oldSubscription.endpoint], opted_out: false });
    expect(JSON.stringify(pending.state())).not.toMatch(/p256dh|auth|second-rotation-new/);
  });

  it('fails explicitly when the endpoint-only source list is full', async () => {
    const oldSubscription = subscription('https://push.example/overflow-old', 1);
    const sources = Array.from({ length: MAX_PENDING_PUSH_SOURCES }, (_, index) => `https://push.example/source-${index}`);
    const pending = memoryPendingReplacement(sources);
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    const registration = { pushManager: { subscribe: vi.fn() } } as unknown as ServiceWorkerRegistration;

    await expect(handlePushSubscriptionChange(registration, oldSubscription, null, pending)).rejects.toBeInstanceOf(PendingReplacementOverflowError);

    expect(fetchMock).not.toHaveBeenCalled();
    expect(oldSubscription.unsubscribe).not.toHaveBeenCalled();
    expect(pending.state().source_endpoints).toEqual(sources);
  });

  it('cleans a browser rotation without resubscribing while opted out', async () => {
    const oldSubscription = subscription('https://push.example/opted-out-old', 1);
    const newSubscription = subscription('https://push.example/opted-out-new', 2);
    const pending = memoryPendingReplacement([], true);
    const subscribe = vi.fn();
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(newSubscription), subscribe } } as unknown as ServiceWorkerRegistration;
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: 'Ag', logout_url: 'https://id.example/logout' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal('fetch', fetchMock);

    await handlePushSubscriptionChange(registration, oldSubscription, newSubscription, pending);

    expect(JSON.parse(String(fetchMock.mock.calls[2]?.[1]?.body))).toEqual({ endpoints: [oldSubscription.endpoint, newSubscription.endpoint] });
    expect(newSubscription.unsubscribe).toHaveBeenCalledOnce();
    expect(subscribe).not.toHaveBeenCalled();
    expect(pending.state()).toEqual({ source_endpoints: [], opted_out: true });
  });
});
