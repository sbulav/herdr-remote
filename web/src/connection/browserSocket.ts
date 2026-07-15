import type { InboundMessage, OutboundMessage } from '../protocol/types';
import { validateInbound, validateOutbound } from '../protocol/validate';

const MAX_FRAME_BYTES = 256 * 1024;

export function reconnectDelay(attempt: number, random = Math.random): number {
  const ceiling = Math.min(60_000, 1_000 * 2 ** Math.min(attempt, 16));
  return Math.round(1_000 + random() * Math.max(0, ceiling - 1_000));
}

export function browserSocketUrl(location: Pick<Location, 'protocol' | 'host'> = window.location): string {
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${protocol}//${location.host}/v1/browser/ws`;
}

interface BrowserSocketCallbacks {
  onConnecting(reconnect: boolean): void;
  onOpen(): void;
  onMessage(message: InboundMessage): boolean;
  onClose(offline: boolean): void;
  onProtocolFailure(): void;
}

export class BrowserSocket {
  readonly #callbacks: BrowserSocketCallbacks;
  readonly #url: string;
  #socket: WebSocket | null = null;
  #timer: number | null = null;
  #attempt = 0;
  #stopped = true;
  #receivedSnapshot = false;

  constructor(callbacks: BrowserSocketCallbacks, url = browserSocketUrl()) {
    this.#callbacks = callbacks;
    this.#url = url;
  }

  start(): void {
    if (!this.#stopped) return;
    this.#stopped = false;
    window.addEventListener('online', this.#handleOnline);
    window.addEventListener('offline', this.#handleOffline);
    this.#connect(false);
  }

  stop(): void {
    this.#stopped = true;
    window.removeEventListener('online', this.#handleOnline);
    window.removeEventListener('offline', this.#handleOffline);
    if (this.#timer !== null) window.clearTimeout(this.#timer);
    this.#timer = null;
    this.#socket?.close(1000, 'client shutdown');
    this.#socket = null;
  }

  refresh(): void {
    if (this.#stopped) return;
    if (this.#timer !== null) window.clearTimeout(this.#timer);
    this.#timer = null;
    this.#attempt = 0;
    const socket = this.#socket;
    this.#socket = null;
    this.#receivedSnapshot = false;
    socket?.close(1000, 'operator refresh');
    this.#connect(true);
  }

  send(message: OutboundMessage): boolean {
    if (!this.#receivedSnapshot || this.#socket?.readyState !== WebSocket.OPEN) return false;
    const validated = validateOutbound(message);
    this.#socket.send(JSON.stringify(validated));
    return true;
  }

  #connect(reconnect: boolean): void {
    if (this.#stopped) return;
    if (!navigator.onLine) {
      this.#callbacks.onClose(true);
      return;
    }
    this.#callbacks.onConnecting(reconnect);
    this.#receivedSnapshot = false;
    const socket = new WebSocket(this.#url);
    this.#socket = socket;
    socket.addEventListener('open', () => {
      if (socket !== this.#socket) return;
      this.#callbacks.onOpen();
    });
    socket.addEventListener('message', (event: MessageEvent<unknown>) => this.#receive(socket, event.data));
    socket.addEventListener('close', () => {
      if (socket !== this.#socket || this.#stopped) return;
      this.#socket = null;
      const offline = !navigator.onLine;
      this.#callbacks.onClose(offline);
      if (!offline) this.#scheduleReconnect();
    });
    socket.addEventListener('error', () => socket.close());
  }

  #receive(socket: WebSocket, data: unknown): void {
    if (socket !== this.#socket) return;
    if (typeof data !== 'string' || new TextEncoder().encode(data).byteLength > MAX_FRAME_BYTES) {
      this.#callbacks.onProtocolFailure();
      socket.close(1009, 'invalid frame');
      return;
    }
    try {
      const message = validateInbound(JSON.parse(data) as unknown);
      if (!this.#receivedSnapshot && message.type !== 'session.snapshot') throw new Error('Snapshot required');
      const accepted = this.#callbacks.onMessage(message);
      if (message.type === 'session.snapshot') {
        if (!accepted) {
          socket.close(1007, 'rejected state snapshot');
          return;
        }
        this.#receivedSnapshot = true;
        this.#attempt = 0;
      }
    } catch {
      this.#callbacks.onProtocolFailure();
      socket.close(1007, 'invalid protocol message');
    }
  }

  #scheduleReconnect(): void {
    if (this.#stopped || this.#timer !== null) return;
    const delay = reconnectDelay(this.#attempt);
    this.#attempt += 1;
    this.#timer = window.setTimeout(() => {
      this.#timer = null;
      if (this.#socket) return;
      this.#connect(true);
    }, delay);
  }

  #handleOnline = (): void => {
    if (this.#stopped || this.#socket) return;
    this.#attempt = 0;
    this.#connect(true);
  };

  #handleOffline = (): void => {
    if (this.#timer !== null) window.clearTimeout(this.#timer);
    this.#timer = null;
    if (this.#socket) this.#socket.close();
    else this.#callbacks.onClose(true);
  };
}
