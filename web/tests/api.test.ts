import { describe, expect, it, vi } from 'vitest';
import { bootstrapSession, logout, navigateToLogout } from '../src/api/session';
import schema from '../../protocol/http-v1.schema.json';
import { httpValidator } from './http-schema';

describe('same-origin session APIs', () => {
  it('bootstraps without storing or sending browser-managed credentials', async () => {
    const sessionResponse = { authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' };
    expect(httpValidator('#/$defs/sessionResponse')(sessionResponse)).toBe(true);
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(sessionResponse), { status: 200 }),
    );
    vi.stubGlobal('fetch', fetchMock);
    await expect(bootstrapSession()).resolves.toMatchObject({ authenticated: true });
    expect(fetchMock).toHaveBeenCalledWith(schema['x-endpoints'].session.path, expect.objectContaining({ credentials: 'same-origin', cache: 'no-store' }));
  });

  it('rejects the obsolete unauthenticated response body', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: false, operator: null, push_public_key: null, logout_url: 'https://id.example/logout' }), { status: 200 })));
    await expect(bootstrapSession()).rejects.toThrow('Invalid session response');
  });

  it('rejects an unsafe bootstrap logout URL', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'http://id.example/logout' }), { status: 200 })));
    await expect(bootstrapSession()).rejects.toThrow('Invalid session response');
  });

  it('fetches a fresh CSRF token before logout', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf-value' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ logout_url: 'https://id.example/logout' }), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    await expect(logout()).resolves.toBe('https://id.example/logout');
    expect(httpValidator('#/$defs/emptyRequest')({})).toBe(true);
    expect(fetchMock.mock.calls[0]?.[0]).toBe(schema['x-endpoints'].csrf.path);
    const [path, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(path).toBe(schema['x-endpoints'].logout.path);
    expect(init.method).toBe(schema['x-endpoints'].logout.method);
    expect(new Headers(init.headers).get('X-CSRF-Token')).toBe('csrf-value');
    expect(init.credentials).toBe('same-origin');
    expect(httpValidator('#/$defs/logoutResponse')({ logout_url: 'https://id.example/logout' })).toBe(true);
    const assign = vi.fn();
    navigateToLogout('https://id.example/logout', { assign });
    expect(assign).toHaveBeenCalledWith('https://id.example/logout');
  });

  it('rejects unsafe upstream logout responses', async () => {
    for (const logout_url of ['http://id.example/logout', 'https://user@id.example/logout', 'https://id.example/logout#fragment']) {
      const fetchMock = vi.fn()
        .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
        .mockResolvedValueOnce(new Response(JSON.stringify({ logout_url }), { status: 200 }));
      vi.stubGlobal('fetch', fetchMock);
      await expect(logout()).rejects.toThrow('Invalid logout response');
    }
  });
});
