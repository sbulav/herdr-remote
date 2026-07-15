import { describe, expect, it } from 'vitest';

function luminance(hex: string): number {
  const channels = hex.match(/[a-f\d]{2}/gi)?.map((value) => Number.parseInt(value, 16) / 255) ?? [];
  const linear = channels.map((value) => value <= 0.03928 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
  return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!;
}

function contrast(foreground: string, background: string): number {
  const first = luminance(foreground);
  const second = luminance(background);
  return (Math.max(first, second) + 0.05) / (Math.min(first, second) + 0.05);
}

describe('accessible color palette', () => {
  it.each([
    ['primary text', '#f5f7f8', '#111418'],
    ['muted text', '#aeb8c2', '#171b20'],
    ['link and focus', '#78b7ff', '#171b20'],
    ['warning text', '#fff1c9', '#453513'],
    ['danger text', '#ffe7e7', '#481f23'],
  ])('%s meets WCAG AA text contrast', (_name, foreground, background) => {
    expect(contrast(foreground, background)).toBeGreaterThanOrEqual(4.5);
  });
});
