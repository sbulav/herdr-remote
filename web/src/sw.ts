/// <reference lib="webworker" />

import { clientsClaim } from 'workbox-core';
import { cleanupOutdatedCaches, createHandlerBoundToURL, precacheAndRoute } from 'workbox-precaching';
import { NavigationRoute, registerRoute } from 'workbox-routing';
import { EventDeduplicator, parsePushPayload } from './push/payload';
import { handlePushSubscriptionChange } from './push/subscriptionChange';

declare let self: ServiceWorkerGlobalScope & { __WB_MANIFEST: Array<{ url: string; revision?: string }> };

const deduplicator = new EventDeduplicator();
const privatePrefixes = ['/api/', '/auth/'];

self.skipWaiting();
clientsClaim();
cleanupOutdatedCaches();
precacheAndRoute(self.__WB_MANIFEST);

registerRoute(
  new NavigationRoute(createHandlerBoundToURL('/index.html'), {
    allowlist: [/^\/agents(?:\/.*)?$/],
    denylist: privatePrefixes.map((prefix) => new RegExp(`^${prefix}`)),
  }),
);

self.addEventListener('push', (event: PushEvent) => {
  let value: unknown;
  try {
    value = event.data?.json();
  } catch {
    return;
  }
  const payload = parsePushPayload(value);
  if (!payload || !deduplicator.accept(payload.event_id)) return;
  event.waitUntil(
    self.registration.showNotification('Herdr agent update', {
      body: 'Open Herdr Remote to view current agent state.',
      icon: '/icon.svg',
      badge: '/icon.svg',
      tag: payload.event_id,
      data: { path: '/agents' },
    }),
  );
});

type SubscriptionChangeEvent = ExtendableEvent & {
  oldSubscription: PushSubscription | null;
  newSubscription: PushSubscription | null;
};

self.addEventListener('pushsubscriptionchange', ((event: SubscriptionChangeEvent) => {
  event.waitUntil((async () => {
    await handlePushSubscriptionChange(self.registration, event.oldSubscription, event.newSubscription);
  })());
}) as EventListener);

self.addEventListener('notificationclick', (event: NotificationEvent) => {
  event.notification.close();
  event.waitUntil(
    (async () => {
      const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
      const existing = windows.find((client) => new URL(client.url).origin === self.location.origin);
      if (existing) {
        await existing.navigate('/agents');
        existing.postMessage({ type: 'push-refresh' });
        await existing.focus();
        return;
      }
      await self.clients.openWindow('/agents');
    })(),
  );
});
