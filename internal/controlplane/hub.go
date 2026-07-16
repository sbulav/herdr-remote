// Package controlplane implements the central browser and connector routing
// boundary. It persists action metadata but never terminal or prompt content.
package controlplane

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/dcolinmorgan/herdr-remote/internal/connector"
	"github.com/dcolinmorgan/herdr-remote/internal/enrollment"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
	"github.com/dcolinmorgan/herdr-remote/internal/store"
)

type Metrics struct{ ConnectorConnections, Actions, Rejected, Malformed, AuditFailures atomic.Uint64 }
type Hub struct {
	mu           sync.RWMutex
	leases       map[string]*lease
	hosts        map[string]protocol.HostSnapshot
	listeners    map[uint64]func(string, any)
	nextListener uint64
	store        *store.Store
	log          *slog.Logger
	metrics      *Metrics
	auditBlocked atomic.Bool
	auditMu      sync.Mutex
	auditRepairs map[string]func(context.Context) error
}
type StateEvent struct {
	Changes   []protocol.StateChange
	Attention bool
}
type lease struct {
	hostID, displayName, connectionID, version string
	conn                                       *websocket.Conn
	queue                                      *connector.Queue
	instances                                  map[string]protocol.InstanceSnapshot
	pending                                    map[string]*pending
	outputs                                    map[string]func(protocol.OutputSnapshot)
	closed                                     chan struct{}
	rateTokens                                 float64
	lastRate                                   time.Time
	previousEpochs                             map[string]string
	instanceIDs                                map[string]struct{}
	inventoryExpected, inventoryReceived       bool
}
type pending struct {
	operation    string
	expected     protocol.Expected
	received     chan struct{}
	receivedOnce sync.Once
	result       chan protocol.ActionResult
	done         chan struct{}
	doneOnce     sync.Once
}

func NewHub(st *store.Store, log *slog.Logger, m *Metrics) (*Hub, error) {
	if log == nil {
		log = slog.Default()
	}
	if m == nil {
		m = &Metrics{}
	}
	h := &Hub{leases: map[string]*lease{}, hosts: map[string]protocol.HostSnapshot{}, listeners: map[uint64]func(string, any){}, store: st, log: log, metrics: m, auditRepairs: map[string]func(context.Context) error{}}
	if st != nil {
		hosts, err := st.KnownHosts(context.Background())
		if err != nil {
			return nil, fmt.Errorf("known host recovery: %w", err)
		} else {
			for _, host := range hosts {
				h.hosts[host.HostID] = protocol.HostSnapshot{HostID: host.HostID, DisplayName: host.DisplayName, Status: "disconnected"}
			}
		}
	}
	return h, nil
}

func (h *Hub) Subscribe(fn func(string, any)) func() {
	h.mu.Lock()
	id := h.nextListener
	h.nextListener++
	h.listeners[id] = fn
	h.mu.Unlock()
	return func() { h.mu.Lock(); delete(h.listeners, id); h.mu.Unlock() }
}
func (h *Hub) notify(kind string, v any) {
	h.mu.RLock()
	ls := make([]func(string, any), 0, len(h.listeners))
	for _, fn := range h.listeners {
		ls = append(ls, fn)
	}
	h.mu.RUnlock()
	for _, fn := range ls {
		fn(kind, v)
	}
}

func (h *Hub) Snapshot(sessionID, stateEpoch string) protocol.SessionSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := protocol.SessionSnapshot{SessionID: sessionID, StateEpoch: stateEpoch, Sequence: 0, ServerTime: time.Now().UTC().Format(time.RFC3339Nano), Hosts: make([]protocol.HostSnapshot, 0, len(h.hosts))}
	for _, host := range h.hosts {
		copy := host
		copy.Instances = append([]protocol.BrowserInstance{}, host.Instances...)
		for i := range copy.Instances {
			copy.Instances[i].Capabilities = append([]string{}, copy.Instances[i].Capabilities...)
			copy.Instances[i].Agents = append([]protocol.Agent{}, copy.Instances[i].Agents...)
		}
		out.Hosts = append(out.Hosts, copy)
	}
	return out
}

func (h *Hub) ValidateAction(a protocol.BrowserActionRequest) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	l := h.leases[a.Target.HostID]
	if l == nil {
		return codeError("TARGET_NOT_FOUND")
	}
	i, ok := l.instances[a.Target.InstanceID]
	if !ok {
		return codeError("TARGET_NOT_FOUND")
	}
	if i.EffectiveEpoch() != a.Expected.ConnectorEpoch {
		return codeError("STALE_STATE")
	}
	var current *protocol.Agent
	for n := range i.Agents {
		if i.Agents[n].TerminalID == a.Target.TerminalID {
			current = &i.Agents[n]
			break
		}
	}
	if current == nil {
		return codeError("TARGET_NOT_FOUND")
	}
	if current.EffectiveGeneration() != a.Expected.AgentGeneration || current.HerdrInputRevision != a.Expected.HerdrInputRevision || current.Agent != a.Expected.Agent || !contains(a.Expected.Statuses, current.Status) {
		return codeError("STALE_STATE")
	}
	required := "checked_input.v1"
	if a.Operation.Type == "agent.read" {
		required = "read.v1"
	}
	if !protocol.HasCapability(i.Capabilities, required) {
		return codeError("HERDR_INCOMPATIBLE")
	}
	if a.Operation.Type == "prompt.respond" && !protocol.HasCapability(i.Capabilities, "prompt.respond.v1") {
		return codeError("HERDR_INCOMPATIBLE")
	}
	return nil
}

type ActionHandle struct {
	Received <-chan struct{}
	Result   <-chan protocol.ActionResult
}

func (h *Hub) Dispatch(ctx context.Context, a protocol.ActionRequest) (ActionHandle, error) {
	h.mu.Lock()
	l := h.leases[a.Target.HostID]
	if l == nil {
		h.mu.Unlock()
		return ActionHandle{}, codeError("TARGET_NOT_FOUND")
	}
	p := &pending{operation: a.Operation.Type, expected: a.Expected, received: make(chan struct{}), result: make(chan protocol.ActionResult, 1), done: make(chan struct{})}
	if _, exists := l.pending[a.ActionID]; exists {
		h.mu.Unlock()
		return ActionHandle{}, codeError("DUPLICATE_ACTION")
	}
	if len(l.pending) >= 32 {
		h.mu.Unlock()
		return ActionHandle{}, codeError("BUSY")
	}
	now := time.Now()
	l.rateTokens = min(10, l.rateTokens+now.Sub(l.lastRate).Seconds())
	l.lastRate = now
	if l.rateTokens < 1 {
		h.mu.Unlock()
		return ActionHandle{}, codeError("RATE_LIMITED")
	}
	l.rateTokens--
	l.pending[a.ActionID] = p
	h.mu.Unlock()
	b, err := protocol.MarshalEnvelope(1, "action.request", a)
	if err != nil {
		return ActionHandle{}, err
	}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err = l.queue.Put(qctx, b)
	cancel()
	if err != nil {
		h.mu.Lock()
		delete(l.pending, a.ActionID)
		h.mu.Unlock()
		closePending(p)
		code, status := "CONNECTION_LOST", "failed"
		if protocol.IsWrite(a.Operation.Type) {
			code, status = "OUTCOME_UNKNOWN", "unknown"
		}
		result := protocol.ActionResult{ActionID: a.ActionID, OperationType: a.Operation.Type, Status: status, Code: &code, Result: json.RawMessage("null")}
		h.completeAction(context.Background(), a.ActionID, status, &code, time.Now())
		p.result <- result
		h.closeLease(l)
		return ActionHandle{p.received, p.result}, nil
	}
	go h.expirePending(l, a.ActionID, p, time.Duration(a.TimeoutMS)*time.Millisecond)
	return ActionHandle{p.received, p.result}, nil
}
func (h *Hub) expirePending(l *lease, id string, p *pending, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return
	case <-timer.C:
	}
	h.mu.Lock()
	if l.pending[id] != p {
		h.mu.Unlock()
		return
	}
	delete(l.pending, id)
	h.mu.Unlock()
	closePending(p)
	code, status := "DEADLINE_EXCEEDED", "failed"
	if protocol.IsWrite(p.operation) {
		code, status = "OUTCOME_UNKNOWN", "unknown"
	}
	h.completeAction(context.Background(), id, status, &code, time.Now())
	p.result <- protocol.ActionResult{ActionID: id, OperationType: p.operation, Status: status, Code: &code, Result: json.RawMessage("null")}
}
func (h *Hub) SubscribeOutput(ctx context.Context, s protocol.OutputSubscribe, fn func(protocol.OutputSnapshot)) error {
	h.mu.Lock()
	l := h.leases[s.Target.HostID]
	if l == nil {
		h.mu.Unlock()
		return codeError("TARGET_NOT_FOUND")
	}
	if len(l.outputs) >= 4 {
		h.mu.Unlock()
		return codeError("BUSY")
	}
	instance, ok := l.instances[s.Target.InstanceID]
	if !ok || !protocol.HasCapability(instance.Capabilities, "output.subscribe.v1") {
		h.mu.Unlock()
		return codeError("HERDR_INCOMPATIBLE")
	}
	found := false
	for _, agent := range instance.Agents {
		if agent.TerminalID == s.Target.TerminalID {
			found = true
			break
		}
	}
	if !found {
		h.mu.Unlock()
		return codeError("TARGET_NOT_FOUND")
	}
	l.outputs[s.SubscriptionID] = fn
	h.mu.Unlock()
	b, err := protocol.MarshalEnvelope(1, "output.subscribe", s)
	if err != nil {
		h.mu.Lock()
		delete(l.outputs, s.SubscriptionID)
		h.mu.Unlock()
		return err
	}
	qctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	if err := l.queue.Put(qctx, b); err != nil {
		h.mu.Lock()
		delete(l.outputs, s.SubscriptionID)
		h.mu.Unlock()
		h.closeLease(l)
		return err
	}
	return nil
}
func (h *Hub) UnsubscribeOutput(ctx context.Context, host, id string) {
	h.mu.Lock()
	l := h.leases[host]
	if l == nil {
		for _, candidate := range h.leases {
			if _, ok := candidate.outputs[id]; ok {
				l = candidate
				break
			}
		}
	}
	if l != nil {
		delete(l.outputs, id)
	}
	h.mu.Unlock()
	if l != nil {
		b, _ := protocol.MarshalEnvelope(1, "output.unsubscribe", protocol.OutputUnsubscribe{SubscriptionID: id})
		qctx, c := context.WithTimeout(ctx, 2*time.Second)
		if err := l.queue.Put(qctx, b); err != nil {
			h.closeLease(l)
		}
		c()
	}
}
func (h *Hub) closeLease(l *lease) {
	if l != nil && l.conn != nil {
		_ = l.conn.Close(websocket.StatusTryAgainLater, "priority delivery unavailable")
	}
}

func (h *Hub) ConnectorHandler(w http.ResponseWriter, r *http.Request) {
	cert, record, err := h.authenticateConnector(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_ = cert
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(protocol.MaxFrameBytes)
	ctx := r.Context()
	_, frame, err := c.Read(ctx)
	if err != nil {
		return
	}
	_, decoded, err := protocol.DecodeStrict(frame, "connector")
	if err != nil {
		h.metrics.Malformed.Add(1)
		return
	}
	hello, ok := decoded.(*protocol.Hello)
	if !ok || protocol.ValidateDirection(protocol.ConnectorToControl, "connector.hello") != nil {
		return
	}
	selected, compatible := protocol.NegotiateProtocol(hello.MinProtocol, hello.MaxProtocol, 1, 1)
	if !compatible {
		h.sendProtocolError(ctx, c, "UNSUPPORTED_PROTOCOL")
		_ = c.Close(websocket.StatusCode(4406), "unsupported protocol")
		return
	}
	connectionID, _ := protocol.NewUUIDv7()
	acceptedCapabilities := acceptConnectorCapabilities(hello.Capabilities)
	l := &lease{hostID: record.HostID, displayName: hello.DisplayName, connectionID: connectionID, version: hello.ConnectorVersion, conn: c, queue: connector.NewQueue(256), instances: map[string]protocol.InstanceSnapshot{}, pending: map[string]*pending{}, outputs: map[string]func(protocol.OutputSnapshot){}, closed: make(chan struct{}), rateTokens: 10, lastRate: time.Now(), inventoryExpected: protocol.HasCapability(acceptedCapabilities, protocol.StateInventoryCapability)}
	if err = h.acquire(l); err != nil {
		h.sendProtocolError(ctx, c, "HOST_ALREADY_CONNECTED")
		return
	}
	defer h.release(l)
	welcome := protocol.Welcome{SelectedProtocol: selected, ServerMinProtocol: 1, ServerMaxProtocol: 1, AcceptedCapabilities: acceptedCapabilities, ConnectionID: connectionID, HostID: record.HostID, HeartbeatIntervalMS: 20000, MaxMessageBytes: protocol.MaxFrameBytes, ServerTime: time.Now().UTC().Format(time.RFC3339Nano)}
	b, _ := protocol.MarshalEnvelope(0, "server.welcome", welcome)
	if err = c.Write(ctx, websocket.MessageText, b); err != nil {
		return
	}
	writeErr := make(chan error, 1)
	go func() {
		for {
			b, err := l.queue.Next(ctx)
			if err != nil {
				writeErr <- err
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = c.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				writeErr <- err
				return
			}
		}
	}()
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
				err := c.Ping(pctx)
				cancel()
				if err != nil {
					select {
					case writeErr <- err:
					default:
					}
					return
				}
			}
		}
	}()
	var malformed protocol.MalformedTracker
	for {
		select {
		case <-writeErr:
			return
		default:
		}
		_, frame, err = c.Read(ctx)
		if err != nil {
			return
		}
		_, msg, err := protocol.DecodeStrict(frame, "connector")
		directionErr := protocol.ValidateDirection(protocol.ConnectorToControl, envelopeType(frame))
		if err != nil || directionErr != nil {
			h.metrics.Malformed.Add(1)
			code := "INVALID_MESSAGE"
			if directionErr != nil || (err != nil && stringsContainsText(err.Error(), "unsupported message type")) {
				code = "UNSUPPORTED_MESSAGE"
			}
			if enqueueErr := h.enqueueConnectorError(ctx, l, nil, code); enqueueErr != nil {
				return
			}
			if malformed.Add(time.Now()) {
				return
			}
			continue
		}
		h.mu.RLock()
		inventoryPending := l.inventoryExpected && !l.inventoryReceived
		h.mu.RUnlock()
		if inventoryPending {
			if _, isInventory := msg.(*protocol.InstanceInventory); !isInventory {
				return
			}
		}
		switch m := msg.(type) {
		case *protocol.InstanceInventory:
			if h.updateInventory(l, *m) != nil {
				return
			}
		case *protocol.InstanceSnapshot:
			if h.updateSnapshot(l, *m) != nil {
				return
			}
		case *protocol.StateDelta:
			if h.updateDelta(l, *m) != nil {
				h.mu.RLock()
				current, ok := l.instances[m.InstanceID]
				h.mu.RUnlock()
				var epoch *string
				var sequence *uint64
				if ok {
					value := current.EffectiveEpoch()
					epoch = &value
					next := current.Sequence + 1
					sequence = &next
				}
				body := protocol.ConnectorStateResync{InstanceID: m.InstanceID, ExpectedEpoch: epoch, ExpectedSequence: sequence, Reason: "gap"}
				b, _ := protocol.MarshalEnvelope(1, "state.resync", body)
				qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				err := l.queue.Put(qctx, b)
				cancel()
				if err != nil {
					return
				}
			}
		case *protocol.PromptSnapshot:
			h.routePrompt(l, *m)
		case *protocol.OutputSnapshot:
			h.routeOutput(l, *m)
		case *protocol.ActionReceived:
			h.actionReceived(l, m.ActionID)
		case *protocol.ActionResult:
			h.actionResult(l, *m)
		}
	}
}
func envelopeType(frame []byte) string {
	var value struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(frame, &value)
	return value.Type
}
func stringsContainsText(value, part string) bool {
	return len(part) == 0 || strings.Index(value, part) >= 0
}

func acceptConnectorCapabilities(offered []string) []string {
	supported := map[string]bool{"output.subscribe.v1": true, "prompt.respond.v1": true, protocol.StateInventoryCapability: true}
	accepted := make([]string, 0, len(offered))
	for _, capability := range offered {
		if supported[capability] {
			accepted = append(accepted, capability)
		}
	}
	return accepted
}
func (h *Hub) enqueueConnectorError(ctx context.Context, l *lease, reply *string, code string) error {
	body := protocol.ProtocolError{InReplyTo: reply, Code: code, Message: "connector message rejected"}
	b, err := protocol.MarshalEnvelope(1, "protocol.error", body)
	if err != nil {
		return err
	}
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return l.queue.Put(qctx, b)
}

func (h *Hub) authenticateConnector(r *http.Request) (*x509.Certificate, store.Certificate, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		return nil, store.Certificate{}, errors.New("one client certificate required")
	}
	cert := r.TLS.PeerCertificates[0]
	record, err := h.store.CertificateByFingerprint(r.Context(), enrollment.Fingerprint(cert))
	if err != nil || record.Revoked || time.Now().Before(record.NotBefore) || !time.Now().Before(record.NotAfter) {
		return nil, record, errors.New("certificate not active")
	}
	return cert, record, nil
}
func (h *Hub) acquire(l *lease) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.leases[l.hostID] != nil {
		return errors.New("host already connected")
	}
	previous, known := h.hosts[l.hostID]
	if !known && len(h.hosts) >= 10 {
		return errors.New("host limit reached")
	}
	h.leases[l.hostID] = l
	host := h.hosts[l.hostID]
	if l.previousEpochs == nil {
		l.previousEpochs = map[string]string{}
	}
	for _, instance := range host.Instances {
		l.previousEpochs[instance.InstanceID] = instance.EffectiveEpoch()
	}
	host.HostID = l.hostID
	host.DisplayName = l.displayName
	host.Status = "connected"
	h.hosts[l.hostID] = host
	if err := h.auditEvent(context.Background(), "connector.connect", l.hostID, nil); err != nil {
		delete(h.leases, l.hostID)
		if known {
			h.hosts[l.hostID] = previous
		} else {
			delete(h.hosts, l.hostID)
		}
		return err
	}
	h.metrics.ConnectorConnections.Add(1)
	h.notifyAsync("state.delta", StateEvent{Changes: []protocol.StateChange{{Operation: "host.upsert", HostID: l.hostID, Host: &protocol.HostState{DisplayName: l.displayName, Status: "connected"}}}})
	return nil
}
func (h *Hub) release(l *lease) {
	h.mu.Lock()
	if h.leases[l.hostID] == l {
		delete(h.leases, l.hostID)
	}
	host := h.hosts[l.hostID]
	host.Status = "disconnected"
	h.hosts[l.hostID] = host
	pendingActions := l.pending
	l.pending = map[string]*pending{}
	l.outputs = map[string]func(protocol.OutputSnapshot){}
	h.mu.Unlock()
	for id, p := range pendingActions {
		closePending(p)
		code := "CONNECTION_LOST"
		status := "failed"
		if protocol.IsWrite(p.operation) {
			code = "OUTCOME_UNKNOWN"
			status = "unknown"
		}
		result := protocol.ActionResult{ActionID: id, OperationType: p.operation, Status: status, Code: &code, Result: json.RawMessage("null")}
		select {
		case p.result <- result:
		default:
		}
		h.completeAction(context.Background(), id, status, &code, time.Now())
	}
	_ = h.auditEvent(context.Background(), "connector.disconnect", l.hostID, nil)
	h.notify("state.delta", StateEvent{Changes: []protocol.StateChange{{Operation: "host.upsert", HostID: l.hostID, Host: &protocol.HostState{DisplayName: host.DisplayName, Status: "disconnected"}}}})
}

func (h *Hub) updateInventory(l *lease, inventory protocol.InstanceInventory) error {
	if len(inventory.InstanceIDs) < 1 || len(inventory.InstanceIDs) > protocol.MaxInstances {
		return errors.New("invalid instance inventory")
	}
	configured := make(map[string]struct{}, len(inventory.InstanceIDs))
	for _, instanceID := range inventory.InstanceIDs {
		if _, duplicate := configured[instanceID]; duplicate {
			return errors.New("duplicate instance inventory entry")
		}
		configured[instanceID] = struct{}{}
	}
	h.mu.Lock()
	if h.leases[l.hostID] != l {
		h.mu.Unlock()
		return errors.New("inactive connector lease")
	}
	if !l.inventoryExpected || l.inventoryReceived {
		h.mu.Unlock()
		return errors.New("unexpected instance inventory")
	}
	host := h.hosts[l.hostID]
	retained := make([]protocol.BrowserInstance, 0, len(host.Instances))
	changes := make([]protocol.StateChange, 0, len(host.Instances))
	for _, instance := range host.Instances {
		if _, returning := configured[instance.InstanceID]; returning {
			retained = append(retained, instance)
			continue
		}
		delete(l.previousEpochs, instance.InstanceID)
		delete(l.instances, instance.InstanceID)
		changes = append(changes, protocol.StateChange{Operation: "instance.remove", HostID: l.hostID, InstanceID: instance.InstanceID, Reason: "unconfigured"})
	}
	host.Instances = retained
	h.hosts[l.hostID] = host
	l.instanceIDs = configured
	l.inventoryReceived = true
	h.mu.Unlock()
	if len(changes) > 0 {
		h.notify("state.delta", StateEvent{Changes: changes})
	}
	return nil
}

func (h *Hub) updateSnapshot(l *lease, s protocol.InstanceSnapshot) error {
	h.mu.Lock()
	if h.leases[l.hostID] != l {
		h.mu.Unlock()
		return errors.New("inactive connector lease")
	}
	if l.inventoryExpected && !l.inventoryReceived {
		h.mu.Unlock()
		return errors.New("instance inventory required before snapshots")
	}
	if l.instanceIDs != nil {
		if _, configured := l.instanceIDs[s.InstanceID]; !configured {
			h.mu.Unlock()
			return errors.New("snapshot names undeclared instance")
		}
	}
	old, exists := l.instances[s.InstanceID]
	previousEpoch := l.previousEpochs[s.InstanceID]
	if !exists && previousEpoch == s.EffectiveEpoch() && previousEpoch != "" {
		h.mu.Unlock()
		return errors.New("connector reused previous epoch after reconnect")
	}
	if exists && old.EffectiveEpoch() == s.EffectiveEpoch() {
		h.mu.Unlock()
		if !reflect.DeepEqual(old, s) {
			return errors.New("snapshot changed without new epoch")
		}
		return nil
	}
	if !exists && len(l.instances) >= protocol.MaxInstances {
		h.mu.Unlock()
		return errors.New("instance limit exceeded")
	}
	host := h.hosts[l.hostID]
	oldStatuses := map[string]string{}
	if exists {
		for _, agent := range old.Agents {
			oldStatuses[agent.TerminalID] = agent.Status
		}
	} else {
		for _, instance := range host.Instances {
			if instance.InstanceID == s.InstanceID {
				for _, agent := range instance.Agents {
					oldStatuses[agent.TerminalID] = agent.Status
				}
				break
			}
		}
	}
	attention := false
	for _, agent := range s.Agents {
		if agent.Status == "blocked" && oldStatuses[agent.TerminalID] != "blocked" {
			attention = true
			break
		}
	}
	l.instances[s.InstanceID] = s
	delete(l.previousEpochs, s.InstanceID)
	projected := projectInstance(s)
	replaced := false
	for i := range host.Instances {
		if host.Instances[i].InstanceID == s.InstanceID {
			host.Instances[i] = projected
			replaced = true
			break
		}
	}
	if !replaced {
		host.Instances = append(host.Instances, projected)
	}
	h.hosts[l.hostID] = host
	h.mu.Unlock()
	if exists || previousEpoch != "" {
		oldEpoch := old.EffectiveEpoch()
		if !exists {
			oldEpoch = previousEpoch
		}
		h.notify("state.delta", StateEvent{Changes: []protocol.StateChange{{Operation: "instance.epoch_changed", HostID: l.hostID, InstanceID: s.InstanceID, PreviousConnectorEpoch: oldEpoch, ConnectorEpoch: s.EffectiveEpoch()}}})
	} else {
		changes := []protocol.StateChange{{Operation: "instance.upsert", HostID: l.hostID, InstanceID: s.InstanceID, Instance: &protocol.InstanceState{ConnectorEpoch: s.EffectiveEpoch(), HerdrVersion: s.HerdrVersion, HerdrProtocol: s.HerdrProtocol, Status: s.Status, Capabilities: append([]string(nil), s.Capabilities...)}}}
		for _, a := range s.Agents {
			agent := projectAgent(a, s.EffectiveEpoch())
			agent.TerminalID = ""
			changes = append(changes, protocol.StateChange{Operation: "agent.upsert", Target: &protocol.Target{HostID: l.hostID, InstanceID: s.InstanceID, TerminalID: a.TerminalID}, Agent: &agent})
		}
		h.notify("state.delta", StateEvent{Changes: changes})
	}
	if attention {
		h.notify("attention", nil)
	}
	return nil
}
func (h *Hub) updateDelta(l *lease, d protocol.StateDelta) error {
	if err := protocol.ValidateConnectorDelta(d); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := l.instances[d.InstanceID]
	if !ok || state.EffectiveEpoch() != d.Epoch || d.Sequence != state.Sequence+1 {
		return errors.New("connector state sequence mismatch")
	}
	next := state
	next.Agents = append([]protocol.Agent(nil), state.Agents...)
	attention := false
	browserChanges := make([]protocol.StateChange, 0, len(d.Changes))
	for _, change := range d.Changes {
		switch change.Operation {
		case "upsert":
			if change.Agent == nil {
				return errors.New("missing agent")
			}
			found := false
			for i := range next.Agents {
				if next.Agents[i].TerminalID == change.Agent.TerminalID {
					if change.Agent.EffectiveGeneration() <= next.Agents[i].EffectiveGeneration() {
						return errors.New("generation regression")
					}
					if next.Agents[i].Status != "blocked" && change.Agent.Status == "blocked" {
						attention = true
					}
					next.Agents[i] = *change.Agent
					found = true
					break
				}
			}
			if !found {
				next.Agents = append(next.Agents, *change.Agent)
				if change.Agent.Status == "blocked" {
					attention = true
				}
			}
			agent := projectAgent(*change.Agent, d.Epoch)
			agent.TerminalID = ""
			browserChanges = append(browserChanges, protocol.StateChange{Operation: "agent.upsert", Target: &protocol.Target{HostID: l.hostID, InstanceID: d.InstanceID, TerminalID: change.Agent.TerminalID}, Agent: &agent})
		case "remove":
			found := -1
			for i := range next.Agents {
				if next.Agents[i].TerminalID == change.TerminalID {
					found = i
					break
				}
			}
			if found < 0 {
				return errors.New("unknown removal")
			}
			next.Agents = append(next.Agents[:found], next.Agents[found+1:]...)
			browserChanges = append(browserChanges, protocol.StateChange{Operation: "agent.remove", Target: &protocol.Target{HostID: l.hostID, InstanceID: d.InstanceID, TerminalID: change.TerminalID}, Reason: change.Reason})
		default:
			return errors.New("unsupported state change")
		}
	}
	if len(next.Agents) > protocol.MaxAgents {
		return errors.New("agent limit exceeded")
	}
	next.Sequence = d.Sequence
	l.instances[d.InstanceID] = next
	host := h.hosts[l.hostID]
	for i := range host.Instances {
		if host.Instances[i].InstanceID == d.InstanceID {
			host.Instances[i] = projectInstance(next)
			break
		}
	}
	h.hosts[l.hostID] = host
	go h.notify("state.delta", StateEvent{Changes: browserChanges, Attention: attention})
	if attention {
		go h.notify("attention", nil)
	}
	return nil
}
func projectAgent(a protocol.Agent, epoch string) protocol.Agent {
	return protocol.Agent{TerminalID: a.TerminalID, Agent: a.Agent, DisplayName: a.DisplayName, Status: a.Status, Project: a.Project, AgentGeneration: a.EffectiveGeneration(), HerdrInputRevision: a.HerdrInputRevision, ConnectorEpoch: epoch}
}
func projectInstance(src protocol.InstanceSnapshot) protocol.BrowserInstance {
	i := protocol.BrowserInstance{InstanceID: src.InstanceID, ConnectorEpoch: src.EffectiveEpoch(), HerdrVersion: src.HerdrVersion, HerdrProtocol: src.HerdrProtocol, Status: src.Status, Capabilities: append([]string{}, src.Capabilities...), Agents: make([]protocol.Agent, 0, len(src.Agents))}
	for _, a := range src.Agents {
		i.Agents = append(i.Agents, projectAgent(a, src.EffectiveEpoch()))
	}
	return i
}
func (h *Hub) routeOutput(l *lease, o protocol.OutputSnapshot) {
	h.mu.RLock()
	if o.Target.HostID != l.hostID {
		h.mu.RUnlock()
		return
	}
	instance, ok := l.instances[o.Target.InstanceID]
	if !ok || instance.EffectiveEpoch() != o.StateEpoch {
		h.mu.RUnlock()
		return
	}
	valid := false
	for _, agent := range instance.Agents {
		if agent.TerminalID == o.Target.TerminalID && agent.EffectiveGeneration() == o.AgentGeneration && agent.HerdrInputRevision == o.HerdrInputRevision {
			valid = true
			break
		}
	}
	if !valid {
		h.mu.RUnlock()
		return
	}
	fn := l.outputs[o.SubscriptionID]
	h.mu.RUnlock()
	if fn != nil {
		fn(o)
	}
}
func (h *Hub) routePrompt(l *lease, p protocol.PromptSnapshot) {
	h.mu.RLock()
	if p.Target.HostID != l.hostID {
		h.mu.RUnlock()
		return
	}
	instance, ok := l.instances[p.Target.InstanceID]
	if !ok || instance.EffectiveEpoch() != p.StateEpoch {
		h.mu.RUnlock()
		return
	}
	valid := false
	for _, agent := range instance.Agents {
		if agent.TerminalID == p.Target.TerminalID && agent.Status == "blocked" && agent.EffectiveGeneration() == p.AgentGeneration && agent.HerdrInputRevision == p.HerdrInputRevision {
			valid = true
			break
		}
	}
	h.mu.RUnlock()
	if valid {
		h.notify("prompt", p)
	}
}
func (h *Hub) actionReceived(l *lease, id string) {
	h.mu.RLock()
	p := l.pending[id]
	h.mu.RUnlock()
	if p == nil {
		return
	}
	p.receivedOnce.Do(func() { close(p.received) })
	h.receivedAction(context.Background(), id, time.Now())
}
func (h *Hub) actionResult(l *lease, r protocol.ActionResult) {
	h.mu.Lock()
	p := l.pending[r.ActionID]
	if p != nil {
		delete(l.pending, r.ActionID)
		closePending(p)
	}
	h.mu.Unlock()
	if p == nil {
		return
	}
	r.Message = nil
	valid := r.OperationType == p.operation
	if valid && r.Status == "succeeded" && p.operation == "agent.read" {
		var read protocol.ReadResult
		if json.Unmarshal(r.Result, &read) != nil || read.StateEpoch != p.expected.StateEpoch || read.AgentGeneration != p.expected.AgentGeneration || read.HerdrInputRevision != p.expected.HerdrInputRevision {
			valid = false
		}
	}
	if !valid {
		code := "INTERNAL"
		status := "failed"
		if protocol.IsWrite(p.operation) {
			code = "OUTCOME_UNKNOWN"
			status = "unknown"
		}
		r = protocol.ActionResult{ActionID: r.ActionID, OperationType: p.operation, Status: status, Code: &code, Result: json.RawMessage("null")}
	}
	h.completeAction(context.Background(), r.ActionID, r.Status, r.Code, time.Now())
	p.result <- r
	h.metrics.Actions.Add(1)
}
func (h *Hub) AuditBlocked() bool { return h.auditBlocked.Load() }
func closePending(p *pending) {
	if p != nil && p.done != nil {
		p.doneOnce.Do(func() { close(p.done) })
	}
}
func (h *Hub) queueAuditRepair(key string, repair func(context.Context) error) {
	h.auditMu.Lock()
	h.auditRepairs[key] = repair
	h.auditMu.Unlock()
	h.auditBlocked.Store(true)
	h.metrics.AuditFailures.Add(1)
	h.log.Error("required audit write failed", "record", key)
}
func (h *Hub) completeAction(ctx context.Context, id, status string, code *string, at time.Time) {
	repair := func(ctx context.Context) error { return h.store.Complete(ctx, id, status, code, at) }
	if err := repair(ctx); err != nil && !errors.Is(err, store.ErrAlreadyComplete) {
		h.queueAuditRepair("complete:"+id, repair)
	}
}
func (h *Hub) receivedAction(ctx context.Context, id string, at time.Time) {
	repair := func(ctx context.Context) error { return h.store.Received(ctx, id, at) }
	if err := repair(ctx); err != nil {
		h.queueAuditRepair("received:"+id, repair)
	}
}
func (h *Hub) auditEvent(ctx context.Context, kind, host string, code *string) error {
	eventID, err := store.NewAuditEventID()
	if err != nil {
		return err
	}
	occurred := time.Now().UTC()
	key := "event:" + eventID
	repair := func(ctx context.Context) error { return h.store.AuditEventAt(ctx, eventID, kind, host, code, occurred) }
	if err = repair(ctx); err != nil {
		h.queueAuditRepair(key, repair)
		return err
	}
	return nil
}
func (h *Hub) RepairAudits(ctx context.Context) error {
	h.auditMu.Lock()
	defer h.auditMu.Unlock()
	for key, repair := range h.auditRepairs {
		if err := repair(ctx); err != nil {
			return err
		}
		delete(h.auditRepairs, key)
	}
	h.auditBlocked.Store(false)
	return nil
}
func (h *Hub) ConnectionMetadata(host string) (string, string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	l := h.leases[host]
	if l == nil {
		return "", "", false
	}
	return l.connectionID, l.version, true
}
func (h *Hub) notifyAsync(kind string, v any) { go h.notify(kind, v) }
func (h *Hub) sendProtocolError(ctx context.Context, c *websocket.Conn, code string) {
	body := map[string]any{"in_reply_to": nil, "code": code, "message": "connection rejected"}
	b, _ := protocol.MarshalEnvelope(0, "protocol.error", body)
	_ = c.Write(ctx, websocket.MessageText, b)
}

type codedError string

func (e codedError) Error() string { return string(e) }
func codeError(code string) error  { return codedError(code) }
func ErrorCode(err error) string {
	var c codedError
	if errors.As(err, &c) {
		return string(c)
	}
	return "INTERNAL"
}
func contains(v []string, w string) bool {
	for _, x := range v {
		if x == w {
			return true
		}
	}
	return false
}

var _ = fmt.Sprintf
