import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react';
import { subscribePush, reconcilePush, testPush, unsubscribePush } from '../api/push';
import { bootstrapSession, logout, type SessionBootstrap } from '../api/session';
import { ApiError } from '../api/http';
import { useControlPlane, type ControlPlane } from '../connection/useControlPlane';
import type { AgentStatus, KeyName, Operation, Target } from '../protocol/types';
import { listAgents, targetKey, type AgentView, type BrowserState } from '../state/reducer';

const STATUS_ORDER: Array<{ title: string; statuses: AgentStatus[] }> = [
  { title: 'Needs input', statuses: ['blocked'] },
  { title: 'Working', statuses: ['working'] },
  { title: 'Idle and finished', statuses: ['idle', 'done', 'unknown'] },
];

const RESULT_TEXT: Record<string, string> = {
  DUPLICATE_ACTION: 'This action ID was already used. Nothing was sent again.',
  STALE_STATE: 'Agent state changed before the action could run. State is being refreshed.',
  STALE_TARGET: 'The selected agent changed. State is being refreshed.',
  PROMPT_CHANGED: 'The prompt changed before the response arrived. State is being refreshed.',
  OUTCOME_UNKNOWN: 'The write may have happened, but its outcome is unknown. Check fresh output before retrying.',
  CONNECTION_LOST: 'The read did not finish because the connection was lost.',
  HERDR_INCOMPATIBLE: 'This Herdr version does not support safe checked input.',
  UNAUTHORIZED: 'Your session is no longer authorized.',
};

export function App() {
  const [session, setSession] = useState<SessionBootstrap | null>(null);
  const [sessionError, setSessionError] = useState(false);
  const [selectedKey, setSelectedKey] = useState<string | null>(null);
  const loadSession = useCallback(async () => {
    setSessionError(false);
    try {
      setSession(await bootstrapSession());
    } catch (error) {
      if (error instanceof ApiError && (error.status === 401 || error.status === 403)) {
        setSession({ authenticated: false, operator: null, push_public_key: null });
        return;
      }
      setSessionError(true);
    }
  }, []);
  const handleUnauthorized = useCallback(() => {
    setSession({ authenticated: false, operator: null, push_public_key: null });
    void loadSession();
  }, [loadSession]);
  const control = useControlPlane(Boolean(session?.authenticated), handleUnauthorized);

  useEffect(() => void loadSession(), [loadSession]);
  useEffect(() => {
    const refresh = () => void loadSession();
    window.addEventListener('herdr:push-refresh', refresh);
    return () => window.removeEventListener('herdr:push-refresh', refresh);
  }, [loadSession]);

  const agents = useMemo(() => listAgents(control.state), [control.state]);
  const selected = agents.find((candidate) => targetKey(candidate.target) === selectedKey) ?? null;

  if (sessionError) {
    return <SessionProblem retry={() => void loadSession()} />;
  }
  if (!session) {
    return <main className="centered"><p role="status">Checking your session…</p></main>;
  }
  if (!session.authenticated) {
    return <SignedOut />;
  }

  const handleLogout = async () => {
    try {
      await logout();
      setSession({ authenticated: false, operator: null, push_public_key: null });
    } catch {
      setSessionError(true);
    }
  };

  return (
    <div className="app-shell">
      <a className="skip-link" href="#main-content">Skip to agents</a>
      <header className="app-header">
        <img src="/icon.svg" alt="" width="36" height="36" />
        <div className="brand"><strong>Herdr Remote</strong><span>Enterprise control</span></div>
        <span className="operator">{session.operator?.display_name}</span>
        <button className="quiet-button" type="button" onClick={() => void handleLogout()}>Log out</button>
      </header>
      <StatusBanners state={control.state} requestRefresh={() => control.requestResync()} />
      <main id="main-content" tabIndex={-1} className={`workspace ${selected ? 'has-selection' : ''}`}>
        <AgentList agents={agents} state={control.state} selectedKey={selectedKey} onSelect={(target) => setSelectedKey(targetKey(target))} />
        <section id="agent-detail" className="detail-pane" aria-label="Agent detail">
          {selected ? (
            <AgentDetail key={targetKey(selected.target)} view={selected} control={control} onBack={() => setSelectedKey(null)} />
          ) : (
            <div className="empty-detail"><h2>Select an agent</h2><p>Live output and safe controls appear here.</p></div>
          )}
        </section>
      </main>
      <aside className="settings-strip" aria-label="Notifications">
        <PushSettings publicKey={session.push_public_key} />
      </aside>
    </div>
  );
}

function SessionProblem({ retry }: { retry: () => void }) {
  return (
    <main className="centered">
      <div className="panel"><h1>Herdr Remote</h1><p role="alert">The authenticated session could not be checked.</p><button type="button" onClick={retry}>Try again</button></div>
    </main>
  );
}

function SignedOut() {
  return (
    <main className="centered">
      <div className="panel sign-in-panel">
        <img src="/icon.svg" width="72" height="72" alt="" />
        <h1>Herdr Remote</h1>
        <p>Sign in through your organization to view authorized agents.</p>
        <a className="primary-link" href="/auth/login?return_to=%2Fagents">Sign in</a>
      </div>
    </main>
  );
}

export function StatusBanners({ state, requestRefresh }: { state: BrowserState; requestRefresh: () => void }) {
  const pending = Object.values(state.pendingActions);
  const unresolvedResults = Object.values(state.actionResults).filter((result) => result.status !== 'succeeded');
  return (
    <div className="banner-stack" aria-live="polite" aria-atomic="true">
      {state.connection !== 'connected' && (
        <div className={`banner ${state.connection === 'offline' ? 'warning' : ''}`} role="status">
          {state.connection === 'offline' ? 'Offline. Controls are unavailable.' : state.connection === 'unauthorized' ? 'Session authorization expired.' : 'Connecting to the control plane…'}
        </div>
      )}
      {state.stale && state.hosts.length > 0 && (
        <div className="banner warning" role="alert">Displayed state may be stale. Actions are unavailable. <button type="button" onClick={requestRefresh}>Refresh</button></div>
      )}
      {pending.length > 0 && <div className="banner" role="status">{pending.length} action{pending.length === 1 ? '' : 's'} awaiting a final result.</div>}
      {unresolvedResults.length > 0 && (
        <section className="action-outcomes" aria-label="Unresolved action outcomes">
          <h2>Action outcomes</h2>
          <ul>
            {unresolvedResults.map((result) => (
              <li className={result.status === 'unknown' ? 'danger' : 'warning'} key={result.action_id}>
                <strong>{result.operation_type}</strong>{' — '}
                {result.code ? RESULT_TEXT[result.code] ?? `Action did not complete (${result.code}).` : 'Action did not complete.'}
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

function AgentList({ agents, state, selectedKey, onSelect }: { agents: AgentView[]; state: BrowserState; selectedKey: string | null; onSelect: (target: Target) => void }) {
  return (
    <nav className="agent-pane" aria-labelledby="agents-title">
      <div className="pane-heading"><div><p className="eyebrow">Current state</p><h1 id="agents-title">Agents</h1></div><span>{agents.length}</span></div>
      {agents.length === 0 ? <p className="empty-list" role="status">{state.connection === 'connected' ? 'No authorized agents.' : 'Waiting for current state…'}</p> : null}
      {STATUS_ORDER.map((group) => {
        const members = agents.filter(({ agent }) => group.statuses.includes(agent.status));
        if (members.length === 0) return null;
        return (
          <section className="agent-group" key={group.title} aria-labelledby={`group-${group.statuses[0]}`}>
            <h2 id={`group-${group.statuses[0]}`}><span className={`status-dot ${group.statuses[0]}`} />{group.title}<span>{members.length}</span></h2>
            <ul>
              {members.map((view) => {
                const key = targetKey(view.target);
                return (
                  <li key={key}>
                    <button type="button" className="agent-card" aria-current={selectedKey === key ? 'true' : undefined} onClick={() => onSelect(view.target)}>
                      <span className="agent-mark" aria-hidden="true">{(view.agent.display_name ?? view.agent.agent).slice(0, 1).toUpperCase()}</span>
                      <span className="agent-copy"><strong>{view.agent.display_name ?? view.agent.agent}</strong><span>{view.agent.project ?? 'No project label'} · {view.hostName}</span></span>
                      <span className={`status-label ${view.agent.status}`}>{view.agent.status}</span>
                    </button>
                  </li>
                );
              })}
            </ul>
          </section>
        );
      })}
    </nav>
  );
}

function AgentDetail({ view, control, onBack }: { view: AgentView; control: ControlPlane; onBack: () => void }) {
  const [subscriptionId, setSubscriptionId] = useState<string | null>(null);
  const [text, setText] = useState('');
  const [sendEnter, setSendEnter] = useState(false);
  const prompt = control.state.prompts[targetKey(view.target)];
  const output = subscriptionId ? control.state.outputs[subscriptionId] : undefined;
  const latestRead = Object.values(control.state.actionResults)
    .filter((result) => result.operation_type === 'agent.read' && result.status === 'succeeded' && result.target && targetKey(result.target) === targetKey(view.target))
    .at(-1);
  const readText = latestRead?.result && 'text' in latestRead.result ? latestRead.result.text : undefined;
  const canWrite = !control.state.stale && view.instance.capabilities.includes('checked_input.v1') && view.agent.herdr_input_revision > 0;
  const canRespondToPrompt = canWrite && view.instance.capabilities.includes('prompt.respond.v1');
  const busy = Object.values(control.state.pendingActions).some((pending) => targetKey(pending.target) === targetKey(view.target));

  useEffect(() => {
    const id = control.subscribe(view.target);
    setSubscriptionId(id);
    return () => {
      if (id) control.unsubscribe(id);
    };
  }, [
    control.state.sessionId,
    control.state.stateEpoch,
    control.state.stale,
    view.agent.agent_generation,
    view.agent.herdr_input_revision,
    control.subscribe,
    control.unsubscribe,
    view.target.host_id,
    view.target.instance_id,
    view.target.terminal_id,
  ]);

  const sendText = (event: FormEvent) => {
    event.preventDefault();
    if (!text || !canWrite || busy) return;
    const operation: Operation = sendEnter
      ? { type: 'agent.send_input', text, keys: ['enter'] }
      : { type: 'agent.send_text', text };
    if (control.act(view.target, operation)) setText('');
  };

  const key = (name: KeyName) => control.sendKey(view.target, name);

  return (
    <article className="agent-detail">
      <header className="detail-header">
        <button className="back-button" type="button" onClick={onBack} aria-label="Back to agents">‹</button>
        <div><p className="eyebrow">{view.hostName} · {view.target.instance_id}</p><h2>{view.agent.display_name ?? view.agent.agent}</h2><p>{view.agent.project ?? 'No project label'}</p></div>
        <span className={`status-pill ${view.agent.status}`}>{view.agent.status}</span>
      </header>
      {prompt && (
        <section className="prompt-panel" aria-labelledby="prompt-title">
          <h3 id="prompt-title">Agent needs input</h3>
          <p className="prompt-excerpt">{prompt.excerpt}{prompt.excerpt_truncated ? '…' : ''}</p>
          {prompt.options.length > 0 ? (
            <div className="prompt-options">
              {prompt.options.map((option) => <button type="button" key={option.id} disabled={!canRespondToPrompt || busy} onClick={() => control.act(view.target, { type: 'prompt.respond', option_id: option.id })}>{option.label}</button>)}
            </div>
          ) : <p>No structured response options are available.</p>}
        </section>
      )}
      <section className="output-panel" aria-labelledby="output-title">
        <div className="section-heading"><h3 id="output-title">Recent output</h3><button type="button" disabled={!view.instance.capabilities.includes('read.v1') || control.state.stale || busy} onClick={() => control.act(view.target, { type: 'agent.read', source: 'recent', lines: 200 })}>Read now</button></div>
        <pre tabIndex={0} aria-label="Bounded terminal output">{output?.text ?? readText ?? (view.instance.capabilities.includes('output.subscribe.v1') ? 'Waiting for output…' : 'Live output is unavailable for this instance.')}</pre>
        {output?.truncated && <p className="muted">Earlier output was truncated by the bounded reader.</p>}
      </section>
      <section className="controls" aria-labelledby="controls-title">
        <h3 id="controls-title">Checked input</h3>
        {!canWrite && (
          <p className="readonly-message" role="note"><strong>Read-only.</strong> This agent cannot accept checked atomic input. No unsafe fallback is available.</p>
        )}
        <form onSubmit={sendText}>
          <label htmlFor="agent-input">Text to send</label>
          <textarea id="agent-input" value={text} maxLength={4096} rows={2} disabled={!canWrite || busy} onChange={(event) => setText(event.target.value.replace(/[\u0000-\u001f\u007f-\u009f]/g, ''))} />
          <label className="checkbox"><input type="checkbox" checked={sendEnter} disabled={!canWrite || busy} onChange={(event) => setSendEnter(event.target.checked)} /> Send Enter in the same checked action</label>
          <button className="primary-button" type="submit" disabled={!canWrite || busy || text.length === 0}>Send text</button>
        </form>
        <div className="key-grid" aria-label="Special keys">
          <button type="button" disabled={!canWrite || busy} onClick={() => key('tab')}>Tab</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('shift+tab')}>Shift+Tab</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('esc')}>Esc</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('ctrl+c')}>Ctrl+C</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('left')} aria-label="Left arrow">←</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('up')} aria-label="Up arrow">↑</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('down')} aria-label="Down arrow">↓</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('right')} aria-label="Right arrow">→</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('enter')}>Enter</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('pageup')}>Page Up</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('pagedown')}>Page Down</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('home')}>Home</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('end')}>End</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('backspace')}>Backspace</button>
          <button type="button" disabled={!canWrite || busy} onClick={() => key('delete')}>Delete</button>
        </div>
        <button className="interrupt-button" type="button" disabled={!canWrite || busy} onClick={() => control.act(view.target, { type: 'agent.interrupt' })}>Interrupt agent</button>
      </section>
    </article>
  );
}

function PushSettings({ publicKey }: { publicKey: string | null }) {
  const [supported, setSupported] = useState(false);
  const [subscribed, setSubscribed] = useState(false);
  const [message, setMessage] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const ios = /iphone|ipad|ipod/i.test(navigator.userAgent);
  const standalone = Boolean(window.matchMedia?.('(display-mode: standalone)')?.matches) || Boolean((navigator as Navigator & { standalone?: boolean }).standalone);

  useEffect(() => {
    const available = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window && Boolean(publicKey);
    setSupported(available);
    if (!available) return;
    void navigator.serviceWorker.ready
      .then((registration) => reconcilePush(registration))
      .then((value) => setSubscribed(value.subscribed))
      .catch(() => setMessage('Push status could not be reconciled.'));
  }, [publicKey]);

  const run = async (task: (registration: ServiceWorkerRegistration) => Promise<void>, success: string) => {
    setBusy(true);
    setMessage(null);
    try {
      const registration = await navigator.serviceWorker.ready;
      await task(registration);
      setMessage(success);
    } catch {
      setMessage('The notification request did not complete.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <details>
      <summary>Notifications</summary>
      {ios && !standalone && <p className="ios-guidance">On iPhone or iPad, use Share → Add to Home Screen before enabling push notifications.</p>}
      {!supported ? <p>Push notifications are not available in this browser or deployment.</p> : subscribed ? (
        <div className="notification-actions">
          <button type="button" disabled={busy} onClick={() => void run(async () => testPush(), 'A generic test notification was requested.')}>Send test</button>
          <button type="button" disabled={busy} onClick={() => void run(async (registration) => { await unsubscribePush(registration); setSubscribed(false); }, 'Notifications are off.')}>Turn off</button>
        </div>
      ) : (
        <button type="button" disabled={busy || !publicKey} onClick={() => void run(async (registration) => { await subscribePush(registration, publicKey!); setSubscribed(true); }, 'Notifications are on.')}>Enable notifications</button>
      )}
      {message && <p role="status">{message}</p>}
      <p className="muted">Notifications contain only a generic update hint. Open the app to load current state.</p>
    </details>
  );
}
