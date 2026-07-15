import type {
  ActionRequestMessage,
  ActionResultMessage,
  AgentRecord,
  ExpectedState,
  HostRecord,
  InboundMessage,
  OperationType,
  OutputSnapshotMessage,
  OutputSubscribeMessage,
  PromptSnapshotMessage,
  StateChange,
  Target,
} from '../protocol/types';

export type ConnectionState = 'bootstrapping' | 'connecting' | 'connected' | 'reconnecting' | 'offline' | 'unauthorized';
export type ResyncReason = 'gap' | 'epoch_mismatch' | 'unknown_remove' | 'connector_epoch_changed' | 'operator_refresh';

export interface PendingAction {
  actionId: string;
  operationType: OperationType;
  target: Target;
  phase: 'sending' | 'received';
  expected: ExpectedState;
}

export type DisplayedResult = ActionResultMessage['body'] & { local: boolean; target?: Target };

export interface BrowserState {
  connection: ConnectionState;
  sessionId: string | null;
  stateEpoch: string | null;
  sequence: number | null;
  hosts: HostRecord[];
  stale: boolean;
  needsResync: ResyncReason | null;
  prompts: Record<string, PromptSnapshotMessage['body']>;
  subscriptions: Record<string, OutputSubscribeMessage['body']>;
  outputs: Record<string, OutputSnapshotMessage['body']>;
  pendingActions: Record<string, PendingAction>;
  actionResults: Record<string, DisplayedResult>;
  generationWatermarks: Record<string, number>;
  seenStateEpochs: Record<string, true>;
  snapshotRejected: boolean;
  lastErrorCode: string | null;
}

export type StateEvent =
  | { type: 'session.bootstrapping' }
  | { type: 'socket.connecting'; reconnect: boolean }
  | { type: 'socket.open' }
  | { type: 'socket.closed'; offline: boolean }
  | { type: 'message.received'; message: InboundMessage }
  | { type: 'subscription.started'; message: OutputSubscribeMessage }
  | { type: 'subscription.ended'; subscriptionId: string }
  | { type: 'action.sent'; message: ActionRequestMessage }
  | { type: 'resync.sent' }
  | { type: 'resync.requested'; reason: ResyncReason }
  | { type: 'session.unauthorized' };

export const initialBrowserState: BrowserState = {
  connection: 'bootstrapping',
  sessionId: null,
  stateEpoch: null,
  sequence: null,
  hosts: [],
  stale: true,
  needsResync: null,
  prompts: {},
  subscriptions: {},
  outputs: {},
  pendingActions: {},
  actionResults: {},
  generationWatermarks: {},
  seenStateEpochs: {},
  snapshotRejected: false,
  lastErrorCode: null,
};

export function targetKey(target: Target): string {
  return `${target.host_id}\u0000${target.instance_id}\u0000${target.terminal_id}`;
}

function generationKey(target: Target, connectorEpoch: string): string {
  return `${targetKey(target)}\u0000${connectorEpoch}`;
}

function copyHosts(hosts: HostRecord[]): HostRecord[] {
  return hosts.map((host) => ({
    ...host,
    instances: host.instances.map((instance) => ({
      ...instance,
      capabilities: [...instance.capabilities],
      agents: instance.agents.map((agent) => ({ ...agent })),
    })),
  }));
}

function invalidate(state: BrowserState, reason: ResyncReason): BrowserState {
  return {
    ...state,
    stale: true,
    needsResync: state.sessionId ? reason : null,
    prompts: {},
    subscriptions: {},
    outputs: {},
  };
}

function locateAgent(hosts: HostRecord[], target: Target): AgentRecord | undefined {
  return hosts
    .find((host) => host.host_id === target.host_id)
    ?.instances.find((instance) => instance.instance_id === target.instance_id)
    ?.agents.find((agent) => agent.terminal_id === target.terminal_id);
}

export function locateInstance(hosts: HostRecord[], target: Pick<Target, 'host_id' | 'instance_id'>) {
  return hosts
    .find((host) => host.host_id === target.host_id)
    ?.instances.find((instance) => instance.instance_id === target.instance_id);
}

function removeTransientForTarget(state: BrowserState, target: Target): void {
  delete state.prompts[targetKey(target)];
  for (const [subscriptionId, subscription] of Object.entries(state.subscriptions)) {
    if (targetKey(subscription.target) === targetKey(target)) {
      delete state.subscriptions[subscriptionId];
      delete state.outputs[subscriptionId];
    }
  }
}

function applyChange(state: BrowserState, change: StateChange): ResyncReason | null {
  switch (change.operation) {
    case 'host.upsert': {
      const host = state.hosts.find((candidate) => candidate.host_id === change.host_id);
      if (host) {
        host.display_name = change.host.display_name;
        host.status = change.host.status;
      } else {
        state.hosts.push({ host_id: change.host_id, ...change.host, instances: [] });
      }
      return null;
    }
    case 'host.remove': {
      const index = state.hosts.findIndex((host) => host.host_id === change.host_id);
      if (index < 0) return 'unknown_remove';
      state.hosts.splice(index, 1);
      state.prompts = {};
      state.subscriptions = {};
      state.outputs = {};
      return null;
    }
    case 'instance.upsert': {
      const host = state.hosts.find((candidate) => candidate.host_id === change.host_id);
      if (!host) return 'epoch_mismatch';
      const instance = host.instances.find((candidate) => candidate.instance_id === change.instance_id);
      if (!instance) {
        host.instances.push({ instance_id: change.instance_id, ...change.instance, agents: [] });
        return null;
      }
      if (instance.connector_epoch !== change.instance.connector_epoch) return 'epoch_mismatch';
      Object.assign(instance, change.instance);
      return null;
    }
    case 'instance.remove': {
      const host = state.hosts.find((candidate) => candidate.host_id === change.host_id);
      if (!host) return 'unknown_remove';
      const index = host.instances.findIndex((instance) => instance.instance_id === change.instance_id);
      if (index < 0) return 'unknown_remove';
      host.instances.splice(index, 1);
      state.prompts = {};
      state.subscriptions = {};
      state.outputs = {};
      return null;
    }
    case 'instance.epoch_changed': {
      const instance = locateInstance(state.hosts, change);
      if (!instance || instance.connector_epoch !== change.previous_connector_epoch) return 'epoch_mismatch';
      return 'connector_epoch_changed';
    }
    case 'agent.upsert': {
      const instance = locateInstance(state.hosts, change.target);
      if (!instance || instance.connector_epoch !== change.agent.connector_epoch) return 'epoch_mismatch';
      const index = instance.agents.findIndex((agent) => agent.terminal_id === change.target.terminal_id);
      const current = instance.agents[index];
      if (current && current.connector_epoch === change.agent.connector_epoch && change.agent.agent_generation < current.agent_generation) {
        return 'epoch_mismatch';
      }
      const watermarkKey = generationKey(change.target, change.agent.connector_epoch);
      const maximumGeneration = state.generationWatermarks[watermarkKey] ?? 0;
      if (change.agent.agent_generation < maximumGeneration) return 'epoch_mismatch';
      state.generationWatermarks[watermarkKey] = Math.max(maximumGeneration, change.agent.agent_generation);
      const bindingChanged = Boolean(current && (
        current.agent_generation !== change.agent.agent_generation ||
        current.herdr_input_revision !== change.agent.herdr_input_revision
      ));
      const record = { terminal_id: change.target.terminal_id, ...change.agent };
      if (index < 0) instance.agents.push(record);
      else instance.agents[index] = record;
      if (bindingChanged || change.agent.status !== 'blocked') removeTransientForTarget(state, change.target);
      return null;
    }
    case 'agent.remove': {
      const instance = locateInstance(state.hosts, change.target);
      if (!instance) return 'unknown_remove';
      const index = instance.agents.findIndex((agent) => agent.terminal_id === change.target.terminal_id);
      if (index < 0) return 'unknown_remove';
      instance.agents.splice(index, 1);
      removeTransientForTarget(state, change.target);
      return null;
    }
    default:
      return assertNever(change);
  }
}

function acceptPrompt(state: BrowserState, message: PromptSnapshotMessage): BrowserState {
  const body = message.body;
  const agent = locateAgent(state.hosts, body.target);
  if (
    body.session_id !== state.sessionId ||
    body.state_epoch !== state.stateEpoch ||
    body.state_sequence !== state.sequence ||
    !agent ||
    agent.status !== 'blocked' ||
    agent.connector_epoch !== body.connector_epoch ||
    agent.agent_generation !== body.agent_generation ||
    agent.herdr_input_revision !== body.herdr_input_revision
  ) {
    return invalidate(state, 'epoch_mismatch');
  }
  return { ...state, prompts: { ...state.prompts, [targetKey(body.target)]: body } };
}

function acceptOutput(state: BrowserState, message: OutputSnapshotMessage): BrowserState {
  const body = message.body;
  const subscription = state.subscriptions[body.subscription_id];
  const agent = locateAgent(state.hosts, body.target);
  if (
    body.session_id !== state.sessionId ||
    body.state_epoch !== state.stateEpoch ||
    !subscription ||
    targetKey(subscription.target) !== targetKey(body.target) ||
    !agent ||
    agent.connector_epoch !== body.connector_epoch ||
    agent.agent_generation !== body.agent_generation ||
    agent.herdr_input_revision !== body.herdr_input_revision
  ) {
    return state;
  }
  return { ...state, outputs: { ...state.outputs, [body.subscription_id]: body } };
}

function syntheticResult(pending: PendingAction): DisplayedResult {
  const read = pending.operationType === 'agent.read';
  return {
    session_id: '',
    action_id: pending.actionId,
    operation_type: pending.operationType,
    status: read ? 'failed' : 'unknown',
    code: read ? 'CONNECTION_LOST' : 'OUTCOME_UNKNOWN',
    result: null,
    local: true,
    target: pending.target,
  };
}

function settlePendingActions(state: BrowserState): Pick<BrowserState, 'pendingActions' | 'actionResults'> {
  const actionResults = { ...state.actionResults };
  for (const pending of Object.values(state.pendingActions)) actionResults[pending.actionId] = syntheticResult(pending);
  return { pendingActions: {}, actionResults };
}

function updateSnapshotWatermarks(state: BrowserState, hosts: HostRecord[]): Record<string, number> | null {
  const watermarks = { ...state.generationWatermarks };
  for (const host of hosts) {
    for (const instance of host.instances) {
      for (const agent of instance.agents) {
        const key = generationKey(
          { host_id: host.host_id, instance_id: instance.instance_id, terminal_id: agent.terminal_id },
          agent.connector_epoch,
        );
        const maximum = watermarks[key] ?? 0;
        if (agent.agent_generation < maximum) return null;
        watermarks[key] = Math.max(maximum, agent.agent_generation);
      }
    }
  }
  return watermarks;
}

function rejectSnapshot(state: BrowserState): BrowserState {
  return {
    ...state,
    stale: true,
    needsResync: null,
    prompts: {},
    subscriptions: {},
    outputs: {},
    snapshotRejected: true,
    lastErrorCode: 'STALE_STATE',
  };
}

function disconnect(state: BrowserState, offline: boolean): BrowserState {
  const settled = settlePendingActions(state);
  return {
    ...state,
    connection: offline ? 'offline' : 'reconnecting',
    sessionId: null,
    stateEpoch: null,
    sequence: null,
    stale: true,
    needsResync: null,
    prompts: {},
    subscriptions: {},
    outputs: {},
    ...settled,
    snapshotRejected: false,
  };
}

function readResultMatches(state: BrowserState, pending: PendingAction, message: ActionResultMessage): boolean {
  const result = message.body.result;
  if (message.body.operation_type !== 'agent.read' || message.body.status !== 'succeeded') return true;
  if (!result || !('state_epoch' in result)) return false;
  const agent = locateAgent(state.hosts, pending.target);
  return Boolean(
    agent &&
    result.state_epoch === pending.expected.state_epoch &&
    result.state_epoch === state.stateEpoch &&
    result.connector_epoch === pending.expected.connector_epoch &&
    result.connector_epoch === agent.connector_epoch &&
    result.agent_generation === pending.expected.agent_generation &&
    result.agent_generation === agent.agent_generation &&
    result.herdr_input_revision === pending.expected.herdr_input_revision &&
    result.herdr_input_revision === agent.herdr_input_revision
  );
}

function promptMatchesPendingAction(prompt: PromptSnapshotMessage['body'], pending: PendingAction): boolean {
  return Boolean(
    pending.operationType === 'prompt.respond' &&
    targetKey(prompt.target) === targetKey(pending.target) &&
    prompt.connector_epoch === pending.expected.connector_epoch &&
    prompt.agent_generation === pending.expected.agent_generation &&
    prompt.herdr_input_revision === pending.expected.herdr_input_revision &&
    prompt.fingerprint === pending.expected.prompt_fingerprint &&
    prompt.herdr_content_hash === pending.expected.herdr_content_hash
  );
}

function receiveMessage(state: BrowserState, message: InboundMessage): BrowserState {
  switch (message.type) {
    case 'session.snapshot': {
      const sameSocket = state.sessionId !== null;
      if (sameSocket && message.body.session_id !== state.sessionId) return rejectSnapshot(state);
      if (state.seenStateEpochs[message.body.state_epoch]) return rejectSnapshot(state);
      const generationWatermarks = updateSnapshotWatermarks(state, message.body.hosts);
      if (!generationWatermarks) return rejectSnapshot(state);
      const actions = sameSocket
        ? { pendingActions: state.pendingActions, actionResults: state.actionResults }
        : settlePendingActions(state);
      return {
        ...state,
        ...actions,
        connection: 'connected',
        sessionId: message.body.session_id,
        stateEpoch: message.body.state_epoch,
        sequence: 0,
        hosts: copyHosts(message.body.hosts),
        stale: false,
        needsResync: null,
        prompts: {},
        subscriptions: {},
        outputs: {},
        generationWatermarks,
        seenStateEpochs: { ...state.seenStateEpochs, [message.body.state_epoch]: true },
        snapshotRejected: false,
        lastErrorCode: null,
      };
    }
    case 'state.delta': {
      if (message.body.session_id !== state.sessionId || message.body.state_epoch !== state.stateEpoch) {
        return invalidate(state, 'epoch_mismatch');
      }
      if (state.sequence === null || message.body.sequence !== state.sequence + 1) return invalidate(state, 'gap');
      const next = {
        ...state,
        hosts: copyHosts(state.hosts),
        prompts: { ...state.prompts },
        subscriptions: { ...state.subscriptions },
        outputs: { ...state.outputs },
        generationWatermarks: { ...state.generationWatermarks },
      };
      for (const change of message.body.changes) {
        const reason = applyChange(next, change);
        if (reason === 'connector_epoch_changed') {
          return invalidate({ ...state, sequence: message.body.sequence }, reason);
        }
        if (reason) return invalidate(state, reason);
      }
      next.sequence = message.body.sequence;
      return next;
    }
    case 'prompt.snapshot':
      return acceptPrompt(state, message);
    case 'output.snapshot':
      return acceptOutput(state, message);
    case 'action.received': {
      if (message.body.session_id !== state.sessionId) return state;
      const pending = state.pendingActions[message.body.action_id];
      if (!pending) return state;
      return { ...state, pendingActions: { ...state.pendingActions, [pending.actionId]: { ...pending, phase: 'received' } } };
    }
    case 'action.result': {
      if (message.body.session_id !== state.sessionId) return state;
      const pending = state.pendingActions[message.body.action_id];
      if (!pending || state.actionResults[message.body.action_id]) return state;
      if (pending.operationType !== message.body.operation_type) return invalidate(state, 'operator_refresh');
      const pendingActions = { ...state.pendingActions };
      delete pendingActions[message.body.action_id];
      const validReadResult = readResultMatches(state, pending, message);
      const result: DisplayedResult = validReadResult
        ? { ...message.body, local: false, target: pending.target }
        : {
            ...message.body,
            status: 'rejected',
            code: 'STALE_STATE',
            result: null,
            local: true,
            target: pending.target,
          };
      const next = {
        ...state,
        pendingActions,
        actionResults: { ...state.actionResults, [message.body.action_id]: result },
      };
      if (message.body.status === 'succeeded' && message.body.operation_type === 'prompt.respond') {
        const prompts = { ...next.prompts };
        const promptKey = targetKey(pending.target);
        const currentPrompt = prompts[promptKey];
        if (currentPrompt && promptMatchesPendingAction(currentPrompt, pending)) delete prompts[promptKey];
        next.prompts = prompts;
      }
      if (!validReadResult) return invalidate(next, 'operator_refresh');
      if (message.body.code === 'STALE_STATE' || message.body.code === 'STALE_TARGET' || message.body.code === 'PROMPT_CHANGED') {
        return invalidate(next, 'operator_refresh');
      }
      return next;
    }
    case 'protocol.error':
      if (message.body.code === 'UNAUTHORIZED') {
        return { ...disconnect(state, false), connection: 'unauthorized', lastErrorCode: message.body.code };
      }
      return { ...state, lastErrorCode: message.body.code };
    default:
      return assertNever(message);
  }
}

function assertNever(value: never): never {
  throw new Error(`Unhandled state variant: ${String(value)}`);
}

export function browserReducer(state: BrowserState, event: StateEvent): BrowserState {
  switch (event.type) {
    case 'session.bootstrapping':
      return { ...state, connection: 'bootstrapping' };
    case 'socket.connecting':
      return {
        ...disconnect(state, false),
        connection: event.reconnect ? 'reconnecting' : 'connecting',
      };
    case 'socket.open':
      return { ...state, connection: 'connecting', lastErrorCode: null };
    case 'socket.closed':
      return disconnect(state, event.offline);
    case 'message.received':
      return receiveMessage(state, event.message);
    case 'subscription.started':
      return {
        ...state,
        subscriptions: { ...state.subscriptions, [event.message.body.subscription_id]: event.message.body },
      };
    case 'subscription.ended': {
      const subscriptions = { ...state.subscriptions };
      const outputs = { ...state.outputs };
      delete subscriptions[event.subscriptionId];
      delete outputs[event.subscriptionId];
      return { ...state, subscriptions, outputs };
    }
    case 'action.sent': {
      const body = event.message.body;
      return {
        ...state,
        pendingActions: {
          ...state.pendingActions,
          [body.action_id]: {
            actionId: body.action_id,
            operationType: body.operation.type,
            target: body.target,
            phase: 'sending',
            expected: body.expected,
          },
        },
      };
    }
    case 'resync.sent':
      return { ...state, needsResync: null };
    case 'resync.requested':
      return invalidate(state, event.reason);
    case 'session.unauthorized':
      return { ...disconnect(state, false), connection: 'unauthorized' };
    default:
      return assertNever(event);
  }
}

export interface AgentView {
  target: Target;
  hostName: string;
  instance: ReturnType<typeof locateInstance> & {};
  agent: AgentRecord;
}

export function listAgents(state: BrowserState): AgentView[] {
  const agents: AgentView[] = [];
  for (const host of state.hosts) {
    for (const instance of host.instances) {
      for (const agent of instance.agents) {
        agents.push({
          target: { host_id: host.host_id, instance_id: instance.instance_id, terminal_id: agent.terminal_id },
          hostName: host.display_name,
          instance,
          agent,
        });
      }
    }
  }
  return agents;
}
