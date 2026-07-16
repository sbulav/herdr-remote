// Package connector contains the outbound connector state machine. State and
// terminal content are memory-only; only the central control plane is durable.
package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dcolinmorgan/herdr-remote/internal/herdr"
	"github.com/dcolinmorgan/herdr-remote/internal/prompt"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
)

type Local interface {
	Ping(context.Context) (herdr.Ping, error)
	Snapshot(context.Context) (herdr.Snapshot, error)
	AgentGet(context.Context, string) (herdr.AgentInfo, error)
	Read(context.Context, string, string, int) (herdr.PaneRead, error)
	ReadAgent(context.Context, string, string, int) (herdr.PaneRead, error)
	InspectSchema(context.Context) (herdr.APISchema, error)
	SendChecked(context.Context, herdr.CheckedInput) (herdr.CheckedAck, error)
	Subscribe(context.Context, []herdr.SubscriptionSpec) (*herdr.Subscription, error)
}

type Config struct {
	HostID, InstanceID string
	ReconcileInterval  time.Duration
}
type Engine struct {
	cfg                Config
	local              Local
	mu                 sync.RWMutex
	reconcileMu        sync.Mutex
	stateStreamMu      sync.Mutex
	state              protocol.InstanceSnapshot
	byTerminal         map[string]protocol.Agent
	checked            bool
	completed          map[string]struct{}
	completedOrder     []string
	locks              map[string]chan struct{}
	subscriptions      []*herdr.Subscription
	outputMu           sync.Mutex
	outputs            map[string]context.CancelFunc
	outputTargets      map[string]string
	promptFingerprints map[string]string
	actionSlots        chan struct{}
	rateMu             sync.Mutex
	rateTokens         float64
	lastRate           time.Time
	statePublisher     func(protocol.InstanceSnapshot) error
	deltaPublisher     func(protocol.StateDelta) error
}

type statePublishError struct{ err error }

func (e *statePublishError) Error() string { return e.err.Error() }
func (e *statePublishError) Unwrap() error { return e.err }

func NewEngine(cfg Config, local Local) (*Engine, error) {
	if !protocol.IsUUID(cfg.HostID) || cfg.InstanceID == "" {
		return nil, errors.New("invalid connector engine identity")
	}
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = 30 * time.Second
	}
	return &Engine{cfg: cfg, local: local, byTerminal: map[string]protocol.Agent{}, completed: map[string]struct{}{}, locks: map[string]chan struct{}{}, outputs: map[string]context.CancelFunc{}, outputTargets: map[string]string{}, promptFingerprints: map[string]string{}, actionSlots: make(chan struct{}, 32), rateTokens: 10, lastRate: time.Now()}, nil
}

func (e *Engine) Snapshot() protocol.InstanceSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneSnapshot(e.state)
}

// Reconcile performs subscribe-snapshot-subscribe-snapshot. The second
// snapshot is authoritative, so events cannot create a bootstrap gap. Initial
// and compatibility changes publish snapshots; ordinary agent changes keep
// the epoch and publish sequenced deltas upstream.
func (e *Engine) Reconcile(ctx context.Context) (protocol.InstanceSnapshot, error) {
	snapshot, _, err := e.reconcile(ctx)
	return snapshot, err
}

func (e *Engine) reconcile(ctx context.Context) (protocol.InstanceSnapshot, *protocol.StateDelta, error) {
	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()
	ping, err := e.local.Ping(ctx)
	if err != nil {
		return protocol.InstanceSnapshot{}, nil, err
	}
	global, err := e.local.Subscribe(ctx, []herdr.SubscriptionSpec{{Type: "pane.created"}, {Type: "pane.closed"}, {Type: "pane.agent_detected"}, {Type: "pane.exited"}})
	if err != nil {
		return protocol.InstanceSnapshot{}, nil, err
	}
	first, err := e.local.Snapshot(ctx)
	if err != nil {
		global.Close()
		return protocol.InstanceSnapshot{}, nil, err
	}
	specs := make([]herdr.SubscriptionSpec, 0, len(first.Snapshot.Agents))
	for _, a := range first.Snapshot.Agents {
		if a.PaneID != "" {
			specs = append(specs, herdr.SubscriptionSpec{Type: "pane.agent_status_changed", PaneID: a.PaneID})
		}
	}
	paneSub, err := e.local.Subscribe(ctx, specs)
	if err != nil {
		global.Close()
		return protocol.InstanceSnapshot{}, nil, err
	}
	authoritative, err := e.local.Snapshot(ctx)
	if err != nil {
		global.Close()
		paneSub.Close()
		return protocol.InstanceSnapshot{}, nil, err
	}
	schema, schemaErr := e.local.InspectSchema(ctx)
	checked := schemaErr == nil && herdr.SupportsCheckedInput(ping, schema)
	epoch, err := protocol.NewUUIDv7()
	if err != nil {
		return protocol.InstanceSnapshot{}, nil, err
	}
	state := protocol.InstanceSnapshot{InstanceID: e.cfg.InstanceID, Epoch: epoch, Sequence: 0, HerdrVersion: ping.Version, HerdrProtocol: ping.Protocol, Status: "online", Capabilities: []string{"read.v1", "output.subscribe.v1", "prompt.snapshot.v1"}}
	if checked {
		state.Capabilities = append(state.Capabilities, "checked_input.v1", "prompt.respond.v1")
	}
	if ping.Protocol <= 0 {
		state.Status = "incompatible"
		state.Capabilities = nil
	}
	if len(authoritative.Snapshot.Agents) > protocol.MaxAgents {
		global.Close()
		paneSub.Close()
		return protocol.InstanceSnapshot{}, nil, errors.New("Herdr agent limit exceeded")
	}
	byTerminal := map[string]protocol.Agent{}
	for _, a := range authoritative.Snapshot.Agents {
		if a.TerminalID == "" || a.Agent == "" {
			continue
		}
		rev := a.EffectiveRevision()
		if !checked {
			rev = 0
		}
		pa := protocol.Agent{TerminalID: a.TerminalID, PaneID: a.PaneID, WorkspaceID: a.WorkspaceID, TabID: a.TabID, Agent: a.Agent, DisplayName: a.DisplayName, Status: normalizeStatus(a.AgentStatus), Project: herdr.RedactedProject(a.CWD), Generation: 1, HerdrInputRevision: rev}
		if err := protocol.ValidateSnapshot(protocol.InstanceSnapshot{InstanceID: e.cfg.InstanceID, Epoch: epoch, Sequence: 0, HerdrVersion: ping.Version, HerdrProtocol: ping.Protocol, Status: "online", Capabilities: state.Capabilities, Agents: []protocol.Agent{pa}}); err != nil {
			global.Close()
			paneSub.Close()
			return protocol.InstanceSnapshot{}, nil, err
		}
		state.Agents = append(state.Agents, pa)
		byTerminal[pa.TerminalID] = pa
	}
	e.mu.Lock()
	previous := e.state
	var delta *protocol.StateDelta
	if sameInstanceMetadata(previous, state) {
		changes := reconcileAgentChanges(previous.Agents, state.Agents)
		if len(changes) == 0 {
			state = previous
			byTerminal = cloneAgentMap(e.byTerminal)
		} else {
			state.Epoch = previous.EffectiveEpoch()
			state.Sequence = previous.Sequence + 1
			byTerminal = make(map[string]protocol.Agent, len(state.Agents))
			for _, agent := range state.Agents {
				byTerminal[agent.TerminalID] = agent
			}
			delta = &protocol.StateDelta{InstanceID: state.InstanceID, Epoch: state.EffectiveEpoch(), Sequence: state.Sequence, Changes: changes}
		}
	}
	old := e.subscriptions
	e.subscriptions = []*herdr.Subscription{global, paneSub}
	e.state = state
	e.byTerminal = byTerminal
	e.checked = checked
	if previous.EffectiveEpoch() != state.EffectiveEpoch() {
		e.promptFingerprints = map[string]string{}
	} else if delta != nil {
		for _, change := range delta.Changes {
			if change.Operation == "remove" || (change.Agent != nil && change.Agent.Status != "blocked") {
				terminalID := change.TerminalID
				if change.Agent != nil {
					terminalID = change.Agent.TerminalID
				}
				delete(e.promptFingerprints, terminalID)
			}
		}
	}
	e.mu.Unlock()
	for _, s := range old {
		_ = s.Close()
	}
	return cloneSnapshot(state), delta, nil
}

// PollPrompts samples blocked agents, increments their connector generation on
// a prompt change, and returns a sequenced state delta before prompt snapshots.
func (e *Engine) PollPrompts(ctx context.Context) ([]protocol.PromptSnapshot, *protocol.StateDelta, error) {
	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()
	e.mu.RLock()
	state := cloneSnapshot(e.state)
	e.mu.RUnlock()
	if state.EffectiveEpoch() == "" {
		return nil, nil, nil
	}
	var prompts []protocol.PromptSnapshot
	var changes []protocol.StateChange
	for _, a := range state.Agents {
		if a.Status != "blocked" {
			continue
		}
		read, err := e.readAgent(ctx, a, "detection", 1000)
		if err != nil {
			return nil, nil, err
		}
		p := prompt.Extract(prompt.Input{Text: read.Read.Text, HostID: e.cfg.HostID, InstanceID: e.cfg.InstanceID, TerminalID: a.TerminalID})
		contentHash := p.ContentHash
		e.mu.RLock()
		checked := e.checked
		e.mu.RUnlock()
		if checked {
			contentHash = read.Read.ContentHash
		}
		e.mu.Lock()
		previous := e.promptFingerprints[a.TerminalID]
		if previous == p.Fingerprint {
			e.mu.Unlock()
			continue
		}
		e.promptFingerprints[a.TerminalID] = p.Fingerprint
		current, ok := e.byTerminal[a.TerminalID]
		if !ok {
			e.mu.Unlock()
			continue
		}
		current.Generation = current.EffectiveGeneration() + 1
		current.AgentGeneration = 0
		current.HerdrInputRevision = read.Read.EffectiveRevision()
		e.byTerminal[a.TerminalID] = current
		for i := range e.state.Agents {
			if e.state.Agents[i].TerminalID == a.TerminalID {
				e.state.Agents[i] = current
				break
			}
		}
		e.mu.Unlock()
		options := make([]protocol.PromptOption, 0, len(p.Options))
		for _, o := range p.Options {
			options = append(options, o.PromptOption)
		}
		prompts = append(prompts, protocol.PromptSnapshot{Target: protocol.Target{HostID: e.cfg.HostID, InstanceID: e.cfg.InstanceID, TerminalID: a.TerminalID}, AgentGeneration: current.EffectiveGeneration(), HerdrInputRevision: read.Read.EffectiveRevision(), HerdrContentHash: contentHash, Fingerprint: p.Fingerprint, Excerpt: p.Excerpt, ExcerptTruncated: p.Truncated, AdapterVersion: prompt.Version, Options: options})
		copy := current
		changes = append(changes, protocol.StateChange{Operation: "upsert", Agent: &copy})
	}
	if len(changes) == 0 {
		return nil, nil, nil
	}
	e.mu.Lock()
	e.state.Sequence++
	sequence := e.state.Sequence
	epoch := e.state.EffectiveEpoch()
	e.mu.Unlock()
	for i := range prompts {
		prompts[i].StateEpoch = epoch
		prompts[i].StateSequence = sequence
	}
	delta := &protocol.StateDelta{InstanceID: e.cfg.InstanceID, Epoch: epoch, Sequence: sequence, Changes: changes}
	return prompts, delta, nil
}

func (e *Engine) pollPromptsAndPublish(ctx context.Context) ([]protocol.PromptSnapshot, error) {
	e.stateStreamMu.Lock()
	defer e.stateStreamMu.Unlock()
	prompts, delta, err := e.PollPrompts(ctx)
	if err != nil || delta == nil {
		return prompts, err
	}
	e.mu.RLock()
	publish := e.deltaPublisher
	e.mu.RUnlock()
	if publish == nil {
		return nil, &statePublishError{err: errors.New("state delta publisher unavailable")}
	}
	if err := publish(*delta); err != nil {
		return nil, &statePublishError{err: err}
	}
	return prompts, nil
}

func (e *Engine) reconcileAndPublish(ctx context.Context, forceSnapshot bool) (protocol.InstanceSnapshot, error) {
	e.stateStreamMu.Lock()
	defer e.stateStreamMu.Unlock()
	before := e.Snapshot().EffectiveEpoch()
	snapshot, delta, err := e.reconcile(ctx)
	if err != nil {
		return protocol.InstanceSnapshot{}, err
	}
	e.mu.RLock()
	publishSnapshot := e.statePublisher
	publishDelta := e.deltaPublisher
	e.mu.RUnlock()
	if delta != nil {
		if publishDelta == nil {
			return protocol.InstanceSnapshot{}, errors.New("state delta publisher unavailable")
		}
		err = publishDelta(*delta)
	} else if forceSnapshot || snapshot.EffectiveEpoch() != before {
		if publishSnapshot == nil {
			return protocol.InstanceSnapshot{}, errors.New("state publisher unavailable")
		}
		err = publishSnapshot(snapshot)
	}
	return snapshot, err
}

func (e *Engine) RunReconciliation(ctx context.Context, publish func(protocol.InstanceSnapshot) error, publishDelta func(protocol.StateDelta) error) error {
	e.SetStatePublisher(publish)
	e.SetDeltaPublisher(publishDelta)
	if _, err := e.reconcileAndPublish(ctx, true); err != nil {
		return err
	}
	t := time.NewTimer(e.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		e.mu.RLock()
		subs := append([]*herdr.Subscription(nil), e.subscriptions...)
		e.mu.RUnlock()
		var events1, events2 <-chan herdr.Event
		var errors1, errors2 <-chan error
		if len(subs) > 0 {
			events1, errors1 = subs[0].Events, subs[0].Errors
		}
		if len(subs) > 1 {
			events2, errors2 = subs[1].Events, subs[1].Errors
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-events1:
		case <-events2:
		case err := <-errors1:
			if err != nil {
				return err
			}
		case err := <-errors2:
			if err != nil {
				return err
			}
		}
		if _, err := e.reconcileAndPublish(ctx, false); err != nil {
			return err
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(e.cfg.ReconcileInterval)
	}
}
func (e *Engine) BeginConnection() {
	e.Stop()
	e.mu.Lock()
	e.state = protocol.InstanceSnapshot{}
	e.byTerminal = map[string]protocol.Agent{}
	e.promptFingerprints = map[string]string{}
	e.mu.Unlock()
}
func (e *Engine) ForceReconcile(ctx context.Context) (protocol.InstanceSnapshot, error) {
	e.mu.Lock()
	e.state = protocol.InstanceSnapshot{}
	e.byTerminal = map[string]protocol.Agent{}
	e.promptFingerprints = map[string]string{}
	e.mu.Unlock()
	return e.Reconcile(ctx)
}
func (e *Engine) forceReconcileAndPublish(ctx context.Context) error {
	e.stateStreamMu.Lock()
	defer e.stateStreamMu.Unlock()
	e.mu.Lock()
	e.state = protocol.InstanceSnapshot{}
	e.byTerminal = map[string]protocol.Agent{}
	e.promptFingerprints = map[string]string{}
	publish := e.statePublisher
	e.mu.Unlock()
	if publish == nil {
		return errors.New("state publisher unavailable")
	}
	snapshot, _, err := e.reconcile(ctx)
	if err != nil {
		return err
	}
	return publish(snapshot)
}
func sameInstanceMetadata(a, b protocol.InstanceSnapshot) bool {
	return a.EffectiveEpoch() != "" && a.InstanceID == b.InstanceID && a.HerdrVersion == b.HerdrVersion && a.HerdrProtocol == b.HerdrProtocol && a.Status == b.Status && slices.Equal(a.Capabilities, b.Capabilities)
}
func reconcileAgentChanges(previous, current []protocol.Agent) []protocol.StateChange {
	previousByID := make(map[string]protocol.Agent, len(previous))
	for _, agent := range previous {
		previousByID[agent.TerminalID] = agent
	}
	currentIDs := make(map[string]struct{}, len(current))
	changes := make([]protocol.StateChange, 0)
	for i := range current {
		agent := &current[i]
		currentIDs[agent.TerminalID] = struct{}{}
		if old, exists := previousByID[agent.TerminalID]; exists {
			old.Generation = old.EffectiveGeneration()
			old.AgentGeneration = 0
			agent.Generation = old.EffectiveGeneration()
			agent.AgentGeneration = 0
			if reflect.DeepEqual(old, *agent) {
				continue
			}
			agent.Generation++
		}
		copy := *agent
		changes = append(changes, protocol.StateChange{Operation: "upsert", Agent: &copy})
	}
	for _, agent := range previous {
		if _, exists := currentIDs[agent.TerminalID]; !exists {
			changes = append(changes, protocol.StateChange{Operation: "remove", TerminalID: agent.TerminalID, Reason: "reconciled"})
		}
	}
	return changes
}
func cloneAgentMap(in map[string]protocol.Agent) map[string]protocol.Agent {
	out := make(map[string]protocol.Agent, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
func (e *Engine) SetStatePublisher(publish func(protocol.InstanceSnapshot) error) {
	e.mu.Lock()
	e.statePublisher = publish
	e.mu.Unlock()
}
func (e *Engine) SetDeltaPublisher(publish func(protocol.StateDelta) error) {
	e.mu.Lock()
	e.deltaPublisher = publish
	e.mu.Unlock()
}

type Response struct {
	Received bool
	Result   protocol.ActionResult
}

func (e *Engine) HandleAction(parent context.Context, req protocol.ActionRequest) Response {
	return e.HandleActionWithReceipt(parent, req, nil)
}

// HandleActionWithReceipt invokes onReceived after final validation and
// immediately before the first local execution call.
func (e *Engine) HandleActionWithReceipt(parent context.Context, req protocol.ActionRequest, onReceived func() bool) Response {
	op := req.Operation.Type
	result := protocol.ActionResult{ActionID: req.ActionID, OperationType: op, Message: json.RawMessage("null"), Result: json.RawMessage("null")}
	reject := func(code string) Response {
		result.Status = "rejected"
		result.Code = &code
		return Response{Result: result}
	}
	if err := protocol.ValidateAction(req, false); err != nil {
		return reject("INVALID_MESSAGE")
	}
	if !e.allowAction() {
		return reject("RATE_LIMITED")
	}
	select {
	case e.actionSlots <- struct{}{}:
		defer func() { <-e.actionSlots }()
	default:
		return reject("BUSY")
	}
	e.mu.Lock()
	if _, ok := e.completed[req.ActionID]; ok {
		e.mu.Unlock()
		return reject("DUPLICATE_ACTION")
	}
	e.rememberLocked(req.ActionID)
	e.mu.Unlock()
	if req.Target.HostID != e.cfg.HostID {
		return reject("UNAUTHORIZED_HOST")
	}
	if req.Target.InstanceID != e.cfg.InstanceID {
		return reject("TARGET_NOT_FOUND")
	}
	e.mu.RLock()
	agent, ok := e.byTerminal[req.Target.TerminalID]
	state := e.state
	checked := e.checked
	e.mu.RUnlock()
	if !ok {
		return reject("TARGET_NOT_FOUND")
	}
	if req.Expected.StateEpoch != state.EffectiveEpoch() || req.Expected.AgentGeneration != agent.EffectiveGeneration() || req.Expected.HerdrInputRevision != agent.HerdrInputRevision || req.Expected.Agent != agent.Agent || !slices.Contains(req.Expected.Statuses, agent.Status) {
		return reject("STALE_STATE")
	}
	if op == "prompt.respond" && agent.Status != "blocked" {
		return reject("STALE_STATE")
	}
	if protocol.IsWrite(op) && (!checked || agent.HerdrInputRevision == 0) {
		return reject("HERDR_INCOMPATIBLE")
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(req.TimeoutMS)*time.Millisecond)
	defer cancel()
	lock := e.terminalLock(agent.TerminalID)
	select {
	case <-ctx.Done():
		return reject("DEADLINE_EXCEEDED")
	case lock <- struct{}{}:
	}
	defer func() { <-lock }()
	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()
	e.mu.RLock()
	latest, stillPresent := e.byTerminal[agent.TerminalID]
	latestEpoch := e.state.EffectiveEpoch()
	e.mu.RUnlock()
	if !stillPresent || latestEpoch != req.Expected.StateEpoch || latest.EffectiveGeneration() != req.Expected.AgentGeneration || latest.HerdrInputRevision != req.Expected.HerdrInputRevision {
		return reject("STALE_STATE")
	}
	current, err := e.local.AgentGet(ctx, agent.TerminalID)
	if err != nil {
		return reject("HERDR_UNAVAILABLE")
	}
	if current.Agent.TerminalID != agent.TerminalID || current.Agent.PaneID != agent.PaneID {
		return reject("STALE_TARGET")
	}
	if current.Agent.Agent == "" {
		return reject("NOT_AN_AGENT")
	}
	if current.Agent.Agent != agent.Agent || normalizeStatus(current.Agent.AgentStatus) != agent.Status || current.Agent.EffectiveRevision() != agent.HerdrInputRevision {
		return reject("STALE_STATE")
	}
	if op == "prompt.respond" && normalizeStatus(current.Agent.AgentStatus) != "blocked" {
		return reject("STALE_STATE")
	}
	if err := ctx.Err(); err != nil {
		return reject("DEADLINE_EXCEEDED")
	}
	if op == "agent.read" {
		if onReceived != nil && !onReceived() {
			code := "INTERNAL"
			result.Status = "failed"
			result.Code = &code
			return Response{Result: result}
		}
		return e.executeRead(ctx, req, agent, result)
	}
	input := herdr.CheckedInput{TerminalID: agent.TerminalID, ExpectedInputRevision: agent.HerdrInputRevision, ExpectedAgent: agent.Agent, ExpectedStatus: agent.Status}
	switch op {
	case "agent.send_text":
		input.Text = *req.Operation.Text
	case "agent.send_keys":
		input.Keys = req.Operation.Keys
	case "agent.send_input":
		if req.Operation.Text != nil {
			input.Text = *req.Operation.Text
		}
		input.Keys = req.Operation.Keys
	case "agent.interrupt":
		input.Keys = []string{"ctrl+c"}
	case "prompt.respond":
		read, err := e.readAgent(ctx, agent, "detection", 1000)
		if err != nil {
			return reject("HERDR_UNAVAILABLE")
		}
		p := prompt.Extract(prompt.Input{Text: read.Read.Text, HostID: e.cfg.HostID, InstanceID: e.cfg.InstanceID, TerminalID: agent.TerminalID})
		if p.Fingerprint != req.Expected.PromptFingerprint || read.Read.ContentHash != req.Expected.HerdrContentHash {
			return reject("PROMPT_CHANGED")
		}
		var found *prompt.BoundOption
		for i := range p.Options {
			if p.Options[i].ID == req.Operation.OptionID {
				found = &p.Options[i]
				break
			}
		}
		if found == nil {
			return reject("PROMPT_CHANGED")
		}
		input.Text = found.Text
		input.Keys = found.Keys
		input.ExpectedContentHash = p.ContentHash
	}
	if onReceived != nil && !onReceived() {
		code := "INTERNAL"
		result.Status = "rejected"
		result.Code = &code
		return Response{Result: result}
	}
	ack, err := e.local.SendChecked(ctx, input)
	if err != nil && herdr.DefinitiveRejection(err) {
		code := "HERDR_REJECTED"
		result.Status = "failed"
		result.Code = &code
		return Response{Received: true, Result: result}
	}
	if err != nil || !ack.Enqueued || ack.InputRevision <= agent.HerdrInputRevision {
		code := "OUTCOME_UNKNOWN"
		result.Status = "unknown"
		result.Code = &code
		return Response{Received: true, Result: result}
	}
	result.Status = "succeeded"
	result.Code = nil
	wr := protocol.WriteResult{HerdrAcknowledged: true}
	if op == "prompt.respond" {
		wr.OptionID = req.Operation.OptionID
	}
	result.Result = mustJSON(wr)
	return Response{Received: true, Result: result}
}

func (e *Engine) allowAction() bool {
	e.rateMu.Lock()
	defer e.rateMu.Unlock()
	now := time.Now()
	elapsed := now.Sub(e.lastRate).Seconds()
	e.lastRate = now
	e.rateTokens = min(10, e.rateTokens+elapsed)
	if e.rateTokens < 1 {
		return false
	}
	e.rateTokens--
	return true
}

func (e *Engine) executeRead(ctx context.Context, req protocol.ActionRequest, a protocol.Agent, result protocol.ActionResult) Response {
	r, err := e.readAgent(ctx, a, req.Operation.Source, *req.Operation.Lines)
	if err != nil {
		code := "HERDR_REJECTED"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = "DEADLINE_EXCEEDED"
		}
		result.Status = "failed"
		result.Code = &code
		return Response{Received: true, Result: result}
	}
	text, truncated := limitRunes(r.Read.Text, protocol.MaxOutputRunes)
	h := sha256.Sum256([]byte(text))
	result.Status = "succeeded"
	result.Result = mustJSON(protocol.ReadResult{StateEpoch: req.Expected.StateEpoch, AgentGeneration: a.EffectiveGeneration(), HerdrInputRevision: r.Read.EffectiveRevision(), Text: text, Truncated: r.Read.Truncated || truncated, ContentRevision: "sha256:" + hex.EncodeToString(h[:])})
	return Response{Received: true, Result: result}
}

func (e *Engine) StartOutput(parent context.Context, s protocol.OutputSubscribe, publish func(protocol.OutputSnapshot) error) error {
	if err := protocol.ValidateOutputSubscribe(s, false); err != nil {
		return err
	}
	if s.Target.HostID != e.cfg.HostID || s.Target.InstanceID != e.cfg.InstanceID {
		return errors.New("wrong output target")
	}
	e.mu.RLock()
	_, targetExists := e.byTerminal[s.Target.TerminalID]
	canRead := protocol.HasCapability(e.state.Capabilities, "output.subscribe.v1")
	e.mu.RUnlock()
	if !targetExists {
		return errors.New("output target not found")
	}
	if !canRead {
		return errors.New("output unavailable")
	}
	e.outputMu.Lock()
	defer e.outputMu.Unlock()
	if _, ok := e.outputs[s.SubscriptionID]; ok {
		return errors.New("duplicate subscription")
	}
	if len(e.outputs) >= 4 {
		return errors.New("output subscription limit")
	}
	for _, terminal := range e.outputTargets {
		if terminal == s.Target.TerminalID {
			return errors.New("terminal already subscribed")
		}
	}
	ctx, cancel := context.WithCancel(parent)
	e.outputs[s.SubscriptionID] = cancel
	e.outputTargets[s.SubscriptionID] = s.Target.TerminalID
	go func() {
		defer func() {
			e.outputMu.Lock()
			delete(e.outputs, s.SubscriptionID)
			delete(e.outputTargets, s.SubscriptionID)
			e.outputMu.Unlock()
		}()
		ticker := time.NewTicker(time.Duration(s.PollIntervalMS) * time.Millisecond)
		defer ticker.Stop()
		last := ""
		for {
			if err := e.pollOutput(ctx, s, &last, publish); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}
func (e *Engine) pollOutput(ctx context.Context, s protocol.OutputSubscribe, last *string, publish func(protocol.OutputSnapshot) error) error {
	e.mu.RLock()
	a, ok := e.byTerminal[s.Target.TerminalID]
	state := e.state
	checked := e.checked
	e.mu.RUnlock()
	if !ok {
		return errors.New("target gone")
	}
	r, err := e.readAgent(ctx, a, s.Source, s.Lines)
	if err != nil {
		return err
	}
	if checked && r.Read.EffectiveRevision() != a.HerdrInputRevision {
		_, reconcileErr := e.reconcileAndPublish(ctx, false)
		if reconcileErr != nil {
			return reconcileErr
		}
		e.mu.RLock()
		a, ok = e.byTerminal[s.Target.TerminalID]
		state = e.state
		e.mu.RUnlock()
		if !ok {
			return errors.New("target gone after reconciliation")
		}
		r, err = e.readAgent(ctx, a, s.Source, s.Lines)
		if err != nil {
			return err
		}
		if r.Read.EffectiveRevision() != a.HerdrInputRevision {
			return errors.New("output revision remained unstable after reconciliation")
		}
	}
	text, tr := limitRunes(r.Read.Text, protocol.MaxOutputRunes)
	h := sha256.Sum256([]byte(text))
	rev := "sha256:" + hex.EncodeToString(h[:])
	if rev == *last {
		return nil
	}
	*last = rev
	return publish(protocol.OutputSnapshot{SubscriptionID: s.SubscriptionID, Target: s.Target, StateEpoch: state.EffectiveEpoch(), AgentGeneration: a.EffectiveGeneration(), HerdrInputRevision: r.Read.EffectiveRevision(), ContentRevision: rev, Text: text, Truncated: r.Read.Truncated || tr})
}

var hashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (e *Engine) readAgent(ctx context.Context, a protocol.Agent, source string, lines int) (herdr.PaneRead, error) {
	e.mu.RLock()
	checked := e.checked
	e.mu.RUnlock()
	if checked {
		read, err := e.local.ReadAgent(ctx, a.TerminalID, source, lines)
		if err != nil {
			return read, err
		}
		if read.Read.EffectiveRevision() == 0 || !hashPattern.MatchString(read.Read.ContentHash) {
			return read, errors.New("checked atomic read omitted revision or content hash")
		}
		return read, nil
	}
	return e.local.Read(ctx, a.PaneID, source, lines)
}
func (e *Engine) StopOutput(id string) {
	e.outputMu.Lock()
	if c := e.outputs[id]; c != nil {
		c()
	}
	e.outputMu.Unlock()
}
func (e *Engine) Stop() {
	e.outputMu.Lock()
	for _, c := range e.outputs {
		c()
	}
	e.outputs = map[string]context.CancelFunc{}
	e.outputTargets = map[string]string{}
	e.outputMu.Unlock()
	e.mu.Lock()
	for _, s := range e.subscriptions {
		_ = s.Close()
	}
	e.subscriptions = nil
	e.mu.Unlock()
}

func (e *Engine) rememberLocked(id string) {
	e.completed[id] = struct{}{}
	e.completedOrder = append(e.completedOrder, id)
	if len(e.completedOrder) > 4096 {
		delete(e.completed, e.completedOrder[0])
		e.completedOrder = e.completedOrder[1:]
	}
}
func (e *Engine) terminalLock(id string) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.locks[id] == nil {
		e.locks[id] = make(chan struct{}, 1)
	}
	return e.locks[id]
}
func normalizeStatus(s string) string {
	switch s {
	case "idle", "working", "blocked", "done":
		return s
	default:
		return "unknown"
	}
}
func cloneSnapshot(s protocol.InstanceSnapshot) protocol.InstanceSnapshot {
	s.Capabilities = append([]string(nil), s.Capabilities...)
	s.Agents = append([]protocol.Agent(nil), s.Agents...)
	return s
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func limitRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	r := []rune(s)
	return string(r[:n]), true
}

var _ = fmt.Sprintf
