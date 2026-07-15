import { apiFetch, isExactObject, mutatingFetch } from './http';

export interface SessionBootstrap {
  authenticated: boolean;
  operator: { display_name: string } | null;
  push_public_key: string | null;
}

function parseSession(value: unknown): SessionBootstrap {
  if (!isExactObject(value, ['authenticated', 'operator', 'push_public_key'])) throw new Error('Invalid session response');
  if (typeof value.authenticated !== 'boolean') throw new Error('Invalid session response');
  if (value.push_public_key !== null && typeof value.push_public_key !== 'string') throw new Error('Invalid session response');
  if (value.operator !== null) {
    if (!isExactObject(value.operator, ['display_name']) || typeof value.operator.display_name !== 'string') {
      throw new Error('Invalid session response');
    }
  }
  if (value.authenticated !== (value.operator !== null)) throw new Error('Invalid session response');
  return value as unknown as SessionBootstrap;
}

export async function bootstrapSession(): Promise<SessionBootstrap> {
  const response = await apiFetch('/api/v1/session');
  return parseSession(await response.json());
}

export async function logout(): Promise<void> {
  await mutatingFetch('/auth/logout', { method: 'POST', body: '{}' });
}
