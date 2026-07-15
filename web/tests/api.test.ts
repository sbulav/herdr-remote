import { describe, expect, it, vi } from 'vitest';
import { bootstrapSession, logout } from '../src/api/session';

describe('same-origin session APIs', () => {
  it('bootstraps without storing or sending browser-managed credentials', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null }), { status: 200 }),
    );
    vi.stubGlobal('fetch', fetchMock);
    await expect(bootstrapSession()).resolves.toMatchObject({ authenticated: true });
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/session', expect.objectContaining({ credentials: 'same-origin', cache: 'no-store' }));
  });

  it('fetches a fresh CSRF token before logout', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf-value' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    await logout();
    expect(fetchMock.mock.calls[0]?.[0]).toBe('/api/v1/csrf');
    const [path, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(path).toBe('/auth/logout');
    expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('csrf-value');
    expect(init.credentials).toBe('same-origin');
  });
});
