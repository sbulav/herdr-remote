import { act, render, screen, waitFor } from '@testing-library/react';
import axe from 'axe-core';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { App, PushSettings, StatusBanners } from '../src/ui/App';
import { initialBrowserState } from '../src/state/reducer';
import * as pushApi from '../src/api/push';
import { pendingReplacementStore } from '../src/push/pendingReplacement';

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
  close(code = 1000, reason = '') { this.readyState = 3; this.dispatchEvent(new CloseEvent('close', { code, reason })); }
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
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('unauthorized', { status: 401 })));
    render(<App />);
    const link = await screen.findByRole('link', { name: 'Sign in' });
    expect(link).toHaveAttribute('href', '/auth/login?return_to=%2Fagents');
    expect(document.querySelector('input[type="password"]')).not.toBeInTheDocument();
  });

  it('renders read-only agents without critical axe violations', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' }), { status: 200 })));
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
      .mockResolvedValueOnce(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' }), { status: 200 }))
      .mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
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

  it('re-bootstraps and reconnects after a silent websocket session expiry', async () => {
    const authenticated = { authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify(authenticated), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify(authenticated), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    render(<App />);
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
    await act(async () => FakeWebSocket.instances[0]!.close(4401, 'session unauthorized'));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(2));
  });

  it('revalidates HTTP and reconnects after an abnormal server-restart close', async () => {
    vi.spyOn(Math, 'random').mockReturnValue(0);
    const authenticated = { authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify(authenticated), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify(authenticated), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    render(<App />);
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
    await act(async () => FakeWebSocket.instances[0]!.close(1006, 'server restart'));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    await act(async () => { await new Promise((resolve) => window.setTimeout(resolve, 1_050)); });
    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it('stops reconnecting when server-restart HTTP revalidation is unauthorized', async () => {
    const authenticated = { authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' };
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify(authenticated), { status: 200 }))
      .mockResolvedValueOnce(new Response('unauthorized', { status: 401 }));
    vi.stubGlobal('fetch', fetchMock);
    render(<App />);
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
    await act(async () => FakeWebSocket.instances[0]!.close(1006, 'failed upgrade'));
    expect(await screen.findByRole('link', { name: 'Sign in' })).toBeVisible();
    await act(async () => { await new Promise((resolve) => window.setTimeout(resolve, 1_050)); });
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('uses the configured upstream URL after local logout', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ token: 'csrf' }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ logout_url: 'https://id.example/logout' }), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    render(<App />);
    const logoutButton = await screen.findByRole('button', { name: 'Log out' });
    await act(async () => logoutButton.click());
    expect(await screen.findByRole('link', { name: 'Sign in' })).toBeVisible();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('requires prompt response capability separately from checked input', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ authenticated: true, operator: { display_name: 'Operator' }, push_public_key: null, logout_url: 'https://id.example/logout' }), { status: 200 })));
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

  it('does not let an older public-key reconciliation overwrite newer UI state', async () => {
    let resolveStale!: (value: { subscribed: boolean }) => void;
    const stale = new Promise<{ subscribed: boolean }>((resolve) => { resolveStale = resolve; });
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(null) } } as unknown as ServiceWorkerRegistration;
    Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: { ready: Promise.resolve(registration) } });
    vi.stubGlobal('PushManager', class {});
    vi.stubGlobal('Notification', class {});
    const reconcile = vi.spyOn(pushApi, 'reconcilePush')
      .mockImplementationOnce(async () => stale)
      .mockResolvedValueOnce({ subscribed: false });

    const view = render(<PushSettings publicKey="AQ" />);
    await waitFor(() => expect(reconcile).toHaveBeenCalledWith(registration, 'AQ'));
    view.rerender(<PushSettings publicKey="Ag" />);
    await waitFor(() => expect(reconcile).toHaveBeenCalledWith(registration, 'Ag'));
    expect(await screen.findByRole('button', { name: 'Enable notifications' })).toBeInTheDocument();

    resolveStale({ subscribed: true });
    await act(async () => { await stale; });
    expect(screen.getByRole('button', { name: 'Enable notifications' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send test' })).not.toBeInTheDocument();
  });

  it('offers only opt-out when push exists without Web Locks and a subscription exists', async () => {
    const locks = Object.getOwnPropertyDescriptor(navigator, 'locks');
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined });
    const local = {} as PushSubscription;
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;
    Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: { ready: Promise.resolve(registration) } });
    vi.stubGlobal('PushManager', class {});
    vi.stubGlobal('Notification', class {});
    const unsubscribe = vi.spyOn(pushApi, 'unsubscribePush').mockResolvedValue();

    try {
      render(<PushSettings publicKey="AQ" />);
      await act(async () => screen.getByText('Notifications').click());
      const button = await screen.findByRole('button', { name: 'Turn off existing notifications' });
      expect(screen.queryByRole('button', { name: 'Enable notifications' })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'Send test' })).not.toBeInTheDocument();
      await act(async () => button.click());
      expect(unsubscribe).toHaveBeenCalledWith(registration);
    } finally {
      if (locks) Object.defineProperty(navigator, 'locks', locks);
    }
  });

  it('shows compatibility text only when push exists without Web Locks and no subscription exists', async () => {
    const locks = Object.getOwnPropertyDescriptor(navigator, 'locks');
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined });
    const getSubscription = vi.fn().mockResolvedValue(null);
    const registration = { pushManager: { getSubscription } } as unknown as ServiceWorkerRegistration;
    Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: { ready: Promise.resolve(registration) } });
    vi.stubGlobal('PushManager', class {});
    vi.stubGlobal('Notification', class {});
    vi.spyOn(pendingReplacementStore, 'load').mockResolvedValue({ source_endpoints: [], opted_out: false });

    try {
      render(<PushSettings publicKey="AQ" />);
      await act(async () => screen.getByText('Notifications').click());
      expect(await screen.findByText('Notifications require Web Locks support in this browser.')).toBeVisible();
      await waitFor(() => expect(getSubscription).toHaveBeenCalledOnce());
      expect(screen.queryAllByRole('button')).toHaveLength(0);
    } finally {
      if (locks) Object.defineProperty(navigator, 'locks', locks);
    }
  });

  it('offers opt-out for an existing subscription when the VAPID public key is absent', async () => {
    const local = {} as PushSubscription;
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(local) } } as unknown as ServiceWorkerRegistration;
    Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: { ready: Promise.resolve(registration) } });
    vi.stubGlobal('PushManager', class {});
    vi.stubGlobal('Notification', class {});
    vi.spyOn(pendingReplacementStore, 'load').mockResolvedValue({ source_endpoints: [], opted_out: false });

    render(<PushSettings publicKey={null} />);
    await act(async () => screen.getByText('Notifications').click());

    expect(await screen.findByRole('button', { name: 'Turn off existing notifications' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Enable notifications' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Send test' })).not.toBeInTheDocument();
  });

  it('offers opt-out for pending cleanup without a local subscription', async () => {
    const locks = Object.getOwnPropertyDescriptor(navigator, 'locks');
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined });
    const registration = { pushManager: { getSubscription: vi.fn().mockResolvedValue(null) } } as unknown as ServiceWorkerRegistration;
    Object.defineProperty(navigator, 'serviceWorker', { configurable: true, value: { ready: Promise.resolve(registration) } });
    vi.stubGlobal('PushManager', class {});
    vi.stubGlobal('Notification', class {});
    vi.spyOn(pendingReplacementStore, 'load').mockResolvedValue({ source_endpoints: ['https://push.example/pending'], opted_out: true });
    const unsubscribe = vi.spyOn(pushApi, 'unsubscribePush').mockResolvedValue();

    try {
      render(<PushSettings publicKey={null} />);
      await act(async () => screen.getByText('Notifications').click());
      const button = await screen.findByRole('button', { name: 'Turn off existing notifications' });
      await act(async () => button.click());

      expect(unsubscribe).toHaveBeenCalledWith(registration);
      expect(screen.queryByRole('button', { name: 'Enable notifications' })).not.toBeInTheDocument();
    } finally {
      if (locks) Object.defineProperty(navigator, 'locks', locks);
    }
  });
});
