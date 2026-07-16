import { describe, expect, it } from 'vitest';
import serviceWorkerSource from '../src/sw.ts?raw';

describe('service worker cache and notification policy', () => {
  it('precaches only the injected shell and defines no runtime content cache', () => {
    expect(serviceWorkerSource).toContain('precacheAndRoute(self.__WB_MANIFEST)');
    expect(serviceWorkerSource).toContain("'/api/'");
    expect(serviceWorkerSource).toContain("'/auth/'");
    expect(serviceWorkerSource).not.toMatch(/CacheFirst|NetworkFirst|StaleWhileRevalidate|caches\.open/);
  });

  it('opens the agents route and asks the page to refresh authenticated state', () => {
    expect(serviceWorkerSource).toContain("openWindow('/agents')");
    expect(serviceWorkerSource).toContain("postMessage({ type: 'push-refresh' })");
  });

  it('handles browser subscription rotation through metadata-only server reconciliation', () => {
    expect(serviceWorkerSource).toContain("addEventListener('pushsubscriptionchange'");
    expect(serviceWorkerSource).toContain('handlePushSubscriptionChange(self.registration, event.oldSubscription, event.newSubscription)');
    expect(serviceWorkerSource).not.toContain('applicationServerKey = event.oldSubscription');
    expect(serviceWorkerSource).not.toMatch(/terminal_id|prompt|terminal output|sent input/i);
  });
});
