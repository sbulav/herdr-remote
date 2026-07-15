import { expect, test } from '@playwright/test';

const snapshot = {
  protocol: 1,
  message_id: '019f64ca-3000-7000-8000-000000000001',
  type: 'session.snapshot',
  sent_at: '2026-07-15T08:00:00Z',
  body: {
    session_id: '019f64ca-3000-7000-8000-000000000101', state_epoch: '019f64ca-3000-7000-8000-000000000103', sequence: 0, server_time: '2026-07-15T08:00:00Z',
    hosts: [{ host_id: '019f64ca-1000-7000-8000-000000000002', display_name: 'workstation', status: 'connected', instances: [{ instance_id: 'default', connector_epoch: '019f64ca-3000-7000-8000-000000000110', herdr_version: '0.7.3', herdr_protocol: 16, status: 'online', capabilities: ['read.v1', 'output.subscribe.v1'], agents: [{ terminal_id: 'term_1', agent: 'opencode', display_name: 'Review agent', status: 'blocked', project: 'enterprise', connector_epoch: '019f64ca-3000-7000-8000-000000000110', agent_generation: 1, herdr_input_revision: 0 }] }] }],
  },
};

test.beforeEach(async ({ page }) => {
  await page.route('**/api/v1/session', (route) => route.fulfill({ json: { authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null } }));
  await page.routeWebSocket('**/v1/browser/ws', (socket) => socket.send(JSON.stringify(snapshot)));
});

test('supports phone and tablet layouts with safe read-only controls', async ({ page }, testInfo) => {
  await page.goto('/agents');
  const skipLink = page.getByRole('link', { name: 'Skip to agents' });
  await skipLink.focus();
  await expect(skipLink).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.locator('#main-content')).toBeFocused();
  const agent = page.getByRole('button', { name: /Review agent/ });
  await expect(agent).toBeVisible();
  await agent.click();
  await expect(page.getByText('Read-only.')).toBeVisible();
  await expect(page.getByLabel('Text to send')).toBeDisabled();
  const horizontalOverflow = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth);
  expect(horizontalOverflow).toBe(false);
  if (testInfo.project.name.startsWith('phone')) await expect(page.getByRole('button', { name: 'Back to agents' })).toBeVisible();
  else await expect(agent).toBeVisible();
});
