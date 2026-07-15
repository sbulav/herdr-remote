export class ApiError extends Error {
  readonly status: number;

  constructor(status: number) {
    super(`Same-origin API request failed (${status})`);
    this.name = 'ApiError';
    this.status = status;
  }
}

export async function apiFetch(path: string, init: RequestInit = {}): Promise<Response> {
  if (!path.startsWith('/')) throw new Error('API paths must be same-origin');
  const headers = new Headers(init.headers);
  headers.set('Accept', 'application/json');
  const response = await fetch(path, {
    ...init,
    headers,
    credentials: 'same-origin',
    cache: 'no-store',
    redirect: 'error',
  });
  if (!response.ok) throw new ApiError(response.status);
  return response;
}

export async function csrfToken(): Promise<string> {
  const response = await apiFetch('/api/v1/csrf');
  const value: unknown = await response.json();
  if (!isExactObject(value, ['token']) || typeof value.token !== 'string' || value.token.length < 1) {
    throw new Error('Invalid CSRF response');
  }
  return value.token;
}

export async function mutatingFetch(path: string, init: RequestInit): Promise<Response> {
  const token = await csrfToken();
  const headers = new Headers(init.headers);
  headers.set('Content-Type', 'application/json');
  headers.set('X-CSRF-Token', token);
  return apiFetch(path, { ...init, headers });
}

export function isExactObject(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  return actual.length === expected.length && actual.every((key, index) => key === expected[index]);
}
