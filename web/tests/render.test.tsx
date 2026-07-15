import { act, render, screen, waitFor } from '@testing-library/react';
import axe from 'axe-core';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { App, StatusBanners } from '../src/ui/App';
import { initialBrowserState } from '../src/state/reducer';

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

class FakeWebSocket extends EventTarget {
  static readonly OPEN = 1;
  static instances: FakeWebSocket[] = [];
  readyState = FakeWebSocket.OPEN;
  sent: string[] = [];
  constructor(readonly url: string) { super(); FakeWebSocket.instances.push(this); }
  send(value: string) { this.sent.push(value); }
  close() { this.readyState = 3; }
  open() { this.dispatchEvent(new Event('open')); }
  message(value: unknown) { this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(value) })); }
}

describe('application rendering and accessibility', () => {
  beforeEach(() => {
    FakeWebSocket.instances = [];
    vi.stubGlobal('WebSocket', FakeWebSocket);
    Object.defineProperty(navigator, 'onLine', { configurable: true, value: true });
  });

  it('renders a secure same-origin sign-in action', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: false, operator: null, push_public_key: null }), { status: 200 })));
    render(<App />);
    const link = await screen.findByRole('link', { name: 'Sign in' });
    expect(link).toHaveAttribute('href', '/auth/login?return_to=%2Fagents');
    expect(document.querySelector('input[type="password"]')).not.toBeInTheDocument();
  });

  it('renders read-only agents without critical axe violations', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null }), { status: 200 })));
    const { container } = render(<App />);
    await waitFor(() => expect(FakeWebSocket.instances.length).toBeGreaterThan(0));
    const socket = FakeWebSocket.instances.at(-1)!;
    await act(async () => { socket.open(); socket.message(snapshot); });
    const agent = await screen.findByRole('button', { name: /Review agent/ });
    await act(async () => agent.click());
    expect(await screen.findByText('Read-only.')).toBeVisible();
    expect(screen.getByLabelText('Text to send')).toBeDisabled();
    const results = await axe.run(container);
    expect(results.violations).toEqual([]);
  });

  it('revalidates the session and returns to login after websocket authorization expires', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ authenticated: false, operator: null, push_public_key: null }), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    render(<App />);
    await waitFor(() => expect(FakeWebSocket.instances.length).toBeGreaterThan(0));
    const socket = FakeWebSocket.instances.at(-1)!;
    await act(async () => {
      socket.open();
      socket.message(snapshot);
      socket.message({
        protocol: 1,
        message_id: '019f64ca-3000-7000-8000-000000000099',
        type: 'protocol.error',
        sent_at: '2026-07-15T08:00:01Z',
        body: { session_id: snapshot.body.session_id, in_reply_to: null, code: 'UNAUTHORIZED', fatal: true },
      });
    });
    expect(await screen.findByRole('link', { name: 'Sign in' })).toBeVisible();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('requires prompt response capability separately from checked input', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null }), { status: 200 })));
    render(<App />);
    await waitFor(() => expect(FakeWebSocket.instances.length).toBeGreaterThan(0));
    const socket = FakeWebSocket.instances.at(-1)!;
    const writable = structuredClone(snapshot);
    writable.body.hosts[0]!.instances[0]!.herdr_version = '1.0.0';
    writable.body.hosts[0]!.instances[0]!.capabilities = ['read.v1', 'prompt.snapshot.v1', 'checked_input.v1'];
    writable.body.hosts[0]!.instances[0]!.agents[0]!.herdr_input_revision = 42;
    const promptMessage = {
      protocol: 1,
      message_id: '019f64ca-3000-7000-8000-000000000098',
      type: 'prompt.snapshot',
      sent_at: '2026-07-15T08:00:01Z',
      body: {
        session_id: snapshot.body.session_id,
        target: { host_id: snapshot.body.hosts[0]!.host_id, instance_id: 'default', terminal_id: 'term_1' },
        state_epoch: snapshot.body.state_epoch,
        state_sequence: 0,
        connector_epoch: snapshot.body.hosts[0]!.instances[0]!.connector_epoch,
        agent_generation: 1,
        herdr_input_revision: 42,
        herdr_content_hash: `sha256:${'1'.repeat(64)}`,
        fingerprint: `sha256:${'2'.repeat(64)}`,
        excerpt: 'Choose',
        excerpt_truncated: false,
        adapter_version: '1.0.0',
        options: [{ id: 'allow_once', label: 'Allow once' }],
      },
    };
    await act(async () => {
      socket.open();
      socket.message(writable);
      socket.message(promptMessage);
    });
    await act(async () => screen.getByRole('button', { name: /Review agent/ }).click());
    expect(screen.getByRole('button', { name: 'Allow once' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Interrupt agent' })).toBeEnabled();
  });

  it('keeps each unresolved action outcome visible', () => {
    render(<StatusBanners requestRefresh={() => undefined} state={{
      ...initialBrowserState,
      actionResults: {
        read: {
          session_id: '', action_id: 'read', operation_type: 'agent.read', status: 'failed',
          code: 'CONNECTION_LOST', result: null, local: true,
        },
        write: {
          session_id: '', action_id: 'write', operation_type: 'agent.send_text', status: 'unknown',
          code: 'OUTCOME_UNKNOWN', result: null, local: true,
        },
      },
    }} />);
    expect(screen.getByText(/read did not finish because the connection was lost/i)).toBeVisible();
    expect(screen.getByText(/write may have happened, but its outcome is unknown/i)).toBeVisible();
    expect(screen.getByRole('region', { name: 'Unresolved action outcomes' }).querySelectorAll('li')).toHaveLength(2);
  });
});
