import type { Envelope, OutboundMessage } from './types';
import { validateOutbound } from './validate';

export function uuidv7(now = Date.now(), random = crypto.getRandomValues(new Uint8Array(10))): string {
  const timestamp = BigInt(now);
  const bytes = new Uint8Array(16);
  for (let index = 5; index >= 0; index -= 1) {
    bytes[index] = Number((timestamp >> BigInt((5 - index) * 8)) & 0xffn);
  }
  bytes.set(random, 6);
  bytes[6] = (bytes[6]! & 0x0f) | 0x70;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export function makeMessage<T extends OutboundMessage['type'], B>(type: T, body: B): Extract<OutboundMessage, { type: T }> {
  const message: Envelope<T, B> = {
    protocol: 1,
    message_id: uuidv7(),
    type,
    sent_at: new Date().toISOString(),
    body,
  };
  return validateOutbound(message) as Extract<OutboundMessage, { type: T }>;
}
