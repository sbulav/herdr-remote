import { describe, expect, it } from 'vitest';
import lifecycle from '../../tests/fixtures/browser_protocol_v1.ndjson?raw';
import failures from '../../tests/fixtures/browser_protocol_v1_failures.json';
import generatedSource from '../src/protocol/generated-validator.ts?raw';
import { validateInbound, validateMessage } from '../src/protocol/validate';

const messages = lifecycle.trim().split('\n').map((line) => JSON.parse(line) as unknown);

describe('browser protocol schema validation', () => {
  it('uses a generated validator without dynamic code evaluation', () => {
    expect(generatedSource).not.toMatch(/require\(|new Function|eval\(/);
  });

  it('accepts every checked-in conformance lifecycle frame', () => {
    for (const message of messages) expect(() => validateMessage(message)).not.toThrow();
  });

  it('rejects every checked-in schema-invalid result combination', () => {
    for (const message of failures.schema_invalid_frames) expect(() => validateMessage(message)).toThrow();
  });

  it('rejects unknown fields, unknown types, and wrong message direction', () => {
    for (const invalid of failures.invalid_frames) expect(() => validateMessage(invalid.frame)).toThrow();
    expect(() => validateInbound(messages[2])).toThrow(/not valid from the control plane/);
  });

  it('enforces logical key and prompt-option uniqueness', () => {
    const snapshot = structuredClone(messages[0]) as Record<string, any>;
    snapshot.body.hosts.push({ ...snapshot.body.hosts[0], display_name: 'duplicate' });
    expect(() => validateMessage(snapshot)).toThrow(/Duplicate host/);

    const prompt = structuredClone(messages[1]) as Record<string, any>;
    prompt.body.options.push({ ...prompt.body.options[0], label: 'Different label' });
    expect(() => validateMessage(prompt)).toThrow(/Duplicate prompt option/);
  });

  it('enforces connector epoch and capability invariants', () => {
    const wrongEpoch = structuredClone(messages[0]) as Record<string, any>;
    wrongEpoch.body.hosts[0].instances[0].agents[0].connector_epoch = '019f64ca-3000-7000-8000-000000000199';
    expect(() => validateMessage(wrongEpoch)).toThrow(/connector epoch/);

    const invalidCapability = structuredClone(messages[0]) as Record<string, any>;
    invalidCapability.body.hosts[0].instances[0].herdr_version = '1.0.0';
    invalidCapability.body.hosts[0].instances[0].capabilities = ['prompt.respond.v1'];
    expect(() => validateMessage(invalidCapability)).toThrow();
  });
});
