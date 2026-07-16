package protocol

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckedFixturesDecodeStrictly(t *testing.T) {
	for _, tc := range []struct{ name, file, side string }{{"connector", "connector_protocol_v1.ndjson", "connector"}, {"browser", "browser_protocol_v1.ndjson", "browser"}} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("..", "..", "tests", "fixtures", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			scan := bufio.NewScanner(f)
			scan.Buffer(make([]byte, 1024), MaxFrameBytes)
			line := 0
			for scan.Scan() {
				line++
				if _, _, err := DecodeStrict(scan.Bytes(), tc.side); err != nil {
					t.Fatalf("line %d: %v", line, err)
				}
			}
			if err := scan.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInventoryUsesNegotiatedApplicationMessage(t *testing.T) {
	hello := Hello{MinProtocol: 1, MaxProtocol: 1, ConnectorVersion: "0.1.0", ConnectorInstanceID: "019f64ca-3000-7000-8000-000000000101", DisplayName: "host", Platform: "linux", Architecture: "amd64", Capabilities: []string{StateInventoryCapability}}
	frame, err := MarshalEnvelope(0, "connector.hello", hello)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeStrict(frame, "connector"); err != nil {
		t.Fatalf("additive capability rejected in fixed hello: %v", err)
	}
	if string(frame) == "" || json.Valid(frame) == false {
		t.Fatal("invalid hello frame")
	}
	var envelope struct {
		Body map[string]json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, exists := envelope.Body["instance_ids"]; exists {
		t.Fatal("fixed protocol-0 hello contains instance inventory")
	}
	frame, err = MarshalEnvelope(1, "state.inventory", InstanceInventory{InstanceIDs: []string{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeStrict(frame, "connector"); err != nil {
		t.Fatalf("valid inventory rejected: %v", err)
	}
	frame, err = MarshalEnvelope(1, "state.inventory", InstanceInventory{InstanceIDs: []string{"default", "default"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeStrict(frame, "connector"); err == nil {
		t.Fatal("duplicate inventory accepted")
	}
}

func TestActionValidationAndResultMatrix(t *testing.T) {
	id := "019f64ca-3000-7000-8000-000000000105"
	epoch := "019f64ca-3000-7000-8000-000000000103"
	lines := 1
	a := ActionRequest{ActionID: id, Target: Target{HostID: "019f64ca-1000-7000-8000-000000000002", InstanceID: "default", TerminalID: "term"}, TimeoutMS: 5000, Expected: Expected{StateEpoch: epoch, AgentGeneration: 1, Agent: "opencode", Statuses: []string{"blocked"}}, Operation: Operation{Type: "agent.read", Source: "recent", Lines: &lines}}
	if err := ValidateAction(a, false); err != nil {
		t.Fatal(err)
	}
	a.ActionID = "not-v7"
	if ValidateAction(a, false) == nil {
		t.Fatal("accepted non-v7 action")
	}
	code := "OUTCOME_UNKNOWN"
	r := ActionResult{ActionID: id, OperationType: "agent.read", Status: "unknown", Code: &code, Result: json.RawMessage("null")}
	if ValidateResult(r, false) == nil {
		t.Fatal("read cannot have unknown outcome")
	}
	r.OperationType = "agent.send_text"
	if err := ValidateResult(r, false); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownFieldsAndBrowserMessageRejected(t *testing.T) {
	frame := []byte(`{"protocol":1,"message_id":"019f64ca-3000-7000-8000-000000000001","type":"output.unsubscribe","sent_at":"2026-07-15T08:00:00Z","body":{"session_id":"019f64ca-3000-7000-8000-000000000101","subscription_id":"019f64ca-3000-7000-8000-000000000104","unexpected":true}}`)
	if _, _, err := DecodeStrict(frame, "browser"); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestSessionLogicalUniquenessAndHerdr073(t *testing.T) {
	epoch := "019f64ca-3000-7000-8000-000000000110"
	agent := Agent{TerminalID: "term", Agent: "opencode", Status: "blocked", AgentGeneration: 1, ConnectorEpoch: epoch}
	inst := BrowserInstance{InstanceID: "default", ConnectorEpoch: epoch, HerdrVersion: "0.7.3", HerdrProtocol: 16, Status: "online", Capabilities: []string{"read.v1"}, Agents: []Agent{agent, agent}}
	snap := SessionSnapshot{SessionID: "019f64ca-3000-7000-8000-000000000101", StateEpoch: "019f64ca-3000-7000-8000-000000000103", Hosts: []HostSnapshot{{HostID: "019f64ca-1000-7000-8000-000000000002", DisplayName: "host", Status: "connected", Instances: []BrowserInstance{inst}}}}
	if ValidateSessionSnapshot(snap) == nil {
		t.Fatal("duplicate terminal accepted")
	}
	inst.Agents = []Agent{agent}
	inst.Capabilities = []string{"read.v1", "checked_input.v1"}
	snap.Hosts[0].Instances = []BrowserInstance{inst}
	if ValidateSessionSnapshot(snap) == nil {
		t.Fatal("0.7.3 checked input accepted")
	}
	inst.HerdrVersion = "0.7.3-checked.1"
	snap.Hosts[0].Instances = []BrowserInstance{inst}
	snap.ServerTime = "2026-07-15T08:00:00Z"
	if err := ValidateSessionSnapshot(snap); err != nil {
		t.Fatalf("patched fork version rejected: %v", err)
	}
}
func TestConnectorDeltaRejectsDuplicateAndMalformedChanges(t *testing.T) {
	epoch := "019f64ca-3000-7000-8000-000000000110"
	agent := Agent{TerminalID: "term", PaneID: "p", WorkspaceID: "w", TabID: "t", Agent: "opencode", Status: "blocked", Generation: 2}
	delta := StateDelta{InstanceID: "default", Epoch: epoch, Sequence: 1, Changes: []StateChange{{Operation: "upsert", Agent: &agent}, {Operation: "remove", TerminalID: "term", Reason: "pane_closed"}}}
	if ValidateConnectorDelta(delta) == nil {
		t.Fatal("duplicate terminal mutations accepted")
	}
	delta.Changes = []StateChange{{Operation: "upsert", Agent: &agent, Reason: "reconciled"}}
	if ValidateConnectorDelta(delta) == nil {
		t.Fatal("upsert with removal fields accepted")
	}
}
func TestMessageDirectionsAndMalformedThreshold(t *testing.T) {
	if ValidateDirection(BrowserToControl, "session.snapshot") == nil {
		t.Fatal("browser accepted server-only message")
	}
	if err := ValidateDirection(BrowserToControl, "action.request"); err != nil {
		t.Fatal(err)
	}
	var tracker MalformedTracker
	now := time.Now()
	if tracker.Add(now) || tracker.Add(now.Add(time.Second)) {
		t.Fatal("closed before malformed threshold")
	}
	if !tracker.Add(now.Add(2 * time.Second)) {
		t.Fatal("did not close at malformed threshold")
	}
}
func TestProtocolNegotiationUsesHighestMutualVersion(t *testing.T) {
	if selected, ok := NegotiateProtocol(1, 3, 1, 2); !ok || selected != 2 {
		t.Fatalf("selection = %d, %v", selected, ok)
	}
	if _, ok := NegotiateProtocol(2, 3, 1, 1); ok {
		t.Fatal("non-overlapping ranges negotiated")
	}
}

type fixtureOutcome struct {
	Status string          `json:"status"`
	Code   *string         `json:"code"`
	Result json.RawMessage `json:"result"`
}

func TestCheckedOperationOutcomeFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "connector_protocol_v1_operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Type       string         `json:"type"`
		Success    fixtureOutcome `json:"success"`
		Pre        fixtureOutcome `json:"pre_execution_failure"`
		Before     fixtureOutcome `json:"timeout_before_execution"`
		After      fixtureOutcome `json:"timeout_after_execution"`
		Disconnect fixtureOutcome `json:"disconnect"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		for name, outcome := range map[string]fixtureOutcome{"success": tc.Success, "pre": tc.Pre, "before": tc.Before, "after": tc.After, "disconnect": tc.Disconnect} {
			if len(outcome.Result) == 0 {
				outcome.Result = json.RawMessage("null")
			}
			result := ActionResult{ActionID: "019f64ca-3000-7000-8000-000000000105", OperationType: tc.Type, Status: outcome.Status, Code: outcome.Code, Result: outcome.Result}
			if err := ValidateResult(result, false); err != nil {
				t.Errorf("%s/%s: %v", tc.Type, name, err)
			}
		}
	}
}
