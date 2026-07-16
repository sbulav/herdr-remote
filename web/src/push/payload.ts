import { isExactObject } from '../api/http';

export interface PushPayload {
  event_id: string;
  kind: 'agent_state_changed' | 'connector_state_changed' | 'test';
}

const uuidV7 = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export function parsePushPayload(value: unknown): PushPayload | null {
  if (!isExactObject(value, ['event_id', 'kind'])) return null;
  if (typeof value.event_id !== 'string' || !uuidV7.test(value.event_id)) return null;
  if (value.kind !== 'agent_state_changed' && value.kind !== 'connector_state_changed' && value.kind !== 'test') return null;
  return value as unknown as PushPayload;
}

export class EventDeduplicator {
  readonly #ids = new Set<string>();
  readonly #limit: number;

  constructor(limit = 128) {
    this.#limit = limit;
  }

  accept(id: string): boolean {
    if (this.#ids.has(id)) return false;
    this.#ids.add(id);
    if (this.#ids.size > this.#limit) {
      const oldest = this.#ids.values().next().value as string | undefined;
      if (oldest) this.#ids.delete(oldest);
    }
    return true;
  }
}
