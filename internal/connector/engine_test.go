package connector

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dcolinmorgan/herdr-remote/internal/herdr"
	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
)

const testHost = "019f64ca-1000-7000-8000-000000000002"

type fakeLocal struct {
	mu       sync.Mutex
	version  string
	checked  bool
	revision uint64
	calls    []string
	sendErr  error
	reads    int
	onSend   func()
	status   string
	schema   *herdr.APISchema
	subs     [][]herdr.SubscriptionSpec
}

func (f *fakeLocal) Ping(context.Context) (herdr.Ping, error) {
	f.calls = append(f.calls, "ping")
	return herdr.Ping{Version: f.version, Protocol: 16, Capabilities: map[string]bool{"checked_input.v1": f.checked}}, nil
}
func (f *fakeLocal) Snapshot(context.Context) (herdr.Snapshot, error) {
	f.calls = append(f.calls, "snapshot")
	status := f.status
	if status == "" {
		status = "blocked"
	}
	return herdr.Snapshot{Snapshot: herdr.SnapshotData{Agents: []herdr.Agent{{TerminalID: "term", PaneID: "p1", WorkspaceID: "w", TabID: "t", Agent: "opencode", AgentStatus: status, Revision: f.revision, CWD: "/secret/project"}}}}, nil
}
func (f *fakeLocal) AgentGet(context.Context, string) (herdr.AgentInfo, error) {
	status := f.status
	if status == "" {
		status = "blocked"
	}
	return herdr.AgentInfo{Agent: herdr.Agent{TerminalID: "term", PaneID: "p1", Agent: "opencode", AgentStatus: status, Revision: f.revision}}, nil
}
func (f *fakeLocal) Read(context.Context, string, string, int) (herdr.PaneRead, error) {
	f.mu.Lock()
	f.reads++
	f.mu.Unlock()
	return herdr.PaneRead{Read: herdr.ReadData{Text: "output", Revision: f.revision, InputRevision: f.revision, ContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, nil
}
func (f *fakeLocal) ReadAgent(ctx context.Context, target, source string, lines int) (herdr.PaneRead, error) {
	return f.Read(ctx, target, source, lines)
}
func (f *fakeLocal) InspectSchema(context.Context) (herdr.APISchema, error) {
	f.calls = append(f.calls, "schema")
	if !f.checked {
		return herdr.APISchema{}, errors.New("unsupported")
	}
	if f.schema != nil {
		return *f.schema, nil
	}
	return fullFakeSchema(), nil
}
func fullFakeSchema() herdr.APISchema {
	return herdr.APISchema{Methods: []herdr.Method{{Name: "agent.read", Atomic: true, Parameters: []string{"target", "source", "lines", "format", "strip_ansi"}, ResultFields: []string{"text", "input_revision", "content_hash", "truncated"}, ParameterTypes: map[string]string{"target": "string", "source": "string", "lines": "integer", "format": "string", "strip_ansi": "boolean"}, ResultTypes: map[string]string{"text": "string", "input_revision": "integer", "content_hash": "string", "truncated": "boolean"}}, {Name: "agent.send_input_checked", Atomic: true, Parameters: []string{"terminal_id", "expected_input_revision", "expected_agent", "expected_status", "expected_content_hash", "text", "keys"}, ResultFields: []string{"enqueued", "input_revision"}, ParameterTypes: map[string]string{"terminal_id": "string", "expected_input_revision": "integer", "expected_agent": "string", "expected_status": "string", "expected_content_hash": "string", "text": "string", "keys": "array"}, ResultTypes: map[string]string{"enqueued": "boolean", "input_revision": "integer"}}}}
}
func (f *fakeLocal) SendChecked(context.Context, herdr.CheckedInput) (herdr.CheckedAck, error) {
	if f.onSend != nil {
		f.onSend()
	}
	return herdr.CheckedAck{Enqueued: f.sendErr == nil, InputRevision: f.revision + 1}, f.sendErr
}
func (f *fakeLocal) Subscribe(_ context.Context, specs []herdr.SubscriptionSpec) (*herdr.Subscription, error) {
	f.calls = append(f.calls, "subscribe")
	f.subs = append(f.subs, slices.Clone(specs))
	return &herdr.Subscription{}, nil
}

func action(s protocol.InstanceSnapshot, op protocol.Operation) protocol.ActionRequest {
	return protocol.ActionRequest{ActionID: mustID(), Target: protocol.Target{HostID: testHost, InstanceID: "default", TerminalID: "term"}, TimeoutMS: 3000, Expected: protocol.Expected{StateEpoch: s.EffectiveEpoch(), AgentGeneration: 1, HerdrInputRevision: s.Agents[0].HerdrInputRevision, Agent: "opencode", Statuses: []string{"blocked"}}, Operation: op}
}
func mustID() string { id, _ := protocol.NewUUIDv7(); return id }
func newEngine(t *testing.T, f *fakeLocal) (*Engine, protocol.InstanceSnapshot) {
	t.Helper()
	e, err := NewEngine(Config{HostID: testHost, InstanceID: "default"}, f)
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return e, s
}

func TestReconciliationOrderAndReadOnly073(t *testing.T) {
	f := &fakeLocal{version: "0.7.3"}
	e, s := newEngine(t, f)
	want := []string{"ping", "subscribe", "snapshot", "subscribe", "snapshot", "schema"}
	for i, v := range want {
		if f.calls[i] != v {
			t.Fatalf("call %d = %s", i, f.calls[i])
		}
	}
	if protocol.HasCapability(s.Capabilities, "checked_input.v1") {
		t.Fatal("0.7.3 advertised writes")
	}
	text := "yes"
	r := e.HandleAction(context.Background(), action(s, protocol.Operation{Type: "agent.send_text", Text: &text}))
	if r.Received || r.Result.Status != "rejected" || *r.Result.Code != "HERDR_INCOMPATIBLE" {
		t.Fatalf("unexpected result: %#v", r)
	}
}

func TestPatchedForkVersionAdvertisesCheckedInput(t *testing.T) {
	f := &fakeLocal{version: "0.7.3-checked.1", checked: true, revision: 42}
	_, s := newEngine(t, f)
	if !protocol.HasCapability(s.Capabilities, "checked_input.v1") {
		t.Fatal("patched fork version did not advertise checked input")
	}
	exact := &fakeLocal{version: "0.7.3", checked: true}
	_, s2 := newEngine(t, exact)
	if protocol.HasCapability(s2.Capabilities, "checked_input.v1") {
		t.Fatal("exact 0.7.3 advertised writes despite checked capability")
	}
}

func TestReconciliationUsesSupportedHerdrEvents(t *testing.T) {
	f := &fakeLocal{version: "0.7.3"}
	_, _ = newEngine(t, f)
	want := []herdr.SubscriptionSpec{{Type: "pane.created"}, {Type: "pane.closed"}, {Type: "pane.agent_detected"}, {Type: "pane.exited"}}
	if len(f.subs) != 2 || !slices.Equal(f.subs[0], want) {
		t.Fatalf("global subscriptions = %#v", f.subs)
	}
}

func TestCheckedInputStaleDuplicateAndUnknown(t *testing.T) {
	f := &fakeLocal{version: "0.8.0", checked: true, revision: 42}
	e, s := newEngine(t, f)
	text := "continue"
	a := action(s, protocol.Operation{Type: "agent.send_text", Text: &text})
	stale := a
	stale.ActionID = mustID()
	stale.Expected.HerdrInputRevision = 41
	if r := e.HandleAction(context.Background(), stale); *r.Result.Code != "STALE_STATE" {
		t.Fatalf("stale accepted: %#v", r)
	}
	staleEpoch := a
	staleEpoch.ActionID = mustID()
	staleEpoch.Expected.StateEpoch = mustID()
	if r := e.HandleAction(context.Background(), staleEpoch); *r.Result.Code != "STALE_STATE" {
		t.Fatalf("stale epoch accepted: %#v", r)
	}
	f.sendErr = errors.New("connection lost after call")
	r := e.HandleAction(context.Background(), a)
	if !r.Received || r.Result.Status != "unknown" || *r.Result.Code != "OUTCOME_UNKNOWN" {
		t.Fatalf("want unknown: %#v", r)
	}
	if dup := e.HandleAction(context.Background(), a); *dup.Result.Code != "DUPLICATE_ACTION" {
		t.Fatalf("duplicate accepted: %#v", dup)
	}
}
func TestReceiptPrecedesCheckedWrite(t *testing.T) {
	f := &fakeLocal{version: "0.8.0", checked: true, revision: 42}
	e, s := newEngine(t, f)
	text := "continue"
	received := false
	f.onSend = func() {
		if !received {
			t.Error("write started before receipt")
		}
	}
	r := e.HandleActionWithReceipt(context.Background(), action(s, protocol.Operation{Type: "agent.send_text", Text: &text}), func() bool { received = true; return true })
	if r.Result.Status != "succeeded" {
		t.Fatalf("write failed: %#v", r)
	}
}

func TestOutputSubscriptionLimit(t *testing.T) {
	f := &fakeLocal{version: "0.7.3"}
	e, _ := newEngine(t, f)
	e.mu.Lock()
	for i := 0; i < 5; i++ {
		terminal := string(rune('a' + i))
		e.byTerminal[terminal] = protocol.Agent{TerminalID: terminal, PaneID: "p1", Agent: "opencode", Status: "blocked", Generation: 1}
	}
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 4; i++ {
		id := mustID()
		err := e.StartOutput(ctx, protocol.OutputSubscribe{SubscriptionID: id, Target: protocol.Target{HostID: testHost, InstanceID: "default", TerminalID: string(rune('a' + i))}, Source: "recent", Lines: 1, PollIntervalMS: 500}, func(protocol.OutputSnapshot) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := e.StartOutput(ctx, protocol.OutputSubscribe{SubscriptionID: mustID(), Target: protocol.Target{HostID: testHost, InstanceID: "default", TerminalID: "e"}, Source: "recent", Lines: 1, PollIntervalMS: 500}, func(protocol.OutputSnapshot) error { return nil }); err == nil {
		t.Fatal("fifth output subscription accepted")
	}
	cancel()
	e.Stop()
}

func TestPromptPollBatchesGenerationChangesIntoOneSequence(t *testing.T) {
	f := &fakeLocal{version: "0.7.3"}
	e, _ := newEngine(t, f)
	e.mu.Lock()
	second := e.byTerminal["term"]
	second.TerminalID = "term2"
	e.byTerminal["term2"] = second
	e.state.Agents = append(e.state.Agents, second)
	e.mu.Unlock()
	prompts, delta, err := e.PollPrompts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d", len(prompts))
	}
	if delta == nil || delta.Sequence != 1 {
		t.Fatalf("one emitted delta must advance once: %#v", delta)
	}
	for _, p := range prompts {
		if p.StateSequence != 1 {
			t.Fatalf("prompt sequence = %d", p.StateSequence)
		}
	}
}

func TestPromptRespondRequiresCurrentBlockedStatus(t *testing.T) {
	f := &fakeLocal{version: "0.8.0", checked: true, revision: 42, status: "working"}
	e, s := newEngine(t, f)
	hash := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a := action(s, protocol.Operation{Type: "prompt.respond", OptionID: "allow_once"})
	a.Expected.Statuses = []string{"working"}
	a.Expected.PromptFingerprint = hash
	a.Expected.HerdrContentHash = hash
	r := e.HandleAction(context.Background(), a)
	if r.Received || r.Result.Status != "rejected" || *r.Result.Code != "STALE_STATE" {
		t.Fatalf("non-blocked prompt response was not rejected: %#v", r)
	}
}
func TestActionDeliveryFailurePreventsWriteAndPropagates(t *testing.T) {
	f := &fakeLocal{version: "0.8.0", checked: true, revision: 42}
	e, s := newEngine(t, f)
	writes := 0
	f.onSend = func() { writes++ }
	q := NewQueue(1)
	if err := q.Put(context.Background(), []byte("occupied")); err != nil {
		t.Fatal(err)
	}
	text := "continue"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := (&Daemon{}).handleAction(ctx, q, e, action(s, protocol.Operation{Type: "agent.send_text", Text: &text}))
	if err == nil {
		t.Fatal("enqueue failure was hidden")
	}
	if writes != 0 {
		t.Fatalf("write executed despite missing receipt delivery: %d", writes)
	}
}
func TestResultDeliveryFailureAfterWritePropagates(t *testing.T) {
	f := &fakeLocal{version: "0.8.0", checked: true, revision: 42}
	e, s := newEngine(t, f)
	writes := 0
	f.onSend = func() { writes++ }
	q := NewQueue(1)
	text := "continue"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	err := (&Daemon{}).handleAction(ctx, q, e, action(s, protocol.Operation{Type: "agent.send_text", Text: &text}))
	if err == nil {
		t.Fatal("result enqueue failure was hidden")
	}
	if writes != 1 {
		t.Fatalf("checked write executions = %d", writes)
	}
	frame, readErr := q.Next(context.Background())
	if readErr != nil {
		t.Fatal(readErr)
	}
	env, _, decodeErr := protocol.DecodeStrict(frame, "connector")
	if decodeErr != nil || env.Type != "action.received" {
		t.Fatalf("queued priority frame = %s, %v", env.Type, decodeErr)
	}
}
func TestOutputRevisionMismatchReconcilesAndReadsAgain(t *testing.T) {
	f := &fakeLocal{version: "0.8.0", checked: true, revision: 42}
	e, first := newEngine(t, f)
	published := 0
	e.SetStatePublisher(func(protocol.InstanceSnapshot) error {
		t.Fatal("revision-only reconciliation published a full snapshot")
		return nil
	})
	e.SetDeltaPublisher(func(delta protocol.StateDelta) error {
		published++
		if delta.Epoch != first.EffectiveEpoch() || delta.Sequence != 1 || len(delta.Changes) != 1 || delta.Changes[0].Agent.HerdrInputRevision != 43 || delta.Changes[0].Agent.EffectiveGeneration() != 2 {
			t.Errorf("reconciled delta = %#v", delta)
		}
		return nil
	})
	f.revision = 43
	var output protocol.OutputSnapshot
	last := ""
	err := e.pollOutput(context.Background(), protocol.OutputSubscribe{SubscriptionID: mustID(), Target: protocol.Target{HostID: testHost, InstanceID: "default", TerminalID: "term"}, Source: "recent", Lines: 1, PollIntervalMS: 500}, &last, func(value protocol.OutputSnapshot) error { output = value; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || output.HerdrInputRevision != 43 {
		t.Fatalf("published=%d output=%#v", published, output)
	}
}
func TestUnchangedPeriodicReconciliationPreservesEpoch(t *testing.T) {
	f := &fakeLocal{version: "0.7.3"}
	e, first := newEngine(t, f)
	second, err := e.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.EffectiveEpoch() != second.EffectiveEpoch() {
		t.Fatalf("unchanged reconciliation rotated epoch: %s -> %s", first.EffectiveEpoch(), second.EffectiveEpoch())
	}
}
func TestCheckedRevisionReconciliationPreservesEpoch(t *testing.T) {
	f := &fakeLocal{version: "0.7.3-checked.1", checked: true, revision: 42}
	e, first := newEngine(t, f)
	f.revision = 43
	second, err := e.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.EffectiveEpoch() != second.EffectiveEpoch() {
		t.Fatalf("revision-only reconciliation rotated epoch: %s -> %s", first.EffectiveEpoch(), second.EffectiveEpoch())
	}
	if second.Sequence != 1 || second.Agents[0].HerdrInputRevision != 43 || second.Agents[0].EffectiveGeneration() != 2 {
		t.Fatalf("revision-only reconciliation state = %#v", second)
	}
}
func TestConcurrentReconciliationPublishesSequencesInOrder(t *testing.T) {
	f := &fakeLocal{version: "0.7.3-checked.1", checked: true, revision: 42}
	e, _ := newEngine(t, f)
	firstPublishing := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var sequences []uint64
	e.SetStatePublisher(func(protocol.InstanceSnapshot) error { return nil })
	e.SetDeltaPublisher(func(delta protocol.StateDelta) error {
		mu.Lock()
		sequences = append(sequences, delta.Sequence)
		mu.Unlock()
		if delta.Sequence == 1 {
			close(firstPublishing)
			<-releaseFirst
		}
		return nil
	})
	f.revision = 43
	firstDone := make(chan error, 1)
	go func() {
		_, err := e.reconcileAndPublish(context.Background(), false)
		firstDone <- err
	}()
	<-firstPublishing
	f.revision = 44
	secondDone := make(chan error, 1)
	go func() {
		_, err := e.reconcileAndPublish(context.Background(), false)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second delta published before first completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sequences, []uint64{1, 2}) {
		t.Fatalf("published sequences = %v", sequences)
	}
}
func TestDaemonSupportsMultipleConfiguredInstances(t *testing.T) {
	first, err := NewEngine(Config{HostID: testHost, InstanceID: "default"}, &fakeLocal{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewEngine(Config{HostID: testHost, InstanceID: "work"}, &fakeLocal{})
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := NewMultiDaemon(DaemonConfig{URL: "wss://example.test/ws", HostID: testHost, CertFile: "cert", KeyFile: "key", CAFile: "ca"}, map[string]*Engine{"default": first, "work": second})
	if err != nil {
		t.Fatal(err)
	}
	if len(daemon.engines) != 2 || daemon.engines["work"] != second {
		t.Fatalf("instances = %#v", daemon.engines)
	}
}
func TestNewConnectorSendsInventoryOnlyWhenServerAcceptsCapability(t *testing.T) {
	first, err := NewEngine(Config{HostID: testHost, InstanceID: "default"}, &fakeLocal{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewEngine(Config{HostID: testHost, InstanceID: "work"}, &fakeLocal{})
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := NewMultiDaemon(DaemonConfig{URL: "wss://example.test/ws", HostID: testHost, CertFile: "cert", KeyFile: "key", CAFile: "ca"}, map[string]*Engine{"work": second, "default": first})
	if err != nil {
		t.Fatal(err)
	}
	if _, send := daemon.negotiatedInventory([]string{"output.subscribe.v1"}); send {
		t.Fatal("new connector would send inventory to old server")
	}
	inventory, send := daemon.negotiatedInventory([]string{"output.subscribe.v1", protocol.StateInventoryCapability})
	if !send || !slices.Equal(inventory.InstanceIDs, []string{"default", "work"}) {
		t.Fatalf("negotiated inventory = %#v, send=%t", inventory, send)
	}
}
func TestPartialCheckedSchemaNeverAdvertisesWritesOrPromptResponse(t *testing.T) {
	partial := herdr.APISchema{Methods: []herdr.Method{{Name: "agent.send_input_checked", Atomic: true, Parameters: []string{"terminal_id", "expected_input_revision", "expected_agent", "expected_status", "expected_content_hash"}, ResultFields: []string{"enqueued"}}}}
	f := &fakeLocal{version: "0.8.0", checked: true, revision: 42, schema: &partial}
	_, snapshot := newEngine(t, f)
	if protocol.HasCapability(snapshot.Capabilities, "checked_input.v1") || protocol.HasCapability(snapshot.Capabilities, "prompt.respond.v1") {
		t.Fatalf("partial schema capabilities = %v", snapshot.Capabilities)
	}
	if snapshot.Agents[0].HerdrInputRevision != 0 {
		t.Fatalf("partial schema exposed revision %d", snapshot.Agents[0].HerdrInputRevision)
	}
}
func TestQueueLimitAndOutputCoalescing(t *testing.T) {
	q := NewQueue(1)
	if err := q.Put(context.Background(), []byte("first")); err != nil {
		t.Fatal(err)
	}
	ctx, c := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer c()
	if err := q.Put(ctx, []byte("second")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want queue full, got %v", err)
	}
	q.ReplaceOutput("s", []byte("old"))
	q.ReplaceOutput("s", []byte("new"))
	b, _ := q.Next(context.Background())
	if string(b) != "first" {
		t.Fatal("priority lost")
	}
	b, _ = q.Next(context.Background())
	if string(b) != "new" {
		t.Fatal("output was not coalesced")
	}
}
