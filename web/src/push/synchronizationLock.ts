const LOCK_NAME = 'herdr-remote-push-synchronization';

export class PushSynchronizationUnsupportedError extends Error {
  constructor() {
    super('Push synchronization requires Web Locks support');
    this.name = 'PushSynchronizationUnsupportedError';
  }
}

export function pushSynchronizationSupported(): boolean {
  return typeof navigator !== 'undefined' && typeof navigator.locks?.request === 'function';
}

export async function withPushSynchronizationLock<T>(operation: () => Promise<T>): Promise<T> {
  if (!pushSynchronizationSupported()) throw new PushSynchronizationUnsupportedError();
  return navigator.locks.request(LOCK_NAME, { mode: 'exclusive' }, operation);
}
