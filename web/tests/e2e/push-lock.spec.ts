import { expect, test, type BrowserContext, type Page } from '@playwright/test';

const sessionPath = '**/api/v1/session';
const csrfPath = '**/api/v1/csrf';
const subscriptionsPath = '**/api/v1/push/subscriptions';

async function preparePages(context: BrowserContext, page: Page, serverSubscriptions: Set<string>): Promise<Page> {
  await context.route(sessionPath, (route) => route.fulfill({ status: 401, body: 'unauthorized' }));
  await context.route(csrfPath, (route) => route.fulfill({ json: { token: 'csrf-token' } }));
  await context.route(subscriptionsPath, async (route) => {
    const request = route.request();
    const body = request.postDataJSON() as { endpoint?: string; endpoints?: string[] };
    if (request.method() === 'POST' && body.endpoint) serverSubscriptions.add(body.endpoint);
    if (request.method() === 'DELETE') for (const endpoint of body.endpoints ?? []) serverSubscriptions.delete(endpoint);
    await route.fulfill({ status: 204 });
  });
  const worker = await context.newPage();
  await Promise.all([page.goto('/agents'), worker.goto('/agents')]);
  await page.evaluate(async () => {
    localStorage.removeItem('push-lock-local-subscription');
    const modulePath = '/src/push/pendingReplacement.ts';
    const { pendingReplacementStore } = await import(/* @vite-ignore */ modulePath);
    const state = await pendingReplacementStore.load();
    await pendingReplacementStore.remove(state.source_endpoints);
    await pendingReplacementStore.setOptedOut(false);
  });
  return worker;
}

test('worker reconciliation first is followed by durable page opt-out', async ({ context, page }) => {
  const serverSubscriptions = new Set<string>();
  const worker = await preparePages(context, page, serverSubscriptions);
  const workerRun = worker.evaluate(async () => {
    const apiPath = '/src/api/push.ts';
    const pendingPath = '/src/push/pendingReplacement.ts';
    const { runPushSynchronization, synchronizeCurrentPushSubscriptionLocked } = await import(/* @vite-ignore */ apiPath);
    const { pendingReplacementStore } = await import(/* @vite-ignore */ pendingPath);
    const makeSubscription = (endpoint: string) => ({
      endpoint,
      options: { applicationServerKey: new Uint8Array([2]).buffer },
      toJSON: () => ({ endpoint, keys: { p256dh: 'key', auth: 'auth' } }),
      unsubscribe: async () => { localStorage.removeItem('push-lock-local-subscription'); return true; },
    });
    const registration = {
      pushManager: {
        getSubscription: async () => {
          const endpoint = localStorage.getItem('push-lock-local-subscription');
          return endpoint ? makeSubscription(endpoint) : null;
        },
        subscribe: async () => {
          const endpoint = 'https://push.example/worker-created';
          localStorage.setItem('push-lock-local-subscription', endpoint);
          return makeSubscription(endpoint);
        },
      },
    } as unknown as ServiceWorkerRegistration;
    return runPushSynchronization(registration, async () => {
      const state = await pendingReplacementStore.load();
      (window as Window & { workerReadFalse?: boolean }).workerReadFalse = !state.opted_out;
      await new Promise<void>((resolve) => { (window as Window & { releasePushRace?: () => void }).releasePushRace = resolve; });
      return synchronizeCurrentPushSubscriptionLocked(registration, null, 'Ag', pendingReplacementStore, { createIfMissing: true, registerIfUnsynchronized: true });
    });
  });
  await expect.poll(() => worker.evaluate(() => Boolean((window as Window & { workerReadFalse?: boolean }).workerReadFalse))).toBe(true);

  const optOut = page.evaluate(async () => {
    const apiPath = '/src/api/push.ts';
    const pendingPath = '/src/push/pendingReplacement.ts';
    const { unsubscribePush } = await import(/* @vite-ignore */ apiPath);
    const { pendingReplacementStore } = await import(/* @vite-ignore */ pendingPath);
    const makeSubscription = (endpoint: string) => ({
      endpoint,
      unsubscribe: async () => { localStorage.removeItem('push-lock-local-subscription'); return true; },
    });
    const registration = { pushManager: { getSubscription: async () => {
      const endpoint = localStorage.getItem('push-lock-local-subscription');
      return endpoint ? makeSubscription(endpoint) : null;
    } } } as unknown as ServiceWorkerRegistration;
    await unsubscribePush(registration, pendingReplacementStore);
  });
  await worker.evaluate(() => (window as Window & { releasePushRace?: () => void }).releasePushRace?.());
  await Promise.all([workerRun, optOut]);

  const final = await page.evaluate(async () => {
    const modulePath = '/src/push/pendingReplacement.ts';
    const { pendingReplacementStore } = await import(/* @vite-ignore */ modulePath);
    return { state: await pendingReplacementStore.load(), local: localStorage.getItem('push-lock-local-subscription') };
  });
  expect(final).toEqual({ state: { source_endpoints: [], opted_out: true }, local: null });
  expect([...serverSubscriptions]).toEqual([]);
});

test('page opt-out first prevents the waiting worker from creating', async ({ context, page }) => {
  const serverSubscriptions = new Set(['https://push.example/existing']);
  const worker = await preparePages(context, page, serverSubscriptions);
  await page.evaluate(() => localStorage.setItem('push-lock-local-subscription', 'https://push.example/existing'));
  const optOut = page.evaluate(async () => {
    const apiPath = '/src/api/push.ts';
    const pendingPath = '/src/push/pendingReplacement.ts';
    const { runPushSynchronization, synchronizeCurrentPushSubscriptionLocked } = await import(/* @vite-ignore */ apiPath);
    const { pendingReplacementStore } = await import(/* @vite-ignore */ pendingPath);
    const endpoint = 'https://push.example/existing';
    const subscription = { endpoint, unsubscribe: async () => { localStorage.removeItem('push-lock-local-subscription'); return true; } } as PushSubscription;
    const registration = { pushManager: { getSubscription: async () => subscription, subscribe: async () => { throw new Error('must not subscribe'); } } } as unknown as ServiceWorkerRegistration;
    await runPushSynchronization(registration, async () => {
      await pendingReplacementStore.setOptedOut(true);
      (window as Window & { optOutLocked?: boolean }).optOutLocked = true;
      await new Promise<void>((resolve) => { (window as Window & { releasePushRace?: () => void }).releasePushRace = resolve; });
      await synchronizeCurrentPushSubscriptionLocked(registration, subscription, 'Ag', pendingReplacementStore, { createIfMissing: true, registerIfUnsynchronized: true });
    });
  });
  await expect.poll(() => page.evaluate(() => Boolean((window as Window & { optOutLocked?: boolean }).optOutLocked))).toBe(true);

  const workerRun = worker.evaluate(async () => {
    const apiPath = '/src/api/push.ts';
    const pendingPath = '/src/push/pendingReplacement.ts';
    const { runPushSynchronization, synchronizeCurrentPushSubscriptionLocked } = await import(/* @vite-ignore */ apiPath);
    const { pendingReplacementStore } = await import(/* @vite-ignore */ pendingPath);
    let subscribed = false;
    const registration = { pushManager: {
      getSubscription: async () => null,
      subscribe: async () => { subscribed = true; throw new Error('must not subscribe'); },
    } } as unknown as ServiceWorkerRegistration;
    await runPushSynchronization(registration, () => synchronizeCurrentPushSubscriptionLocked(registration, null, 'Ag', pendingReplacementStore, { createIfMissing: true, registerIfUnsynchronized: true }));
    return subscribed;
  });
  await page.evaluate(() => (window as Window & { releasePushRace?: () => void }).releasePushRace?.());
  const [, subscribed] = await Promise.all([optOut, workerRun]);

  expect(subscribed).toBe(false);
  const final = await page.evaluate(async () => {
    const modulePath = '/src/push/pendingReplacement.ts';
    const { pendingReplacementStore } = await import(/* @vite-ignore */ modulePath);
    return { state: await pendingReplacementStore.load(), local: localStorage.getItem('push-lock-local-subscription') };
  });
  expect(final).toEqual({ state: { source_endpoints: [], opted_out: true }, local: null });
  expect([...serverSubscriptions]).toEqual([]);
});

test('project browser exposes origin-wide Web Locks', async ({ context, page }) => {
  await context.route(sessionPath, (route) => route.fulfill({ status: 401, body: 'unauthorized' }));
  await page.goto('/agents');
  await expect(page.evaluate(() => typeof navigator.locks?.request)).resolves.toBe('function');
});
