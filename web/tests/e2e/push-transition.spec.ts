import { expect, test } from '@playwright/test';

test('migrates and bounds an endpoint-only source chain in browser IndexedDB', async ({ page }) => {
  await page.route('**/api/v1/session', (route) => route.fulfill({ status: 401, body: 'unauthorized' }));
  await page.goto('/agents');

  const result = await page.evaluate(async () => {
    const request = <T>(value: IDBRequest<T>) => new Promise<T>((resolve, reject) => {
      value.addEventListener('success', () => resolve(value.result));
      value.addEventListener('error', () => reject(value.error));
    });
    await new Promise<void>((resolve, reject) => {
      const deletion = indexedDB.deleteDatabase('herdr-remote-push-v1');
      deletion.addEventListener('success', () => resolve());
      deletion.addEventListener('error', () => reject(deletion.error));
      deletion.addEventListener('blocked', () => reject(new Error('test database deletion blocked')));
    });
    const legacyDatabase = await new Promise<IDBDatabase>((resolve, reject) => {
      const opening = indexedDB.open('herdr-remote-push-v1', 1);
      opening.addEventListener('upgradeneeded', () => opening.result.createObjectStore('pending-replacements', { keyPath: 'key' }));
      opening.addEventListener('success', () => resolve(opening.result));
      opening.addEventListener('error', () => reject(opening.error));
    });
    const legacyTransaction = legacyDatabase.transaction('pending-replacements', 'readwrite');
    legacyTransaction.objectStore('pending-replacements').put({
      key: 'current',
      settled_endpoint: null,
      transitions: [{
        source_endpoint: 'https://push.example/legacy-old',
        target: { endpoint: 'https://push.example/legacy-new', keys: { p256dh: 'historical-p256dh', auth: 'historical-auth' } },
      }],
    });
    await new Promise<void>((resolve, reject) => {
      legacyTransaction.addEventListener('complete', () => resolve());
      legacyTransaction.addEventListener('error', () => reject(legacyTransaction.error));
    });
    legacyDatabase.close();

    const modulePath = '/src/push/pendingReplacement.ts';
    const loadModule = await import(/* @vite-ignore */ modulePath) as {
      MAX_PENDING_PUSH_SOURCES: number;
      pendingReplacementStore: {
        load(): Promise<{ source_endpoints: string[]; opted_out: boolean }>;
        append(source: string): Promise<void>;
        remove(sources: readonly string[]): Promise<void>;
        setOptedOut(value: boolean): Promise<void>;
      };
    };
    const { MAX_PENDING_PUSH_SOURCES, pendingReplacementStore } = loadModule;
    const migrated = await pendingReplacementStore.load();
    const migratedDatabase = await request(indexedDB.open('herdr-remote-push-v1', 3));
    const raw = await request(migratedDatabase.transaction('pending-replacements').objectStore('pending-replacements').get('current'));
    migratedDatabase.close();

    await pendingReplacementStore.remove(migrated.source_endpoints);
    await pendingReplacementStore.setOptedOut(true);
    const endpoint = (index: number) => `https://push.example/device-${index}`;
    for (let index = 0; index < MAX_PENDING_PUSH_SOURCES; index += 1) await pendingReplacementStore.append(endpoint(index));
    let overflow = '';
    try {
      await pendingReplacementStore.append(endpoint(MAX_PENDING_PUSH_SOURCES));
    } catch (error) {
      overflow = error instanceof Error ? `${error.name}: ${error.message}` : String(error);
    }
    const full = await pendingReplacementStore.load();
    await pendingReplacementStore.remove([endpoint(0)]);
    await pendingReplacementStore.append(endpoint(MAX_PENDING_PUSH_SOURCES));
    const advanced = await pendingReplacementStore.load();
    await pendingReplacementStore.remove(advanced.source_endpoints);
    const settled = await pendingReplacementStore.load();
    return { limit: MAX_PENDING_PUSH_SOURCES, migrated, raw, overflow, full, advanced, settled };
  });

  expect(result.migrated).toEqual({ source_endpoints: ['https://push.example/legacy-old'], opted_out: false });
  expect(result.raw).toEqual({ key: 'current', source_endpoints: ['https://push.example/legacy-old'], opted_out: false });
  expect(JSON.stringify(result.raw)).not.toMatch(/p256dh|auth|legacy-new/);
  expect(result.overflow).toContain('PendingReplacementOverflowError');
  expect(result.full.source_endpoints).toHaveLength(result.limit);
  expect(result.advanced.source_endpoints).toHaveLength(result.limit);
  expect(result.advanced.source_endpoints.at(-1)).toBe(`https://push.example/device-${result.limit}`);
  expect(result.settled).toEqual({ source_endpoints: [], opted_out: true });
});
