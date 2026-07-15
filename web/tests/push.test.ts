import { describe, expect, it } from 'vitest';
import { EventDeduplicator, parsePushPayload } from '../src/push/payload';
import { reconcileChangedPushSubscription } from '../src/api/push';
import { vi } from 'vitest';

describe('service-worker push payload handling', () => {
  const valid = { event_id: '019f64ca-3000-7000-8000-000000000001', kind: 'agent_state_changed' };

  it('accepts only generic exact payloads', () => {
    expect(parsePushPayload(valid)).toEqual(valid);
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

  it('reconciles a changed subscription using CSRF and metadata only', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    const subscription = { toJSON: () => ({ endpoint: 'https://push.example/random', keys: { p256dh: 'key', auth: 'auth' } }) } as unknown as PushSubscription;
    await reconcileChangedPushSubscription(subscription);
    const [path, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(path).toBe('/api/v1/push/subscription');
    expect(init.method).toBe('PUT');
    expect(init.body).not.toMatch(/terminal|prompt|output|input/i);
  });
});
