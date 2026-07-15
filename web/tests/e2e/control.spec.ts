import { expect, test, type WebSocketRoute } from '@playwright/test';

const target = {
  host_id: '019f64ca-1000-7000-8000-000000000002',
  instance_id: 'default',
  terminal_id: 'term_1',
};
const sessionId = '019f64ca-3000-7000-8000-000000000101';
const stateEpoch = '019f64ca-3000-7000-8000-000000000103';
const connectorEpoch = '019f64ca-3000-7000-8000-000000000110';

const snapshot = {
  protocol: 1,
  message_id: '019f64ca-3000-7000-8000-000000000001',
  type: 'session.snapshot',
  sent_at: '2026-07-15T08:00:00Z',
  body: {
    session_id: sessionId,
    state_epoch: stateEpoch,
    sequence: 0,
    server_time: '2026-07-15T08:00:00Z',
    hosts: [{
      host_id: target.host_id,
      display_name: 'workstation',
      status: 'connected',
      instances: [{
        instance_id: target.instance_id,
        connector_epoch: connectorEpoch,
        herdr_version: '1.0.0',
        herdr_protocol: 17,
        status: 'online',
        capabilities: ['read.v1', 'output.subscribe.v1', 'prompt.snapshot.v1', 'checked_input.v1', 'prompt.respond.v1'],
        agents: [{
          terminal_id: target.terminal_id,
          agent: 'opencode',
          display_name: 'Writable agent',
          status: 'blocked',
          project: 'enterprise',
          connector_epoch: connectorEpoch,
          agent_generation: 1,
          herdr_input_revision: 42,
        }],
      }],
    }],
  },
};

const prompt = {
  protocol: 1,
  message_id: '019f64ca-3000-7000-8000-000000000002',
  type: 'prompt.snapshot',
  sent_at: '2026-07-15T08:00:01Z',
  body: {
    session_id: sessionId,
    target,
    state_epoch: stateEpoch,
    state_sequence: 0,
    connector_epoch: connectorEpoch,
    agent_generation: 1,
    herdr_input_revision: 42,
    herdr_content_hash: `sha256:${'1'.repeat(64)}`,
    fingerprint: `sha256:${'2'.repeat(64)}`,
    excerpt: 'Choose a safe response',
    excerpt_truncated: false,
    adapter_version: '1.0.0',
    options: [{ id: 'allow_once', label: 'Allow once' }],
  },
};

function envelope(type: string, body: unknown, suffix: string) {
  return {
    protocol: 1,
    message_id: `019f64ca-3000-7000-8000-0000000000${suffix}`,
    type,
    sent_at: '2026-07-15T08:00:02Z',
    body,
  };
}

test('executes writable prompt, complete key, and interrupt controls', async ({ page }) => {
  const requests: Array<Record<string, any>> = [];
  await page.route('**/api/v1/session', (route) => route.fulfill({ json: { authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null } }));
  await page.routeWebSocket('**/v1/browser/ws', (socket) => {
    socket.onMessage((raw) => {
      const message = JSON.parse(String(raw)) as Record<string, any>;
      if (message.type !== 'action.request') return;
      requests.push(message);
      const operationType = message.body.operation.type as string;
      const result = operationType === 'prompt.respond'
        ? { herdr_acknowledged: true, option_id: message.body.operation.option_id }
        : { herdr_acknowledged: true };
      socket.send(JSON.stringify(envelope('action.result', {
        session_id: sessionId,
        action_id: message.body.action_id,
        operation_type: operationType,
        status: 'succeeded',
        code: null,
        result,
      }, String(requests.length + 2).padStart(2, '0'))));
    });
    socket.send(JSON.stringify(snapshot));
    socket.send(JSON.stringify(prompt));
  });
  await page.goto('/agents');
  await page.getByRole('button', { name: /Writable agent/ }).click();

  await page.getByRole('button', { name: 'Allow once' }).click();
  await expect.poll(() => requests.at(-1)?.body.operation.type).toBe('prompt.respond');
  await page.getByRole('button', { name: 'Shift+Tab' }).click();
  await expect.poll(() => requests.at(-1)?.body.operation.keys?.[0]).toBe('shift+tab');
  await page.getByRole('button', { name: 'Interrupt agent' }).click();
  await expect.poll(() => requests.at(-1)?.body.operation.type).toBe('agent.interrupt');

  for (const name of ['Page Up', 'Page Down', 'Home', 'End', 'Backspace', 'Delete']) {
    await expect(page.getByRole('button', { name, exact: true })).toBeVisible();
  }
});

test('resubscribes output intent after a binding change and hides stale reads', async ({ page }) => {
  const subscriptions: string[] = [];
  let serverSocket: WebSocketRoute | null = null;
  await page.route('**/api/v1/session', (route) => route.fulfill({ json: { authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null } }));
  await page.routeWebSocket('**/v1/browser/ws', (socket) => {
    serverSocket = socket;
    socket.onMessage((raw) => {
      const message = JSON.parse(String(raw)) as Record<string, any>;
      if (message.type === 'output.subscribe') subscriptions.push(message.body.subscription_id as string);
      if (message.type !== 'action.request' || message.body.operation.type !== 'agent.read') return;
      socket.send(JSON.stringify(envelope('action.result', {
        session_id: sessionId,
        action_id: message.body.action_id,
        operation_type: 'agent.read',
        status: 'succeeded',
        code: null,
        result: {
          state_epoch: stateEpoch,
          connector_epoch: connectorEpoch,
          agent_generation: 1,
          herdr_input_revision: 42,
          text: 'stale terminal content must not render',
          truncated: false,
          content_revision: `sha256:${'3'.repeat(64)}`,
        },
      }, '20')));
    });
    socket.send(JSON.stringify(snapshot));
  });
  await page.goto('/agents');
  await page.getByRole('button', { name: /Writable agent/ }).click();
  await expect.poll(() => subscriptions.length).toBeGreaterThan(0);
  const initialSubscriptionCount = subscriptions.length;
  const initialSubscriptionId = subscriptions.at(-1);

  serverSocket!.send(JSON.stringify(envelope('state.delta', {
    session_id: sessionId,
    state_epoch: stateEpoch,
    sequence: 1,
    changes: [{
      operation: 'agent.upsert',
      target,
      agent: {
        agent: 'opencode', display_name: 'Writable agent', status: 'blocked', project: 'enterprise',
        connector_epoch: connectorEpoch, agent_generation: 2, herdr_input_revision: 43,
      },
    }],
  }, '21')));
  await expect.poll(() => subscriptions.length).toBeGreaterThan(initialSubscriptionCount);
  expect(subscriptions.at(-1)).not.toBe(initialSubscriptionId);

  await page.getByRole('button', { name: 'Read now' }).click();
  await expect(page.getByText(/Agent state changed before the action could run/)).toBeVisible();
  await expect(page.getByText('stale terminal content must not render')).toHaveCount(0);
});
