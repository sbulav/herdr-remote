import '@testing-library/jest-dom/vitest';
import { afterEach, vi } from 'vitest';
import { cleanup } from '@testing-library/react';

afterEach(() => cleanup());

Object.defineProperty(window, 'matchMedia', {
  configurable: true,
  value: vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: vi.fn(),
    removeListener: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  })),
});

Object.defineProperty(navigator, 'locks', {
  configurable: true,
  value: {
    request: async (_name: string, _options: LockOptions, callback: (lock: Lock) => unknown) => callback({ name: _name, mode: 'exclusive' }),
  },
});

// jsdom has no canvas implementation. Axe probes canvas while evaluating
// icon ligatures, so return null and cover palette contrast independently.
HTMLCanvasElement.prototype.getContext = vi.fn(() => null) as typeof HTMLCanvasElement.prototype.getContext;
