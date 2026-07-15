import { apiFetch, isExactObject, mutatingFetch } from './http';

export interface PushReconciliation {
  subscribed: boolean;
  endpoint: string | null;
}

function parseReconciliation(value: unknown): PushReconciliation {
  if (!isExactObject(value, ['subscribed', 'endpoint'])) throw new Error('Invalid push subscription response');
  if (typeof value.subscribed !== 'boolean') throw new Error('Invalid push subscription response');
  if (value.endpoint !== null && typeof value.endpoint !== 'string') throw new Error('Invalid push subscription response');
  return value as unknown as PushReconciliation;
}

function base64UrlToBytes(value: string): Uint8Array<ArrayBuffer> {
  const padded = `${value}${'='.repeat((4 - (value.length % 4)) % 4)}`.replace(/-/g, '+').replace(/_/g, '/');
  const binary = atob(padded);
  const bytes = new Uint8Array(new ArrayBuffer(binary.length));
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

export async function reconcilePush(registration: ServiceWorkerRegistration): Promise<PushReconciliation> {
  const local = await registration.pushManager.getSubscription();
  const response = await apiFetch('/api/v1/push/subscription');
  const remote = parseReconciliation(await response.json());
  const endpointMatches = local?.endpoint === remote.endpoint;
  if (remote.subscribed && !endpointMatches) {
    await mutatingFetch('/api/v1/push/subscription', { method: 'DELETE', body: '{}' });
    await local?.unsubscribe();
    return { subscribed: false, endpoint: null };
  }
  if (!remote.subscribed && local) {
    await local.unsubscribe();
    return { subscribed: false, endpoint: null };
  }
  return { subscribed: Boolean(local && remote.subscribed), endpoint: local?.endpoint ?? null };
}

export async function subscribePush(registration: ServiceWorkerRegistration, publicKey: string): Promise<void> {
  const permission = await Notification.requestPermission();
  if (permission !== 'granted') throw new Error('Notification permission was not granted');
  const subscription = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: base64UrlToBytes(publicKey),
  });
  try {
    await mutatingFetch('/api/v1/push/subscription', { method: 'PUT', body: JSON.stringify(subscription.toJSON()) });
  } catch (error) {
    await subscription.unsubscribe();
    throw error;
  }
}

export async function unsubscribePush(registration: ServiceWorkerRegistration): Promise<void> {
  const subscription = await registration.pushManager.getSubscription();
  await mutatingFetch('/api/v1/push/subscription', { method: 'DELETE', body: '{}' });
  if (subscription) await subscription.unsubscribe();
}

export async function testPush(): Promise<void> {
  await mutatingFetch('/api/v1/push/test', { method: 'POST', body: '{}' });
}

export async function reconcileChangedPushSubscription(subscription: PushSubscription | null): Promise<void> {
  await mutatingFetch('/api/v1/push/subscription', {
    method: subscription ? 'PUT' : 'DELETE',
    body: subscription ? JSON.stringify(subscription.toJSON()) : '{}',
  });
}
