import { runPushSynchronization, synchronizeCurrentPushSubscriptionLocked } from '../api/push';
import { bootstrapSession } from '../api/session';
import { ApiError } from '../api/http';
import { pendingReplacementStore, type PendingReplacementStore } from './pendingReplacement';
import { pushSynchronizationSupported } from './synchronizationLock';

export async function handlePushSubscriptionChange(
  registration: ServiceWorkerRegistration,
  oldSubscription: PushSubscription | null,
  newSubscription: PushSubscription | null,
  pending: PendingReplacementStore = pendingReplacementStore,
): Promise<void> {
  if (!pushSynchronizationSupported()) return;
  await runPushSynchronization(registration, async () => {
    const endpointsChanged = oldSubscription !== null && (newSubscription === null || oldSubscription.endpoint !== newSubscription.endpoint);
    if (endpointsChanged) await pending.append(oldSubscription.endpoint);
    let session;
    try {
      session = await bootstrapSession();
    } catch (error) {
      if (error instanceof ApiError && (error.status === 401 || error.status === 403)) return;
      throw error;
    }
    if (!session.push_public_key) return;
    await synchronizeCurrentPushSubscriptionLocked(registration, newSubscription, session.push_public_key, pending, { createIfMissing: true, registerIfUnsynchronized: true });
  });
}
