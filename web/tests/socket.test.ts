import { afterEach, describe, expect, it, vi } from 'vitest';
import lifecycle from '../../tests/fixtures/browser_protocol_v1.ndjson?raw';
import { BrowserSocket, browserSocketUrl, reconnectDelay } from '../src/connection/browserSocket';
import { validateMessage } from '../src/protocol/validate';
import type { OutboundMessage } from '../src/protocol/types';

const frames = lifecycle.trim().split('\n').map((line) => JSON.parse(line) as unknown);

class FakeWebSocket extends EventTarget {
  static readonly OPEN = 1;
  static instances: FakeWebSocket[] = [];
  readyState = 0;
  sent: string[] = [];
  constructor(readonly url: string) {
    super();
    FakeWebSocket.instances.push(this);
  }
  open() {
    this.readyState = FakeWebSocket.OPEN;
    this.dispatchEvent(new Event('open'));
  }
  message(value: unknown) {
    this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(value) }));
  }
  send(value: string) {
    this.sent.push(value);
  }
  close() {
    this.readyState = 3;
    this.dispatchEvent(new Event('close'));
  }
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('browser socket security and reconnect policy', () => {
  it('derives only the fixed same-origin endpoint', () => {
    expect(browserSocketUrl({ protocol: 'https:', host: 'control.example' })).toBe('wss://control.example/v1/browser/ws');
    expect(browserSocketUrl({ protocol: 'http:', host: 'localhost:4173' })).toBe('ws://localhost:4173/v1/browser/ws');
  });

  it('uses full jitter bounded from one to sixty seconds', () => {
    expect(reconnectDelay(0, () => 0)).toBe(1000);
    expect(reconnectDelay(4, () => 0.5)).toBe(8500);
    expect(reconnectDelay(20, () => 1)).toBe(60000);
  });

  it('blocks sends until each new socket accepts its first snapshot', () => {
    FakeWebSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeWebSocket);
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true });
    const events: string[] = [];
    const connection = new BrowserSocket({
      onConnecting: () => events.push('connecting'),
      onOpen: () => events.push('open'),
      onMessage: () => true,
      onClose: () => events.push('close'),
      onProtocolFailure: () => events.push('invalid'),
    });
    const outbound = validateMessage(frames[2]) as OutboundMessage;
    connection.start();
    const first = FakeWebSocket.instances.at(-1)!;
    first.open();
    expect(connection.send(outbound)).toBe(false);
    first.message(frames[0]);
    expect(connection.send(outbound)).toBe(true);

    connection.refresh();
    expect(events.at(-1)).toBe('connecting');
    expect(connection.send(outbound)).toBe(false);
    connection.stop();
  });

  it('does not reset exponential backoff for sockets that open without a snapshot', () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(1);
    FakeWebSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeWebSocket);
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true });
    const connection = new BrowserSocket({
      onConnecting: () => undefined,
      onOpen: () => undefined,
      onMessage: () => true,
      onClose: () => undefined,
      onProtocolFailure: () => undefined,
    });
    connection.start();
    FakeWebSocket.instances[0]!.open();
    FakeWebSocket.instances[0]!.close();
    vi.advanceTimersByTime(1000);
    expect(FakeWebSocket.instances).toHaveLength(2);

    FakeWebSocket.instances[1]!.open();
    FakeWebSocket.instances[1]!.close();
    vi.advanceTimersByTime(1999);
    expect(FakeWebSocket.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeWebSocket.instances).toHaveLength(3);

    FakeWebSocket.instances[2]!.open();
    FakeWebSocket.instances[2]!.message(frames[0]);
    FakeWebSocket.instances[2]!.close();
    vi.advanceTimersByTime(1000);
    expect(FakeWebSocket.instances).toHaveLength(4);
    connection.stop();
  });

  it('refreshes during backoff with one socket and cancels the stale reconnect timer', () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(1);
    FakeWebSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeWebSocket);
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true });
    const connection = new BrowserSocket({
      onConnecting: () => undefined,
      onOpen: () => undefined,
      onMessage: () => true,
      onClose: () => undefined,
      onProtocolFailure: () => undefined,
    });
    connection.start();
    FakeWebSocket.instances[0]!.open();
    FakeWebSocket.instances[0]!.close();

    connection.refresh();
    expect(FakeWebSocket.instances).toHaveLength(2);
    const refreshedSocket = FakeWebSocket.instances[1]!;
    refreshedSocket.open();
    refreshedSocket.message(frames[0]);

    vi.advanceTimersByTime(60_000);
    expect(FakeWebSocket.instances).toHaveLength(2);
    expect(refreshedSocket.readyState).toBe(FakeWebSocket.OPEN);
    connection.stop();
  });
});
