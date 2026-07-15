import type { ErrorObject } from 'ajv';
import generatedValidate from './generated-validator';
import type {
  BrowserMessage,
  InboundMessage,
  OutboundMessage,
  PromptSnapshotMessage,
  SessionSnapshotMessage,
} from './types';

const validateSchema = generatedValidate as ((value: unknown) => boolean) & { errors?: ErrorObject[] | null };
const inboundTypes = new Set<InboundMessage['type']>([
  'session.snapshot',
  'state.delta',
  'prompt.snapshot',
  'output.snapshot',
  'action.received',
  'action.result',
  'protocol.error',
]);
const outboundTypes = new Set<OutboundMessage['type']>([
  'state.resync',
  'output.subscribe',
  'output.unsubscribe',
  'action.request',
]);

export class ProtocolValidationError extends Error {
  readonly errors: readonly ErrorObject[];

  constructor(message: string, errors: readonly ErrorObject[] = []) {
    super(message);
    this.name = 'ProtocolValidationError';
    this.errors = errors;
  }
}

function assertUnique(values: readonly string[], description: string): void {
  if (new Set(values).size !== values.length) {
    throw new ProtocolValidationError(`Duplicate ${description}`);
  }
}

function assertSnapshotSemantics(message: SessionSnapshotMessage): void {
  assertUnique(message.body.hosts.map((host) => host.host_id), 'host identifier');
  for (const host of message.body.hosts) {
    assertUnique(host.instances.map((instance) => instance.instance_id), 'instance identifier');
    for (const instance of host.instances) {
      assertUnique(instance.agents.map((agent) => agent.terminal_id), 'terminal identifier');
      if (instance.capabilities.includes('prompt.respond.v1') && !instance.capabilities.includes('checked_input.v1')) {
        throw new ProtocolValidationError('Prompt response requires checked input');
      }
      for (const agent of instance.agents) {
        if (agent.connector_epoch !== instance.connector_epoch) {
          throw new ProtocolValidationError('Agent connector epoch does not match its instance');
        }
        if (instance.herdr_version === '0.7.3' && agent.herdr_input_revision !== 0) {
          throw new ProtocolValidationError('Herdr 0.7.3 agents must be read-only');
        }
      }
    }
  }
}

function assertPromptSemantics(message: PromptSnapshotMessage): void {
  assertUnique(message.body.options.map((option) => option.id), 'prompt option identifier');
}

function assertSemanticMessage(message: BrowserMessage): void {
  switch (message.type) {
    case 'session.snapshot':
      assertSnapshotSemantics(message);
      return;
    case 'prompt.snapshot':
      assertPromptSemantics(message);
      return;
    case 'state.delta':
    case 'state.resync':
    case 'output.subscribe':
    case 'output.unsubscribe':
    case 'output.snapshot':
    case 'action.request':
    case 'action.received':
    case 'action.result':
    case 'protocol.error':
      return;
    default:
      return assertNever(message);
  }
}

function assertNever(value: never): never {
  throw new ProtocolValidationError(`Unsupported message: ${String(value)}`);
}

export function validateMessage(value: unknown): BrowserMessage {
  if (!validateSchema(value)) {
    throw new ProtocolValidationError('Message failed browser protocol v1 validation', validateSchema.errors ?? []);
  }
  const message = value as BrowserMessage;
  assertSemanticMessage(message);
  return message;
}

export function validateInbound(value: unknown): InboundMessage {
  const message = validateMessage(value);
  if (!inboundTypes.has(message.type as InboundMessage['type'])) {
    throw new ProtocolValidationError('Message type is not valid from the control plane');
  }
  return message as InboundMessage;
}

export function validateOutbound(value: unknown): OutboundMessage {
  const message = validateMessage(value);
  if (!outboundTypes.has(message.type as OutboundMessage['type'])) {
    throw new ProtocolValidationError('Message type is not valid from the browser');
  }
  return message as OutboundMessage;
}
