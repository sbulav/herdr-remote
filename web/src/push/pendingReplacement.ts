const DATABASE = 'herdr-remote-push-v1';
const STORE = 'pending-replacements';
const KEY = 'current';
const DATABASE_VERSION = 3;

export const MAX_PENDING_PUSH_SOURCES = 16;
const MAX_ENDPOINT_LENGTH = 2048;

export interface PendingReplacementState {
  source_endpoints: string[];
  opted_out: boolean;
}

interface PendingRecord extends PendingReplacementState {
  key: typeof KEY;
}

interface LegacyPendingRecord {
  key: typeof KEY;
  old_endpoint?: unknown;
  transitions?: unknown;
}

export class PendingReplacementOverflowError extends Error {
  constructor() {
    super(`Pending push replacement sources reached their ${MAX_PENDING_PUSH_SOURCES}-endpoint limit`);
    this.name = 'PendingReplacementOverflowError';
  }
}

export interface PendingReplacementStore {
  load(): Promise<PendingReplacementState>;
  append(sourceEndpoint: string): Promise<void>;
  remove(sourceEndpoints: readonly string[]): Promise<void>;
  setOptedOut(optedOut: boolean): Promise<void>;
}

function validateSources(value: unknown): string[] {
  if (!Array.isArray(value)) throw new Error('Pending push replacement sources are malformed');
  if (value.length > MAX_PENDING_PUSH_SOURCES) throw new PendingReplacementOverflowError();
  if (!value.every((endpoint) => typeof endpoint === 'string' && endpoint.length > 0 && endpoint.length <= MAX_ENDPOINT_LENGTH)) {
    throw new Error('Pending push replacement sources are malformed');
  }
  const sources = value as string[];
  if (new Set(sources).size !== sources.length) throw new Error('Pending push replacement sources contain duplicates');
  return [...sources];
}

function decodeState(value: unknown): PendingReplacementState {
  if (value === undefined) return { source_endpoints: [], opted_out: false };
  if (typeof value !== 'object' || value === null) throw new Error('Pending push replacement record is malformed');
  const record = value as Partial<PendingRecord>;
  if (record.key !== KEY) throw new Error('Pending push replacement record is malformed');
  if (typeof record.opted_out !== 'boolean') throw new Error('Pending push replacement record is malformed');
  return { source_endpoints: validateSources(record.source_endpoints), opted_out: record.opted_out };
}

function migrateState(value: unknown): PendingReplacementState {
  if (value === undefined) return { source_endpoints: [], opted_out: false };
  if (typeof value !== 'object' || value === null) throw new Error('Pending push replacement record is malformed');
  const record = value as LegacyPendingRecord & Partial<PendingRecord>;
  if (record.key !== KEY) throw new Error('Pending push replacement record is malformed');
  const optedOut = typeof record.opted_out === 'boolean' ? record.opted_out : false;
  if (Array.isArray(record.source_endpoints)) return { source_endpoints: validateSources(record.source_endpoints), opted_out: optedOut };
  if (typeof record.old_endpoint === 'string') return { source_endpoints: validateSources([record.old_endpoint]), opted_out: optedOut };
  if (!Array.isArray(record.transitions)) throw new Error('Pending push replacement record is malformed');
  const sources = record.transitions.map((transition) => {
    if (typeof transition !== 'object' || transition === null || !('source_endpoint' in transition)) {
      throw new Error('Pending push replacement record is malformed');
    }
    return (transition as { source_endpoint: unknown }).source_endpoint;
  });
  return { source_endpoints: validateSources(sources), opted_out: optedOut };
}

function openDatabase(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DATABASE, DATABASE_VERSION);
    request.addEventListener('upgradeneeded', (event) => {
      const transaction = request.transaction;
      if (!transaction) return;
      const objectStore = request.result.objectStoreNames.contains(STORE)
        ? transaction.objectStore(STORE)
        : request.result.createObjectStore(STORE, { keyPath: 'key' });
      if ((event as IDBVersionChangeEvent).oldVersion >= DATABASE_VERSION) return;
      const get = objectStore.get(KEY);
      get.addEventListener('success', () => {
        try {
          const state = migrateState(get.result);
          objectStore.put({ key: KEY, source_endpoints: state.source_endpoints, opted_out: state.opted_out } satisfies PendingRecord);
        } catch {
          transaction.abort();
        }
      });
      get.addEventListener('error', () => transaction.abort());
    });
    request.addEventListener('success', () => resolve(request.result));
    request.addEventListener('error', () => reject(request.error ?? new Error('Unable to open push replacement database')));
    request.addEventListener('blocked', () => reject(new Error('Push replacement database upgrade was blocked')));
  });
}

function requestResult<T>(request: IDBRequest<T>, message: string): Promise<T> {
  return new Promise((resolve, reject) => {
    request.addEventListener('success', () => resolve(request.result));
    request.addEventListener('error', () => reject(request.error ?? new Error(message)));
  });
}

function transactionDone(transaction: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    transaction.addEventListener('complete', () => resolve());
    transaction.addEventListener('abort', () => reject(transaction.error ?? new Error('Push replacement transaction aborted')));
    transaction.addEventListener('error', () => reject(transaction.error ?? new Error('Push replacement transaction failed')));
  });
}

async function readState(): Promise<PendingReplacementState> {
  const database = await openDatabase();
  try {
    const transaction = database.transaction(STORE, 'readonly');
    const done = transactionDone(transaction);
    const result = await requestResult(transaction.objectStore(STORE).get(KEY), 'Unable to read pending push replacements');
    await done;
    return decodeState(result);
  } finally {
    database.close();
  }
}

async function updateState(mutator: (state: PendingReplacementState) => void): Promise<void> {
  const database = await openDatabase();
  try {
    const transaction = database.transaction(STORE, 'readwrite');
    const done = transactionDone(transaction);
    const objectStore = transaction.objectStore(STORE);
    const state = decodeState(await requestResult(objectStore.get(KEY), 'Unable to read pending push replacements'));
    mutator(state);
    objectStore.put({ key: KEY, source_endpoints: state.source_endpoints, opted_out: state.opted_out } satisfies PendingRecord);
    await done;
  } finally {
    database.close();
  }
}

export const pendingReplacementStore: PendingReplacementStore = {
  load: readState,

  async append(sourceEndpoint) {
    validateSources([sourceEndpoint]);
    await updateState((state) => {
      if (state.source_endpoints.includes(sourceEndpoint)) return;
      if (state.source_endpoints.length >= MAX_PENDING_PUSH_SOURCES) throw new PendingReplacementOverflowError();
      state.source_endpoints.push(sourceEndpoint);
    });
  },

  async remove(sourceEndpoints) {
    const submitted = validateSources([...sourceEndpoints]);
    const submittedSet = new Set(submitted);
    await updateState((state) => {
      state.source_endpoints = state.source_endpoints.filter((endpoint) => !submittedSet.has(endpoint));
    });
  },

  async setOptedOut(optedOut) {
    await updateState((state) => {
      state.opted_out = optedOut;
    });
  },
};
