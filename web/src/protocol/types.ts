export type UUID = string;
export type AgentStatus = 'idle' | 'working' | 'blocked' | 'done' | 'unknown';
export type InstanceStatus = 'online' | 'degraded' | 'incompatible' | 'offline';
export type Capability =
  | 'read.v1'
  | 'output.subscribe.v1'
  | 'prompt.snapshot.v1'
  | 'checked_input.v1'
  | 'prompt.respond.v1';

export interface Target {
  host_id: UUID;
  instance_id: string;
  terminal_id: string;
}

export interface AgentState {
  agent: string;
  display_name: string | null;
  status: AgentStatus;
  project: string | null;
  connector_epoch: UUID;
  agent_generation: number;
  herdr_input_revision: number;
}

export interface AgentRecord extends AgentState {
  terminal_id: string;
}

export interface InstanceState {
  connector_epoch: UUID;
  herdr_version: string;
  herdr_protocol: number;
  status: InstanceStatus;
  capabilities: Capability[];
}

export interface InstanceRecord extends InstanceState {
  instance_id: string;
  agents: AgentRecord[];
}

export interface HostRecord {
  host_id: UUID;
  display_name: string;
  status: 'connected' | 'disconnected';
  instances: InstanceRecord[];
}

export type StateChange =
  | { operation: 'host.upsert'; host_id: UUID; host: Omit<HostRecord, 'host_id' | 'instances'> }
  | { operation: 'host.remove'; host_id: UUID; reason: 'unenrolled' | 'authorization_changed' }
  | { operation: 'instance.upsert'; host_id: UUID; instance_id: string; instance: InstanceState }
  | { operation: 'instance.remove'; host_id: UUID; instance_id: string; reason: 'unconfigured' | 'host_unenrolled' }
  | {
      operation: 'instance.epoch_changed';
      host_id: UUID;
      instance_id: string;
      previous_connector_epoch: UUID;
      connector_epoch: UUID;
    }
  | { operation: 'agent.upsert'; target: Target; agent: AgentState }
  | { operation: 'agent.remove'; target: Target; reason: 'pane_closed' | 'agent_exited' | 'reconciled' };

export type KeyName =
  | 'enter'
  | 'esc'
  | 'tab'
  | 'shift+tab'
  | 'up'
  | 'down'
  | 'left'
  | 'right'
  | 'pageup'
  | 'pagedown'
  | 'home'
  | 'end'
  | 'backspace'
  | 'delete'
  | 'ctrl+c';

export type Operation =
  | { type: 'agent.read'; source: OutputSource; lines: number }
  | { type: 'agent.send_text'; text: string }
  | { type: 'agent.send_keys'; keys: KeyName[] }
  | { type: 'agent.send_input'; text?: string; keys?: KeyName[] }
  | { type: 'agent.interrupt' }
  | { type: 'prompt.respond'; option_id: string };

export type OperationType = Operation['type'];
export type OutputSource = 'visible' | 'recent' | 'recent_unwrapped' | 'detection';

export interface ExpectedState {
  state_epoch: UUID;
  connector_epoch: UUID;
  agent_generation: number;
  herdr_input_revision: number;
  agent: string;
  statuses: AgentStatus[];
  prompt_fingerprint?: string;
  herdr_content_hash?: string;
}

export interface Envelope<T extends string, B> {
  protocol: 1;
  message_id: UUID;
  type: T;
  sent_at: string;
  body: B;
}

export type SessionSnapshotMessage = Envelope<
  'session.snapshot',
  { session_id: UUID; state_epoch: UUID; sequence: 0; server_time: string; hosts: HostRecord[] }
>;
export type StateDeltaMessage = Envelope<
  'state.delta',
  { session_id: UUID; state_epoch: UUID; sequence: number; changes: StateChange[] }
>;
export type StateResyncMessage = Envelope<
  'state.resync',
  {
    session_id: UUID;
    expected_epoch: UUID | null;
    expected_sequence: number | null;
    reason: 'gap' | 'epoch_mismatch' | 'unknown_remove' | 'connector_epoch_changed' | 'operator_refresh';
  }
>;

export interface PromptOption {
  id: string;
  label: string;
}

export type PromptSnapshotMessage = Envelope<
  'prompt.snapshot',
  {
    session_id: UUID;
    target: Target;
    state_epoch: UUID;
    state_sequence: number;
    connector_epoch: UUID;
    agent_generation: number;
    herdr_input_revision: number;
    herdr_content_hash: string;
    fingerprint: string;
    excerpt: string;
    excerpt_truncated: boolean;
    adapter_version: string;
    options: PromptOption[];
  }
>;
export type OutputSubscribeMessage = Envelope<
  'output.subscribe',
  {
    session_id: UUID;
    subscription_id: UUID;
    target: Target;
    source: OutputSource;
    lines: number;
    poll_interval_ms: number;
  }
>;
export type OutputUnsubscribeMessage = Envelope<
  'output.unsubscribe',
  { session_id: UUID; subscription_id: UUID }
>;
export type OutputSnapshotMessage = Envelope<
  'output.snapshot',
  {
    session_id: UUID;
    subscription_id: UUID;
    target: Target;
    state_epoch: UUID;
    connector_epoch: UUID;
    agent_generation: number;
    herdr_input_revision: number;
    content_revision: string;
    text: string;
    truncated: boolean;
  }
>;
export type ActionRequestMessage = Envelope<
  'action.request',
  {
    session_id: UUID;
    action_id: UUID;
    target: Target;
    timeout_ms: number;
    expected: ExpectedState;
    operation: Operation;
  }
>;
export type ActionReceivedMessage = Envelope<
  'action.received',
  { session_id: UUID; action_id: UUID }
>;

export type ResultCode =
  | 'INVALID_MESSAGE'
  | 'UNSUPPORTED_OPERATION'
  | 'UNAUTHORIZED'
  | 'DUPLICATE_ACTION'
  | 'TARGET_NOT_FOUND'
  | 'STALE_TARGET'
  | 'STALE_STATE'
  | 'NOT_AN_AGENT'
  | 'PROMPT_CHANGED'
  | 'INVALID_TEXT'
  | 'INVALID_KEYS'
  | 'DEADLINE_EXCEEDED'
  | 'CONNECTION_LOST'
  | 'HERDR_UNAVAILABLE'
  | 'HERDR_INCOMPATIBLE'
  | 'HERDR_REJECTED'
  | 'OUTCOME_UNKNOWN'
  | 'AUDIT_UNAVAILABLE'
  | 'BUSY'
  | 'RATE_LIMITED'
  | 'INTERNAL';

export interface ReadResult {
  state_epoch: UUID;
  connector_epoch: UUID;
  agent_generation: number;
  herdr_input_revision: number;
  text: string;
  truncated: boolean;
  content_revision: string;
}

export type ActionResultMessage = Envelope<
  'action.result',
  {
    session_id: UUID;
    action_id: UUID;
    operation_type: OperationType;
    status: 'succeeded' | 'rejected' | 'failed' | 'unknown';
    code: ResultCode | null;
    result: ReadResult | { herdr_acknowledged: true; option_id?: string } | null;
  }
>;
export type ProtocolErrorMessage = Envelope<
  'protocol.error',
  {
    session_id: UUID;
    in_reply_to: UUID | null;
    code: 'INVALID_MESSAGE' | 'UNSUPPORTED_PROTOCOL' | 'UNSUPPORTED_MESSAGE' | 'UNAUTHORIZED' | 'RATE_LIMITED' | 'INTERNAL';
    fatal: boolean;
  }
>;

export type BrowserMessage =
  | SessionSnapshotMessage
  | StateDeltaMessage
  | StateResyncMessage
  | PromptSnapshotMessage
  | OutputSubscribeMessage
  | OutputUnsubscribeMessage
  | OutputSnapshotMessage
  | ActionRequestMessage
  | ActionReceivedMessage
  | ActionResultMessage
  | ProtocolErrorMessage;

export type InboundMessage =
  | SessionSnapshotMessage
  | StateDeltaMessage
  | PromptSnapshotMessage
  | OutputSnapshotMessage
  | ActionReceivedMessage
  | ActionResultMessage
  | ProtocolErrorMessage;

export type OutboundMessage = StateResyncMessage | OutputSubscribeMessage | OutputUnsubscribeMessage | ActionRequestMessage;
