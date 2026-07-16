import { describe, expect, it } from 'vitest';
import fixture from '../../tests/fixtures/http_api_v1.json';
import { httpValidator as validator, schema } from './http-schema';

describe('checked same-origin HTTP contract', () => {
  it('validates every canonical request and response example', () => {
    expect(Object.keys(fixture.cases).sort()).toEqual(Object.keys(schema['x-endpoints']).sort());
    for (const [name, value] of Object.entries(fixture.cases)) {
      const endpoint = schema['x-endpoints'][name as keyof typeof schema['x-endpoints']];
      if (endpoint.request === null) expect(value.request).toBeNull();
      else expect(validator(endpoint.request)(value.request), `${name} request`).toBe(true);
      const response = endpoint.responses[String(value.status) as keyof typeof endpoint.responses];
      expect(response).toBeDefined();
      expect(validator(response!)(value.response), `${name} response`).toBe(true);
    }
  });

  it('rejects unknown, missing, wrong-type, and out-of-bounds fields', () => {
    expect(validator('#/$defs/sessionResponse')({ authenticated: true, operator: { display_name: 'operator' }, push_public_key: null, logout_url: 'https://id.example/logout', issuer: 'secret' })).toBe(false);
    expect(validator('#/$defs/csrfResponse')({ token: 'short' })).toBe(false);
    expect(validator('#/$defs/pushSubscription')({ endpoint: 'http://push.example', keys: { p256dh: 'key', auth: 'auth' } })).toBe(false);
    expect(validator('#/$defs/pushSubscription')({ endpoint: 'https://127.0.0.1/push', keys: { p256dh: 'key', auth: 'auth' } })).toBe(false);
    expect(validator('#/$defs/pushSubscription')({ endpoint: 'https://user@push.example/push', keys: { p256dh: 'key', auth: 'auth' } })).toBe(false);
    expect(validator('#/$defs/pushSubscription')({ endpoint: 'https://push.example:8443/push', keys: { p256dh: 'key', auth: 'auth' } })).toBe(false);
    expect(validator('#/$defs/pushSubscription')({ endpoint: 'https://push.example/push#fragment', keys: { p256dh: 'key', auth: 'auth' } })).toBe(false);
    expect(validator('#/$defs/pushSubscription')({ endpoint: 'https://push.example', keys: { p256dh: 'key' } })).toBe(false);
    expect(validator('#/$defs/endpointsRequest')({ endpoints: [] })).toBe(false);
    expect(validator('#/$defs/endpointsRequest')({ endpoints: ['https://push.example/old', 'https://push.example/old'] })).toBe(false);
    const replacement = { subscription: { endpoint: 'https://push.example/new', keys: { p256dh: 'key', auth: 'auth' } } };
    expect(validator('#/$defs/replaceRequest')({ ...replacement, source_endpoints: [] })).toBe(false);
    expect(validator('#/$defs/replaceRequest')({ ...replacement, source_endpoints: ['https://push.example/old', 'https://push.example/old'] })).toBe(false);
    expect(validator('#/$defs/emptyRequest')({ unexpected: true })).toBe(false);
    expect(validator('#/$defs/logoutResponse')({ logout_url: 'http://id.example/logout' })).toBe(false);
    expect(validator('#/$defs/logoutResponse')({ logout_url: 'https://user@id.example/logout' })).toBe(false);
  });

  it('requires Origin and CSRF metadata on every mutation', () => {
    for (const endpoint of Object.values(schema['x-endpoints'])) {
      if (endpoint.method === 'GET') {
        expect(endpoint.origin_required).toBe(false);
      } else {
        expect(endpoint.origin_required).toBe(true);
        expect(endpoint.csrf_required).toBe(true);
      }
    }
  });
});
