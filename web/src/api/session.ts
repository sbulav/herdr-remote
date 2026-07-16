import { apiFetch, isExactObject, mutatingFetch } from './http';

export interface SessionBootstrap {
  authenticated: true;
  operator: { display_name: string };
  push_public_key: string | null;
  logout_url: string;
}

function parseSession(value: unknown): SessionBootstrap {
  if (!isExactObject(value, ['authenticated', 'operator', 'push_public_key', 'logout_url'])) throw new Error('Invalid session response');
  if (value.authenticated !== true) throw new Error('Invalid session response');
  if (value.push_public_key !== null && typeof value.push_public_key !== 'string') throw new Error('Invalid session response');
  if (!isExactObject(value.operator, ['display_name']) || typeof value.operator.display_name !== 'string') {
    throw new Error('Invalid session response');
  }
  if (typeof value.logout_url !== 'string' || !isValidLogoutURL(value.logout_url)) throw new Error('Invalid session response');
  return value as unknown as SessionBootstrap;
}

export async function bootstrapSession(): Promise<SessionBootstrap> {
  const response = await apiFetch('/api/v1/session');
  return parseSession(await response.json());
}

export async function logout(): Promise<string> {
  const response = await mutatingFetch('/auth/logout', { method: 'POST', body: '{}' });
  const value: unknown = await response.json();
  if (!isExactObject(value, ['logout_url']) || typeof value.logout_url !== 'string') {
    throw new Error('Invalid logout response');
  }
  if (!isValidLogoutURL(value.logout_url)) throw new Error('Invalid logout response');
  return value.logout_url;
}

function isValidLogoutURL(value: string): boolean {
  try {
    const logoutURL = new URL(value);
    return logoutURL.protocol === 'https:' && logoutURL.username === '' && logoutURL.password === '' && logoutURL.hash === '';
  } catch {
    return false;
  }
}

export function navigateToLogout(logoutURL: string, location: Pick<Location, 'assign'> = window.location): void {
  location.assign(logoutURL);
}
