import { ApiError, isExactObject, mutatingFetch } from './http';
import { PendingReplacementOverflowError, pendingReplacementStore, type PendingReplacementStore } from '../push/pendingReplacement';
import { PushSynchronizationUnsupportedError, pushSynchronizationSupported, withPushSynchronizationLock } from '../push/synchronizationLock';

export interface PushReconciliation {
  subscribed: boolean;
}

export interface PushSynchronizationOptions {
  createIfMissing: boolean;
  registerIfUnsynchronized: boolean;
}

export interface StoredPushSubscription {
  endpoint: string;
  keys: { p256dh: string; auth: string };
}

const synchronizationQueues = new WeakMap<ServiceWorkerRegistration, Promise<void>>();

function parseReconciliation(value: unknown): PushReconciliation {
  if (!isExactObject(value, ['subscribed'])) throw new Error('Invalid push subscription response');
  if (typeof value.subscribed !== 'boolean') throw new Error('Invalid push subscription response');
  return value as unknown as PushReconciliation;
}

function base64UrlToBytes(value: string): Uint8Array<ArrayBuffer> {
  const padded = `${value}${'='.repeat((4 - (value.length % 4)) % 4)}`.replace(/-/g, '+').replace(/_/g, '/');
  const binary = atob(padded);
  const bytes = new Uint8Array(new ArrayBuffer(binary.length));
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

function serializeSynchronization<T>(registration: ServiceWorkerRegistration, operation: () => Promise<T>): Promise<T> {
  const previous = synchronizationQueues.get(registration) ?? Promise.resolve();
  const result = previous.catch(() => undefined).then(() => withPushSynchronizationLock(operation));
  const tail = result.then(() => undefined, () => undefined);
  synchronizationQueues.set(registration, tail);
  void tail.then(() => {
    if (synchronizationQueues.get(registration) === tail) synchronizationQueues.delete(registration);
  });
  return result;
}

export function runPushSynchronization<T>(registration: ServiceWorkerRegistration, operation: () => Promise<T>): Promise<T> {
  return serializeSynchronization(registration, operation);
}

export function reconcilePush(registration: ServiceWorkerRegistration, publicKey: string, pending: PendingReplacementStore = pendingReplacementStore): Promise<PushReconciliation> {
  if (!pushSynchronizationSupported()) return Promise.resolve({ subscribed: false });
  return serializeSynchronization(registration, async () => {
    const local = await registration.pushManager.getSubscription();
    const synchronized = await synchronizeCurrentPushSubscriptionNow(registration, local, publicKey, pending, { createIfMissing: false, registerIfUnsynchronized: false });
    if (!synchronized.subscription) return { subscribed: false };
    if (synchronized.serverSynchronized) return { subscribed: true };
    const response = await mutatingFetch('/api/v1/push/subscriptions/reconcile', {
      method: 'POST',
      body: JSON.stringify({ endpoint: synchronized.subscription.endpoint }),
    });
    const remote = parseReconciliation(await response.json());
    if (!remote.subscribed) await registerPushSubscription(serializePushSubscription(synchronized.subscription));
    return { subscribed: true };
  });
}

export async function subscribePush(registration: ServiceWorkerRegistration, publicKey: string, pending: PendingReplacementStore = pendingReplacementStore): Promise<void> {
  if (!pushSynchronizationSupported()) throw new PushSynchronizationUnsupportedError();
  const permission = await Notification.requestPermission();
  if (permission !== 'granted') throw new Error('Notification permission was not granted');
  await serializeSynchronization(registration, async () => {
    await pending.setOptedOut(false);
    const subscription = await subscribeWithPublicKey(registration, publicKey);
    try {
      const state = await pending.load();
      if (state.source_endpoints.length > 0) await settlePendingSources(state.source_endpoints, subscription, pending);
      else await registerPushSubscription(serializePushSubscription(subscription));
    } catch (error) {
      await subscription.unsubscribe();
      throw error;
    }
  });
}

export function unsubscribePush(registration: ServiceWorkerRegistration, pending: PendingReplacementStore = pendingReplacementStore): Promise<void> {
  if (!pushSynchronizationSupported()) return unsubscribePushWithoutSynchronization(registration, pending);
  return serializeSynchronization(registration, async () => {
    await pending.setOptedOut(true);
    const subscription = await registration.pushManager.getSubscription();
    const state = await pending.load();
    await cleanUpOptedOutPush(subscription, state.source_endpoints, pending);
  });
}

export async function testPush(): Promise<boolean> {
  const response = await mutatingFetch('/api/v1/push/test', { method: 'POST', body: '{}' });
  const value: unknown = await response.json();
  if (!isExactObject(value, ['enabled']) || typeof value.enabled !== 'boolean') throw new Error('Invalid push test response');
  return value.enabled;
}

export function synchronizeCurrentPushSubscription(
  registration: ServiceWorkerRegistration,
  subscription: PushSubscription | null,
  publicKey: string,
  pending: PendingReplacementStore = pendingReplacementStore,
  options: PushSynchronizationOptions = { createIfMissing: false, registerIfUnsynchronized: false },
): Promise<{ subscription: PushSubscription | null; serverSynchronized: boolean }> {
  if (!pushSynchronizationSupported()) return Promise.resolve({ subscription, serverSynchronized: false });
  return serializeSynchronization(registration, async () => {
    return synchronizeCurrentPushSubscriptionLocked(registration, subscription, publicKey, pending, options);
  });
}

export async function synchronizeCurrentPushSubscriptionLocked(
  registration: ServiceWorkerRegistration,
  subscription: PushSubscription | null,
  publicKey: string,
  pending: PendingReplacementStore,
  options: PushSynchronizationOptions,
): Promise<{ subscription: PushSubscription | null; serverSynchronized: boolean }> {
  const current = await registration.pushManager.getSubscription();
  return synchronizeCurrentPushSubscriptionNow(registration, current ?? subscription, publicKey, pending, options);
}

async function synchronizeCurrentPushSubscriptionNow(
  registration: ServiceWorkerRegistration,
  subscription: PushSubscription | null,
  publicKey: string,
  pending: PendingReplacementStore,
  options: PushSynchronizationOptions,
): Promise<{ subscription: PushSubscription | null; serverSynchronized: boolean }> {
  let local = subscription;
  let serverSynchronized = false;
  let state = await pending.load();

  if (state.opted_out) {
    await cleanUpOptedOutPush(local, state.source_endpoints, pending);
    return { subscription: null, serverSynchronized: true };
  }

  if (!local && state.source_endpoints.length > 0) {
    local = await subscribeWithPublicKey(registration, publicKey);
  }

  if (local && !applicationServerKeyMatches(local, publicKey)) {
    try {
      await pending.append(local.endpoint);
    } catch (error) {
      if (!(error instanceof PendingReplacementOverflowError) || state.source_endpoints.length === 0) throw error;
      await settlePendingSources(state.source_endpoints, local, pending);
      serverSynchronized = true;
      await pending.append(local.endpoint);
    }
    await local.unsubscribe();
    local = await subscribeWithPublicKey(registration, publicKey);
  }

  state = await pending.load();
  if (state.source_endpoints.length > 0) {
    if (!local) local = await subscribeWithPublicKey(registration, publicKey);
    await settlePendingSources(state.source_endpoints, local, pending);
    serverSynchronized = true;
  }

  if (!local && options.createIfMissing) local = await subscribeWithPublicKey(registration, publicKey);
  if (!local) return { subscription: null, serverSynchronized };

  if (options.registerIfUnsynchronized && !serverSynchronized) {
    await registerPushSubscription(serializePushSubscription(local));
    serverSynchronized = true;
  }
  return { subscription: local, serverSynchronized };
}

async function settlePendingSources(sourceEndpoints: readonly string[], subscription: PushSubscription, pending: PendingReplacementStore): Promise<void> {
  const target = serializePushSubscription(subscription);
  try {
    await replacePushSubscription(sourceEndpoints, target);
  } catch (error) {
    if (!(error instanceof ApiError) || error.status !== 404) throw error;
    await registerPushSubscription(target);
  }
  await pending.remove(sourceEndpoints);
}

async function registerPushSubscription(subscription: StoredPushSubscription): Promise<void> {
  await mutatingFetch('/api/v1/push/subscriptions', { method: 'POST', body: JSON.stringify(subscription) });
}

async function cleanUpOptedOutPush(subscription: PushSubscription | null, sourceEndpoints: readonly string[], pending: PendingReplacementStore): Promise<void> {
  const endpoints = [...sourceEndpoints];
  if (subscription && !endpoints.includes(subscription.endpoint)) endpoints.push(subscription.endpoint);
  if (endpoints.length > 0) await deletePushSubscriptions(endpoints);
  if (subscription && !await subscription.unsubscribe()) throw new Error('Unable to unsubscribe local push subscription');
  await pending.remove(sourceEndpoints);
}

async function deletePushSubscriptions(endpoints: readonly string[]): Promise<void> {
  await mutatingFetch('/api/v1/push/subscriptions', { method: 'DELETE', body: JSON.stringify({ endpoints }) });
}

async function unsubscribePushWithoutSynchronization(registration: ServiceWorkerRegistration, pending: PendingReplacementStore): Promise<void> {
  await pending.setOptedOut(true);
  const subscription = await registration.pushManager.getSubscription();
  let currentEndpointPersisted = !subscription;
  if (subscription) {
    try {
      await pending.append(subscription.endpoint);
      currentEndpointPersisted = true;
    } catch (error) {
      if (!(error instanceof PendingReplacementOverflowError)) throw error;
    }
  }
  const state = await pending.load();
  const endpoints = [...state.source_endpoints];
  if (subscription && !endpoints.includes(subscription.endpoint)) endpoints.push(subscription.endpoint);
  if (subscription && state.source_endpoints.includes(subscription.endpoint)) currentEndpointPersisted = true;
  try {
    if (endpoints.length > 0) await deletePushSubscriptions(endpoints);
  } catch (error) {
    if (subscription && currentEndpointPersisted && !await subscription.unsubscribe()) throw new Error('Unable to unsubscribe local push subscription');
    throw error;
  }
  if (subscription && !await subscription.unsubscribe()) throw new Error('Unable to unsubscribe local push subscription');
  await pending.remove(state.source_endpoints);
}

async function replacePushSubscription(sourceEndpoints: readonly string[], subscription: StoredPushSubscription): Promise<void> {
  await mutatingFetch('/api/v1/push/subscriptions/replace', {
    method: 'POST',
    body: JSON.stringify({ source_endpoints: sourceEndpoints, subscription }),
  });
}

export function applicationServerKeyMatches(subscription: PushSubscription, publicKey: string): boolean {
  const current = subscription.options.applicationServerKey;
  if (!current) return false;
  const actual = new Uint8Array(current);
  const expected = base64UrlToBytes(publicKey);
  return actual.length === expected.length && actual.every((value, index) => value === expected[index]);
}

export function serializePushSubscription(subscription: PushSubscription): StoredPushSubscription {
  const value = subscription.toJSON();
  const p256dh = value.keys?.p256dh;
  const auth = value.keys?.auth;
  if (typeof value.endpoint !== 'string' || typeof p256dh !== 'string' || typeof auth !== 'string') throw new Error('Push subscription is missing replacement payload');
  return { endpoint: value.endpoint, keys: { p256dh, auth } };
}

async function subscribeWithPublicKey(registration: ServiceWorkerRegistration, publicKey: string): Promise<PushSubscription> {
  return registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: base64UrlToBytes(publicKey) });
}
