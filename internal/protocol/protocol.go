// Package protocol contains the strict, shared enterprise v1 wire types.
//
// The checked-in JSON schemas are normative. These validators additionally
// enforce cross-field and stateful rules that JSON Schema cannot express.
package protocol

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Version                  = 1
	StateInventoryCapability = "state.inventory.v1"
	MaxFrameBytes            = 256 * 1024
	MaxAgents                = 256
	MaxInstances             = 16
	MaxOutputRunes           = 32768
	MaxPromptRunes           = 8192
	MaxBrowserPromptRunes    = 2048
)

var (
	uuidV7RE         = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	uuidRE           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	instanceRE       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)
	optionRE         = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)
	hashRE           = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	versionRE        = regexp.MustCompile(`^[A-Za-z0-9.+_-]{1,32}$`)
	capabilityNameRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

// NewUUIDv7 returns a lowercase RFC 9562 UUIDv7 using crypto/rand.
func NewUUIDv7() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | (b[6] & 0x0f)
	b[8] = 0x80 | (b[8] & 0x3f)
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:]), nil
}

func IsUUIDv7(s string) bool { return uuidV7RE.MatchString(s) }
func IsUUID(s string) bool   { return uuidRE.MatchString(strings.ToLower(s)) && s == strings.ToLower(s) }

type Envelope struct {
	Protocol  int             `json:"protocol"`
	MessageID string          `json:"message_id"`
	Type      string          `json:"type"`
	SentAt    string          `json:"sent_at"`
	Body      json.RawMessage `json:"body"`
}
type Direction string

const (
	BrowserToControl   Direction = "browser-to-control"
	ControlToBrowser   Direction = "control-to-browser"
	ConnectorToControl Direction = "connector-to-control"
	ControlToConnector Direction = "control-to-connector"
)

func ValidateDirection(direction Direction, messageType string) error {
	allowed := map[Direction]map[string]bool{BrowserToControl: {"state.resync": true, "output.subscribe": true, "output.unsubscribe": true, "action.request": true}, ControlToBrowser: {"session.snapshot": true, "state.delta": true, "prompt.snapshot": true, "output.snapshot": true, "action.received": true, "action.result": true, "protocol.error": true}, ConnectorToControl: {"connector.hello": true, "state.inventory": true, "state.snapshot": true, "state.delta": true, "prompt.snapshot": true, "output.snapshot": true, "action.received": true, "action.result": true, "protocol.error": true}, ControlToConnector: {"server.welcome": true, "state.resync": true, "output.subscribe": true, "output.unsubscribe": true, "action.request": true, "protocol.error": true}}
	if !allowed[direction][messageType] {
		return errors.New("message type not allowed in this direction")
	}
	return nil
}
func NegotiateProtocol(peerMin, peerMax, serverMin, serverMax int) (int, bool) {
	low := max(peerMin, serverMin)
	high := min(peerMax, serverMax)
	if low > high {
		return 0, false
	}
	return high, true
}

type MalformedTracker struct{ Times []time.Time }

func (m *MalformedTracker) Add(now time.Time) bool {
	cut := now.Add(-time.Minute)
	kept := m.Times[:0]
	for _, at := range m.Times {
		if at.After(cut) {
			kept = append(kept, at)
		}
	}
	m.Times = append(kept, now)
	return len(m.Times) >= 3
}

type Target struct {
	HostID     string `json:"host_id"`
	InstanceID string `json:"instance_id"`
	TerminalID string `json:"terminal_id"`
}

type Expected struct {
	StateEpoch         string   `json:"state_epoch"`
	ConnectorEpoch     string   `json:"connector_epoch,omitempty"`
	AgentGeneration    uint64   `json:"agent_generation"`
	HerdrInputRevision uint64   `json:"herdr_input_revision"`
	Agent              string   `json:"agent"`
	Statuses           []string `json:"statuses"`
	PromptFingerprint  string   `json:"prompt_fingerprint,omitempty"`
	HerdrContentHash   string   `json:"herdr_content_hash,omitempty"`
}

type Operation struct {
	Type     string   `json:"type"`
	Source   string   `json:"source,omitempty"`
	Lines    *int     `json:"lines,omitempty"`
	Text     *string  `json:"text,omitempty"`
	Keys     []string `json:"keys,omitempty"`
	OptionID string   `json:"option_id,omitempty"`
}

type ActionRequest struct {
	ActionID  string    `json:"action_id"`
	Target    Target    `json:"target"`
	TimeoutMS int       `json:"timeout_ms"`
	Expected  Expected  `json:"expected"`
	Operation Operation `json:"operation"`
}

type BrowserActionRequest struct {
	SessionID string `json:"session_id"`
	ActionRequest
}

type ActionReceived struct {
	SessionID string `json:"session_id,omitempty"`
	ActionID  string `json:"action_id"`
}

type ActionResult struct {
	SessionID     string          `json:"session_id,omitempty"`
	ActionID      string          `json:"action_id"`
	OperationType string          `json:"operation_type"`
	Status        string          `json:"status"`
	Code          *string         `json:"code"`
	Message       json.RawMessage `json:"message,omitempty"`
	Result        json.RawMessage `json:"result"`
}

type ReadResult struct {
	StateEpoch         string `json:"state_epoch"`
	ConnectorEpoch     string `json:"connector_epoch,omitempty"`
	AgentGeneration    uint64 `json:"agent_generation"`
	HerdrInputRevision uint64 `json:"herdr_input_revision"`
	Text               string `json:"text"`
	Truncated          bool   `json:"truncated"`
	ContentRevision    string `json:"content_revision"`
}

type WriteResult struct {
	HerdrAcknowledged bool   `json:"herdr_acknowledged"`
	OptionID          string `json:"option_id,omitempty"`
}

type Hello struct {
	MinProtocol         int      `json:"min_protocol"`
	MaxProtocol         int      `json:"max_protocol"`
	ConnectorVersion    string   `json:"connector_version"`
	ConnectorInstanceID string   `json:"connector_instance_id"`
	DisplayName         string   `json:"display_name"`
	Platform            string   `json:"platform"`
	Architecture        string   `json:"architecture"`
	Capabilities        []string `json:"capabilities"`
}

type Welcome struct {
	SelectedProtocol     int      `json:"selected_protocol"`
	ServerMinProtocol    int      `json:"server_min_protocol"`
	ServerMaxProtocol    int      `json:"server_max_protocol"`
	AcceptedCapabilities []string `json:"accepted_capabilities"`
	ConnectionID         string   `json:"connection_id"`
	HostID               string   `json:"host_id"`
	HeartbeatIntervalMS  int      `json:"heartbeat_interval_ms"`
	MaxMessageBytes      int      `json:"max_message_bytes"`
	ServerTime           string   `json:"server_time"`
}

type Agent struct {
	TerminalID         string  `json:"terminal_id,omitempty"`
	PaneID             string  `json:"pane_id,omitempty"`
	WorkspaceID        string  `json:"workspace_id,omitempty"`
	TabID              string  `json:"tab_id,omitempty"`
	Agent              string  `json:"agent"`
	DisplayName        *string `json:"display_name"`
	Status             string  `json:"status"`
	Project            *string `json:"project"`
	Generation         uint64  `json:"generation,omitempty"`
	AgentGeneration    uint64  `json:"agent_generation,omitempty"`
	HerdrInputRevision uint64  `json:"herdr_input_revision"`
	ConnectorEpoch     string  `json:"connector_epoch,omitempty"`
}

func (a Agent) EffectiveGeneration() uint64 {
	if a.Generation != 0 {
		return a.Generation
	}
	return a.AgentGeneration
}

type InstanceSnapshot struct {
	InstanceID     string   `json:"instance_id"`
	Epoch          string   `json:"epoch,omitempty"`
	ConnectorEpoch string   `json:"connector_epoch,omitempty"`
	Sequence       uint64   `json:"sequence"`
	HerdrVersion   string   `json:"herdr_version"`
	HerdrProtocol  int      `json:"herdr_protocol"`
	Status         string   `json:"status"`
	Capabilities   []string `json:"capabilities"`
	Agents         []Agent  `json:"agents"`
}

type InstanceInventory struct {
	InstanceIDs []string `json:"instance_ids"`
}

func (i InstanceSnapshot) EffectiveEpoch() string {
	if i.Epoch != "" {
		return i.Epoch
	}
	return i.ConnectorEpoch
}

type HostSnapshot struct {
	HostID      string            `json:"host_id"`
	DisplayName string            `json:"display_name"`
	Status      string            `json:"status"`
	Instances   []BrowserInstance `json:"instances"`
}

// BrowserInstance is the browser-safe projection of a connector snapshot.
// Connector ordering is consumed by the control plane and never crosses the
// browser protocol boundary.
type BrowserInstance struct {
	InstanceID     string   `json:"instance_id"`
	ConnectorEpoch string   `json:"connector_epoch"`
	HerdrVersion   string   `json:"herdr_version"`
	HerdrProtocol  int      `json:"herdr_protocol"`
	Status         string   `json:"status"`
	Capabilities   []string `json:"capabilities"`
	Agents         []Agent  `json:"agents"`
}

func (i BrowserInstance) EffectiveEpoch() string {
	return i.ConnectorEpoch
}

type SessionSnapshot struct {
	SessionID  string         `json:"session_id"`
	StateEpoch string         `json:"state_epoch"`
	Sequence   uint64         `json:"sequence"`
	ServerTime string         `json:"server_time"`
	Hosts      []HostSnapshot `json:"hosts"`
}

type StateChange struct {
	Operation              string         `json:"operation"`
	Agent                  *Agent         `json:"agent,omitempty"`
	TerminalID             string         `json:"terminal_id,omitempty"`
	Target                 *Target        `json:"target,omitempty"`
	Reason                 string         `json:"reason,omitempty"`
	HostID                 string         `json:"host_id,omitempty"`
	InstanceID             string         `json:"instance_id,omitempty"`
	PreviousConnectorEpoch string         `json:"previous_connector_epoch,omitempty"`
	ConnectorEpoch         string         `json:"connector_epoch,omitempty"`
	Host                   *HostState     `json:"host,omitempty"`
	Instance               *InstanceState `json:"instance,omitempty"`
}
type HostState struct {
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}
type InstanceState struct {
	ConnectorEpoch string   `json:"connector_epoch"`
	HerdrVersion   string   `json:"herdr_version"`
	HerdrProtocol  int      `json:"herdr_protocol"`
	Status         string   `json:"status"`
	Capabilities   []string `json:"capabilities"`
}

type StateDelta struct {
	SessionID  string        `json:"session_id,omitempty"`
	StateEpoch string        `json:"state_epoch,omitempty"`
	InstanceID string        `json:"instance_id,omitempty"`
	Epoch      string        `json:"epoch,omitempty"`
	Sequence   uint64        `json:"sequence"`
	Changes    []StateChange `json:"changes"`
}

type PromptOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type PromptSnapshot struct {
	SessionID          string         `json:"session_id,omitempty"`
	Target             Target         `json:"target"`
	StateEpoch         string         `json:"state_epoch"`
	StateSequence      uint64         `json:"state_sequence"`
	ConnectorEpoch     string         `json:"connector_epoch,omitempty"`
	AgentGeneration    uint64         `json:"agent_generation"`
	HerdrInputRevision uint64         `json:"herdr_input_revision,omitempty"`
	HerdrContentHash   string         `json:"herdr_content_hash"`
	Fingerprint        string         `json:"fingerprint"`
	Excerpt            string         `json:"excerpt"`
	ExcerptTruncated   bool           `json:"excerpt_truncated"`
	AdapterVersion     string         `json:"adapter_version"`
	Options            []PromptOption `json:"options"`
}

type OutputSubscribe struct {
	SessionID      string `json:"session_id,omitempty"`
	SubscriptionID string `json:"subscription_id"`
	Target         Target `json:"target"`
	Source         string `json:"source"`
	Lines          int    `json:"lines"`
	PollIntervalMS int    `json:"poll_interval_ms"`
}
type OutputUnsubscribe struct {
	SessionID      string `json:"session_id,omitempty"`
	SubscriptionID string `json:"subscription_id"`
}
type StateResync struct {
	SessionID        string  `json:"session_id"`
	ExpectedEpoch    *string `json:"expected_epoch"`
	ExpectedSequence *uint64 `json:"expected_sequence"`
	Reason           string  `json:"reason"`
}
type ConnectorStateResync struct {
	InstanceID       string  `json:"instance_id"`
	ExpectedEpoch    *string `json:"expected_epoch"`
	ExpectedSequence *uint64 `json:"expected_sequence"`
	Reason           string  `json:"reason"`
}
type ProtocolError struct {
	SessionID string  `json:"session_id,omitempty"`
	InReplyTo *string `json:"in_reply_to"`
	Code      string  `json:"code"`
	Fatal     *bool   `json:"fatal,omitempty"`
	Message   string  `json:"message,omitempty"`
}
type OutputSnapshot struct {
	SessionID          string `json:"session_id,omitempty"`
	SubscriptionID     string `json:"subscription_id"`
	Target             Target `json:"target"`
	StateEpoch         string `json:"state_epoch"`
	ConnectorEpoch     string `json:"connector_epoch,omitempty"`
	AgentGeneration    uint64 `json:"agent_generation"`
	HerdrInputRevision uint64 `json:"herdr_input_revision"`
	ContentRevision    string `json:"content_revision"`
	Text               string `json:"text"`
	Truncated          bool   `json:"truncated"`
}

var capabilities = map[string]bool{
	"read.v1": true, "output.subscribe.v1": true, "prompt.snapshot.v1": true,
	"checked_input.v1": true, "prompt.respond.v1": true,
}
var statuses = map[string]bool{"idle": true, "working": true, "blocked": true, "done": true, "unknown": true}
var sources = map[string]bool{"visible": true, "recent": true, "recent_unwrapped": true, "detection": true}
var keys = map[string]bool{"enter": true, "esc": true, "tab": true, "shift+tab": true, "up": true, "down": true, "left": true, "right": true, "pageup": true, "pagedown": true, "home": true, "end": true, "backspace": true, "delete": true, "ctrl+c": true}
var operations = map[string]bool{"agent.read": true, "agent.send_text": true, "agent.send_keys": true, "agent.send_input": true, "agent.interrupt": true, "prompt.respond": true}

func IsWrite(op string) bool { return op != "agent.read" }

func ValidateTarget(t Target, browser bool) error {
	if (browser && !IsUUIDv7(t.HostID)) || (!browser && !IsUUID(t.HostID)) {
		return errors.New("invalid host_id")
	}
	if !instanceRE.MatchString(t.InstanceID) {
		return errors.New("invalid instance_id")
	}
	if err := boundedUntrusted(t.TerminalID, 1, 128, true); err != nil {
		return fmt.Errorf("terminal_id: %w", err)
	}
	return nil
}

func ValidateCapabilities(c []string) error {
	if len(c) > 5 {
		return errors.New("too many capabilities")
	}
	seen := map[string]bool{}
	for _, v := range c {
		if !capabilities[v] || seen[v] {
			return errors.New("invalid or duplicate capability")
		}
		seen[v] = true
	}
	if seen["prompt.respond.v1"] && !seen["checked_input.v1"] {
		return errors.New("prompt response requires checked input")
	}
	return nil
}

func HasCapability(c []string, want string) bool { return slices.Contains(c, want) }

func ValidateAction(a ActionRequest, browser bool) error {
	if !IsUUIDv7(a.ActionID) {
		return errors.New("action_id must be UUIDv7")
	}
	if err := ValidateTarget(a.Target, browser); err != nil {
		return err
	}
	if !IsUUIDv7(a.Expected.StateEpoch) || a.Expected.AgentGeneration < 1 {
		return errors.New("invalid expected state")
	}
	if browser && !IsUUIDv7(a.Expected.ConnectorEpoch) {
		return errors.New("invalid connector epoch")
	}
	if err := boundedUntrusted(a.Expected.Agent, 1, 80, true); err != nil {
		return errors.New("invalid agent")
	}
	if len(a.Expected.Statuses) < 1 || len(a.Expected.Statuses) > 5 {
		return errors.New("invalid statuses")
	}
	seen := map[string]bool{}
	for _, s := range a.Expected.Statuses {
		if !statuses[s] || seen[s] {
			return errors.New("invalid statuses")
		}
		seen[s] = true
	}
	if !operations[a.Operation.Type] {
		return errors.New("unsupported operation")
	}
	maxTimeout := 3000
	if a.Operation.Type == "agent.read" {
		maxTimeout = 5000
	}
	if a.TimeoutMS < 1 || a.TimeoutMS > maxTimeout {
		return errors.New("invalid timeout")
	}
	if err := ValidateOperation(a.Operation); err != nil {
		return err
	}
	if a.Operation.Type == "prompt.respond" {
		if !hashRE.MatchString(a.Expected.PromptFingerprint) || !hashRE.MatchString(a.Expected.HerdrContentHash) {
			return errors.New("prompt hashes required")
		}
	} else if a.Expected.PromptFingerprint != "" || a.Expected.HerdrContentHash != "" {
		return errors.New("prompt hashes forbidden")
	}
	return nil
}

func ValidateOperation(o Operation) error {
	switch o.Type {
	case "agent.read":
		if !sources[o.Source] || o.Lines == nil || *o.Lines < 1 || *o.Lines > 1000 || o.Text != nil || len(o.Keys) != 0 || o.OptionID != "" {
			return errors.New("invalid read operation")
		}
	case "agent.send_text":
		if o.Text == nil || o.Source != "" || o.Lines != nil || len(o.Keys) != 0 || o.OptionID != "" {
			return errors.New("invalid send_text operation")
		}
		if err := validInputText(*o.Text); err != nil {
			return err
		}
	case "agent.send_keys":
		if o.Text != nil || o.Source != "" || o.Lines != nil || o.OptionID != "" {
			return errors.New("invalid send_keys operation")
		}
		if err := validKeys(o.Keys); err != nil {
			return err
		}
	case "agent.send_input":
		if o.Source != "" || o.Lines != nil || o.OptionID != "" || (o.Text == nil && len(o.Keys) == 0) {
			return errors.New("invalid send_input operation")
		}
		if o.Text != nil {
			if err := validInputText(*o.Text); err != nil {
				return err
			}
		}
		if len(o.Keys) != 0 {
			if err := validKeys(o.Keys); err != nil {
				return err
			}
		}
	case "agent.interrupt":
		if o.Source != "" || o.Lines != nil || o.Text != nil || len(o.Keys) != 0 || o.OptionID != "" {
			return errors.New("invalid interrupt operation")
		}
	case "prompt.respond":
		if !optionRE.MatchString(o.OptionID) || o.Source != "" || o.Lines != nil || o.Text != nil || len(o.Keys) != 0 {
			return errors.New("invalid prompt response")
		}
	default:
		return errors.New("unsupported operation")
	}
	return nil
}

func validInputText(s string) error {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) < 1 || utf8.RuneCountInString(s) > 4096 || len(s) > 16*1024 {
		return errors.New("invalid text length")
	}
	for _, r := range s {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return errors.New("text contains control character")
		}
	}
	return nil
}

func validKeys(values []string) error {
	if len(values) < 1 || len(values) > 16 {
		return errors.New("invalid keys")
	}
	for _, k := range values {
		if !keys[k] {
			return errors.New("invalid keys")
		}
	}
	return nil
}

func ValidateResult(r ActionResult, browser bool) error {
	if !IsUUIDv7(r.ActionID) || !operations[r.OperationType] {
		return errors.New("invalid action result identity")
	}
	if browser && !IsUUIDv7(r.SessionID) {
		return errors.New("invalid session")
	}
	if r.Status == "succeeded" {
		if r.Code != nil || len(r.Result) == 0 || bytes.Equal(r.Result, []byte("null")) {
			return errors.New("invalid success")
		}
		return validateSuccessResult(r)
	}
	if r.Code == nil || len(r.Result) == 0 || !bytes.Equal(r.Result, []byte("null")) {
		return errors.New("invalid failure result")
	}
	code := *r.Code
	write := IsWrite(r.OperationType)
	switch r.Status {
	case "unknown":
		if !write || code != "OUTCOME_UNKNOWN" {
			return errors.New("invalid unknown outcome")
		}
	case "failed":
		if write && code != "HERDR_REJECTED" {
			return errors.New("invalid write failure")
		}
		if !write && !slices.Contains([]string{"CONNECTION_LOST", "DEADLINE_EXCEEDED", "HERDR_UNAVAILABLE", "HERDR_REJECTED", "INTERNAL"}, code) {
			return errors.New("invalid read failure")
		}
	case "rejected":
		allowed := []string{"INVALID_MESSAGE", "UNSUPPORTED_OPERATION", "UNAUTHORIZED", "UNAUTHORIZED_HOST", "DUPLICATE_ACTION", "TARGET_NOT_FOUND", "STALE_TARGET", "STALE_STATE", "NOT_AN_AGENT", "PROMPT_CHANGED", "INVALID_TEXT", "INVALID_KEYS", "DEADLINE_EXCEEDED", "HERDR_UNAVAILABLE", "HERDR_INCOMPATIBLE", "AUDIT_UNAVAILABLE", "BUSY", "RATE_LIMITED"}
		if write {
			allowed = append(allowed, "INTERNAL")
		}
		if !slices.Contains(allowed, code) {
			return errors.New("invalid rejection")
		}
	default:
		return errors.New("invalid status")
	}
	return nil
}

func validateSuccessResult(r ActionResult) error {
	if r.OperationType == "agent.read" {
		var rr ReadResult
		if err := strictDecode(r.Result, &rr); err != nil {
			return err
		}
		if !IsUUIDv7(rr.StateEpoch) || rr.AgentGeneration < 1 || !hashRE.MatchString(rr.ContentRevision) || utf8.RuneCountInString(rr.Text) > MaxOutputRunes {
			return errors.New("invalid read result")
		}
		return nil
	}
	var wr WriteResult
	if err := strictDecode(r.Result, &wr); err != nil {
		return err
	}
	if !wr.HerdrAcknowledged {
		return errors.New("write not acknowledged")
	}
	if r.OperationType == "prompt.respond" && !optionRE.MatchString(wr.OptionID) {
		return errors.New("missing option id")
	}
	if r.OperationType != "prompt.respond" && wr.OptionID != "" {
		return errors.New("unexpected option id")
	}
	return nil
}

func ValidateSnapshot(s InstanceSnapshot) error {
	if !instanceRE.MatchString(s.InstanceID) || !IsUUIDv7(s.EffectiveEpoch()) || s.Sequence != 0 || len(s.Agents) > MaxAgents {
		return errors.New("invalid instance snapshot")
	}
	if !versionRE.MatchString(s.HerdrVersion) || s.HerdrProtocol < 0 || s.HerdrProtocol > 65535 {
		return errors.New("invalid Herdr version")
	}
	if err := ValidateCapabilities(s.Capabilities); err != nil {
		return err
	}
	if s.Status != "online" && s.Status != "degraded" && s.Status != "incompatible" && s.Status != "offline" {
		return errors.New("invalid instance status")
	}
	seen := map[string]bool{}
	for _, a := range s.Agents {
		if seen[a.TerminalID] {
			return errors.New("duplicate terminal_id")
		}
		seen[a.TerminalID] = true
		if err := validateAgent(a); err != nil {
			return err
		}
		if s.HerdrVersion == "0.7.3" && a.HerdrInputRevision != 0 {
			return errors.New("Herdr 0.7.3 revision must be zero")
		}
	}
	if s.HerdrVersion == "0.7.3" && HasCapability(s.Capabilities, "checked_input.v1") {
		return errors.New("Herdr 0.7.3 cannot write")
	}
	return nil
}

func validateAgent(a Agent) error {
	if err := boundedUntrusted(a.TerminalID, 1, 128, true); err != nil {
		return err
	}
	if err := boundedUntrusted(a.Agent, 1, 80, true); err != nil {
		return err
	}
	if !statuses[a.Status] || a.EffectiveGeneration() < 1 {
		return errors.New("invalid agent state")
	}
	if a.DisplayName != nil && boundedUntrusted(*a.DisplayName, 1, 80, true) != nil {
		return errors.New("invalid display name")
	}
	if a.Project != nil && (strings.ContainsAny(*a.Project, `/\\`) || boundedUntrusted(*a.Project, 1, 120, true) != nil) {
		return errors.New("invalid project label")
	}
	return nil
}

func ValidateSessionSnapshot(s SessionSnapshot) error {
	if !IsUUIDv7(s.SessionID) || !IsUUIDv7(s.StateEpoch) || s.Sequence != 0 || len(s.Hosts) > 10 {
		return errors.New("invalid session snapshot")
	}
	if err := validateTimestamp(s.ServerTime); err != nil {
		return err
	}
	hosts := map[string]bool{}
	for _, h := range s.Hosts {
		if !IsUUIDv7(h.HostID) || hosts[h.HostID] || len(h.Instances) > MaxInstances || boundedUntrusted(h.DisplayName, 1, 80, true) != nil || (h.Status != "connected" && h.Status != "disconnected") {
			return errors.New("invalid or duplicate host")
		}
		hosts[h.HostID] = true
		instances := map[string]bool{}
		for _, i := range h.Instances {
			if instances[i.InstanceID] {
				return errors.New("duplicate instance")
			}
			instances[i.InstanceID] = true
			if err := validateBrowserInstance(i); err != nil {
				return err
			}
			for _, a := range i.Agents {
				if a.ConnectorEpoch != i.ConnectorEpoch {
					return errors.New("agent connector epoch mismatch")
				}
			}
		}
	}
	return nil
}

func validateBrowserInstance(i BrowserInstance) error {
	return ValidateSnapshot(InstanceSnapshot{
		InstanceID:     i.InstanceID,
		ConnectorEpoch: i.ConnectorEpoch,
		Sequence:       0,
		HerdrVersion:   i.HerdrVersion,
		HerdrProtocol:  i.HerdrProtocol,
		Status:         i.Status,
		Capabilities:   i.Capabilities,
		Agents:         i.Agents,
	})
}

func ValidatePrompt(p PromptSnapshot, browser bool) error {
	if err := ValidateTarget(p.Target, browser); err != nil {
		return err
	}
	if !IsUUIDv7(p.StateEpoch) || p.AgentGeneration < 1 || !hashRE.MatchString(p.HerdrContentHash) || !hashRE.MatchString(p.Fingerprint) {
		return errors.New("invalid prompt binding")
	}
	limit := MaxPromptRunes
	if browser {
		limit = MaxBrowserPromptRunes
		if !IsUUIDv7(p.SessionID) || !IsUUIDv7(p.ConnectorEpoch) {
			return errors.New("invalid browser prompt binding")
		}
	}
	if utf8.RuneCountInString(p.Excerpt) > limit || len(p.Options) > 32 {
		return errors.New("prompt too large")
	}
	if !browser && len([]byte(p.Excerpt)) > 8*1024 {
		return errors.New("connector prompt exceeds byte limit")
	}
	if !versionRE.MatchString(p.AdapterVersion) {
		return errors.New("invalid adapter version")
	}
	seen := map[string]bool{}
	for _, o := range p.Options {
		if !optionRE.MatchString(o.ID) || seen[o.ID] || boundedUntrusted(o.Label, 1, 80, true) != nil {
			return errors.New("invalid prompt option")
		}
		seen[o.ID] = true
	}
	return nil
}

func ValidateOutputSubscribe(s OutputSubscribe, browser bool) error {
	if !IsUUIDv7(s.SubscriptionID) || (browser && !IsUUIDv7(s.SessionID)) {
		return errors.New("invalid subscription id")
	}
	if err := ValidateTarget(s.Target, browser); err != nil {
		return err
	}
	if !sources[s.Source] || s.Lines < 1 || s.Lines > 1000 || s.PollIntervalMS < 500 || s.PollIntervalMS > 5000 {
		return errors.New("invalid output subscription")
	}
	return nil
}

func ValidateOutputSnapshot(s OutputSnapshot, browser bool) error {
	if !IsUUIDv7(s.SubscriptionID) || !IsUUIDv7(s.StateEpoch) || s.AgentGeneration < 1 || !hashRE.MatchString(s.ContentRevision) || utf8.RuneCountInString(s.Text) > MaxOutputRunes {
		return errors.New("invalid output snapshot")
	}
	if browser && (!IsUUIDv7(s.SessionID) || !IsUUIDv7(s.ConnectorEpoch)) {
		return errors.New("invalid browser output binding")
	}
	return ValidateTarget(s.Target, browser)
}

func ValidateConnectorDelta(d StateDelta) error {
	if d.SessionID != "" || d.StateEpoch != "" || !instanceRE.MatchString(d.InstanceID) || !IsUUIDv7(d.Epoch) || d.Sequence < 1 || len(d.Changes) < 1 || len(d.Changes) > 512 {
		return errors.New("invalid connector delta")
	}
	seen := map[string]bool{}
	for _, change := range d.Changes {
		switch change.Operation {
		case "upsert":
			if change.Agent == nil || change.TerminalID != "" || change.Target != nil || change.Reason != "" || change.HostID != "" || change.InstanceID != "" || change.PreviousConnectorEpoch != "" || change.ConnectorEpoch != "" || change.Host != nil || change.Instance != nil {
				return errors.New("invalid connector upsert")
			}
			a := *change.Agent
			if a.Generation < 1 || a.AgentGeneration != 0 || a.ConnectorEpoch != "" {
				return errors.New("invalid connector generation fields")
			}
			if seen[a.TerminalID] {
				return errors.New("duplicate terminal change")
			}
			seen[a.TerminalID] = true
			if err := validateAgent(a); err != nil {
				return err
			}
			if boundedUntrusted(a.PaneID, 1, 128, true) != nil || boundedUntrusted(a.WorkspaceID, 1, 128, true) != nil || boundedUntrusted(a.TabID, 1, 128, true) != nil {
				return errors.New("invalid connector route")
			}
		case "remove":
			if change.Agent != nil || change.TerminalID == "" || change.Target != nil || change.HostID != "" || change.InstanceID != "" || change.PreviousConnectorEpoch != "" || change.ConnectorEpoch != "" || change.Host != nil || change.Instance != nil {
				return errors.New("invalid connector remove")
			}
			if seen[change.TerminalID] || boundedUntrusted(change.TerminalID, 1, 128, true) != nil {
				return errors.New("duplicate or invalid terminal change")
			}
			seen[change.TerminalID] = true
			if !slices.Contains([]string{"pane_closed", "agent_exited", "reconciled"}, change.Reason) {
				return errors.New("invalid removal reason")
			}
		default:
			return errors.New("unsupported connector change")
		}
	}
	return nil
}
func validateBrowserDelta(d StateDelta) error {
	if !IsUUIDv7(d.SessionID) || !IsUUIDv7(d.StateEpoch) || d.Sequence < 1 || len(d.Changes) < 1 || len(d.Changes) > 512 {
		return errors.New("invalid browser delta")
	}
	seen := map[string]bool{}
	for _, c := range d.Changes {
		var key string
		switch c.Operation {
		case "host.upsert":
			if !browserChangeShape(c, "host.upsert") || !IsUUIDv7(c.HostID) || c.Host == nil || boundedUntrusted(c.Host.DisplayName, 1, 80, true) != nil || (c.Host.Status != "connected" && c.Host.Status != "disconnected") {
				return errors.New("invalid host upsert")
			}
			key = "h:" + c.HostID
		case "host.remove":
			if !browserChangeShape(c, "host.remove") || !IsUUIDv7(c.HostID) || !slices.Contains([]string{"unenrolled", "authorization_changed"}, c.Reason) {
				return errors.New("invalid host removal")
			}
			key = "h:" + c.HostID
		case "instance.upsert":
			if !browserChangeShape(c, "instance.upsert") || !IsUUIDv7(c.HostID) || !instanceRE.MatchString(c.InstanceID) || c.Instance == nil || !IsUUIDv7(c.Instance.ConnectorEpoch) || !versionRE.MatchString(c.Instance.HerdrVersion) || ValidateCapabilities(c.Instance.Capabilities) != nil {
				return errors.New("invalid instance upsert")
			}
			key = "i:" + c.HostID + ":" + c.InstanceID
		case "instance.remove":
			if !browserChangeShape(c, "instance.remove") || !IsUUIDv7(c.HostID) || !instanceRE.MatchString(c.InstanceID) || !slices.Contains([]string{"unconfigured", "host_unenrolled"}, c.Reason) {
				return errors.New("invalid instance removal")
			}
			key = "i:" + c.HostID + ":" + c.InstanceID
		case "instance.epoch_changed":
			if !browserChangeShape(c, "instance.epoch_changed") || !IsUUIDv7(c.HostID) || !instanceRE.MatchString(c.InstanceID) || !IsUUIDv7(c.PreviousConnectorEpoch) || !IsUUIDv7(c.ConnectorEpoch) || c.PreviousConnectorEpoch == c.ConnectorEpoch {
				return errors.New("invalid epoch change")
			}
			key = "i:" + c.HostID + ":" + c.InstanceID
		case "agent.upsert":
			if !browserChangeShape(c, "agent.upsert") || c.Target == nil || c.Agent == nil || ValidateTarget(*c.Target, true) != nil || c.Agent.TerminalID != "" || !IsUUIDv7(c.Agent.ConnectorEpoch) || c.Agent.EffectiveGeneration() < 1 || boundedUntrusted(c.Agent.Agent, 1, 80, true) != nil || !statuses[c.Agent.Status] {
				return errors.New("invalid agent upsert")
			}
			key = "a:" + c.Target.HostID + ":" + c.Target.InstanceID + ":" + c.Target.TerminalID
		case "agent.remove":
			if !browserChangeShape(c, "agent.remove") || c.Target == nil || ValidateTarget(*c.Target, true) != nil || !slices.Contains([]string{"pane_closed", "agent_exited", "reconciled"}, c.Reason) {
				return errors.New("invalid agent removal")
			}
			key = "a:" + c.Target.HostID + ":" + c.Target.InstanceID + ":" + c.Target.TerminalID
		default:
			return errors.New("unsupported browser change")
		}
		if seen[key] {
			return errors.New("duplicate logical browser change")
		}
		seen[key] = true
	}
	return nil
}
func browserChangeShape(c StateChange, kind string) bool {
	agent := c.Agent != nil
	target := c.Target != nil
	host := c.Host != nil
	instance := c.Instance != nil
	switch kind {
	case "host.upsert":
		return host && !agent && !target && !instance && c.HostID != "" && c.InstanceID == "" && c.TerminalID == "" && c.Reason == "" && c.PreviousConnectorEpoch == "" && c.ConnectorEpoch == ""
	case "host.remove":
		return !host && !agent && !target && !instance && c.HostID != "" && c.InstanceID == "" && c.TerminalID == "" && c.Reason != "" && c.PreviousConnectorEpoch == "" && c.ConnectorEpoch == ""
	case "instance.upsert":
		return instance && !host && !agent && !target && c.HostID != "" && c.InstanceID != "" && c.TerminalID == "" && c.Reason == "" && c.PreviousConnectorEpoch == "" && c.ConnectorEpoch == ""
	case "instance.remove":
		return !instance && !host && !agent && !target && c.HostID != "" && c.InstanceID != "" && c.TerminalID == "" && c.Reason != "" && c.PreviousConnectorEpoch == "" && c.ConnectorEpoch == ""
	case "instance.epoch_changed":
		return !instance && !host && !agent && !target && c.HostID != "" && c.InstanceID != "" && c.TerminalID == "" && c.Reason == "" && c.PreviousConnectorEpoch != "" && c.ConnectorEpoch != ""
	case "agent.upsert":
		return agent && target && !host && !instance && c.HostID == "" && c.InstanceID == "" && c.TerminalID == "" && c.Reason == "" && c.PreviousConnectorEpoch == "" && c.ConnectorEpoch == ""
	case "agent.remove":
		return !agent && target && !host && !instance && c.HostID == "" && c.InstanceID == "" && c.TerminalID == "" && c.Reason != "" && c.PreviousConnectorEpoch == "" && c.ConnectorEpoch == ""
	}
	return false
}

func boundedUntrusted(s string, min, max int, controls bool) error {
	if !utf8.ValidString(s) {
		return errors.New("invalid UTF-8")
	}
	n := utf8.RuneCountInString(s)
	if n < min || n > max {
		return errors.New("length out of bounds")
	}
	if controls {
		for _, r := range s {
			if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
				return errors.New("control character")
			}
		}
	}
	return nil
}

// DecodeStrict rejects unknown fields, trailing data, invalid envelopes, and
// invalid type-specific bodies. side is "browser" or "connector".
func DecodeStrict(frame []byte, side string) (Envelope, any, error) {
	if len(frame) == 0 || len(frame) > MaxFrameBytes {
		return Envelope{}, nil, errors.New("invalid frame size")
	}
	var e Envelope
	if err := strictDecode(frame, &e); err != nil {
		return e, nil, err
	}
	if !IsUUIDv7(e.MessageID) {
		return e, nil, errors.New("message_id must be UUIDv7")
	}
	if err := validateTimestamp(e.SentAt); err != nil {
		return e, nil, err
	}
	bootstrap := e.Type == "connector.hello" || e.Type == "server.welcome" || (e.Type == "protocol.error" && e.Protocol == 0)
	if (bootstrap && e.Protocol != 0) || (!bootstrap && e.Protocol != Version) {
		return e, nil, errors.New("unsupported protocol")
	}
	var body any
	switch e.Type {
	case "connector.hello":
		body = &Hello{}
	case "server.welcome":
		body = &Welcome{}
	case "state.snapshot":
		body = &InstanceSnapshot{}
	case "state.inventory":
		body = &InstanceInventory{}
	case "state.delta":
		body = &StateDelta{}
	case "state.resync":
		if side == "browser" {
			body = &StateResync{}
		} else {
			body = &ConnectorStateResync{}
		}
	case "prompt.snapshot":
		body = &PromptSnapshot{}
	case "output.subscribe":
		body = &OutputSubscribe{}
	case "output.unsubscribe":
		body = &OutputUnsubscribe{}
	case "output.snapshot":
		body = &OutputSnapshot{}
	case "action.request":
		if side == "browser" {
			body = &BrowserActionRequest{}
		} else {
			body = &ActionRequest{}
		}
	case "action.received":
		body = &ActionReceived{}
	case "action.result":
		body = &ActionResult{}
	case "session.snapshot":
		body = &SessionSnapshot{}
	case "protocol.error":
		body = &ProtocolError{}
	default:
		return e, nil, errors.New("unsupported message type")
	}
	if err := strictDecode(e.Body, body); err != nil {
		return e, nil, err
	}
	var rawFields map[string]json.RawMessage
	_ = json.Unmarshal(e.Body, &rawFields)
	if side == "browser" && e.Type == "action.result" {
		if _, ok := rawFields["message"]; ok {
			return e, nil, errors.New("connector message forbidden in browser result")
		}
	}
	if side == "connector" {
		if _, ok := rawFields["session_id"]; ok {
			return e, nil, errors.New("browser session field forbidden")
		}
		if e.Type == "action.result" {
			if _, ok := rawFields["message"]; !ok {
				return e, nil, errors.New("connector result message required")
			}
		}
		if e.Type == "action.request" {
			var req struct {
				Expected map[string]json.RawMessage `json:"expected"`
			}
			_ = json.Unmarshal(e.Body, &req)
			if _, ok := req.Expected["connector_epoch"]; ok {
				return e, nil, errors.New("browser connector epoch field forbidden")
			}
		}
	}
	if err := validateDecoded(body, side); err != nil {
		return e, nil, err
	}
	return e, body, nil
}

func validateDecoded(v any, side string) error {
	browser := side == "browser"
	switch b := v.(type) {
	case *Hello:
		if b.MinProtocol < 1 || b.MaxProtocol < b.MinProtocol || !IsUUIDv7(b.ConnectorInstanceID) || boundedUntrusted(b.DisplayName, 1, 80, true) != nil || !versionRE.MatchString(b.ConnectorVersion) || boundedUntrusted(b.Architecture, 1, 32, true) != nil || (b.Platform != "linux" && b.Platform != "darwin") {
			return errors.New("invalid hello")
		}
		return validateCapabilityNames(b.Capabilities)
	case *Welcome:
		if b.SelectedProtocol != 1 || !IsUUIDv7(b.ConnectionID) || !IsUUID(b.HostID) || b.HeartbeatIntervalMS < 1000 || b.MaxMessageBytes < 1 || b.MaxMessageBytes > MaxFrameBytes {
			return errors.New("invalid welcome")
		}
		if err := validateTimestamp(b.ServerTime); err != nil {
			return err
		}
		return validateCapabilityNames(b.AcceptedCapabilities)
	case *InstanceSnapshot:
		if err := ValidateSnapshot(*b); err != nil {
			return err
		}
		if !browser {
			if b.Status == "offline" {
				return errors.New("connector snapshot cannot be offline")
			}
			for _, a := range b.Agents {
				if a.Generation < 1 || a.AgentGeneration != 0 || a.ConnectorEpoch != "" {
					return errors.New("invalid connector generation fields")
				}
				if boundedUntrusted(a.PaneID, 1, 128, true) != nil || boundedUntrusted(a.WorkspaceID, 1, 128, true) != nil || boundedUntrusted(a.TabID, 1, 128, true) != nil {
					return errors.New("invalid connector route")
				}
			}
		} else {
			for _, a := range b.Agents {
				if a.Generation != 0 || a.AgentGeneration < 1 || a.ConnectorEpoch != b.EffectiveEpoch() || a.PaneID != "" || a.WorkspaceID != "" || a.TabID != "" {
					return errors.New("invalid browser agent projection")
				}
			}
		}
		return nil
	case *InstanceInventory:
		if browser || len(b.InstanceIDs) < 1 || len(b.InstanceIDs) > MaxInstances {
			return errors.New("invalid instance inventory")
		}
		seen := make(map[string]struct{}, len(b.InstanceIDs))
		for _, instanceID := range b.InstanceIDs {
			if !instanceRE.MatchString(instanceID) {
				return errors.New("invalid instance inventory")
			}
			if _, exists := seen[instanceID]; exists {
				return errors.New("duplicate instance inventory entry")
			}
			seen[instanceID] = struct{}{}
		}
		return nil
	case *PromptSnapshot:
		return ValidatePrompt(*b, browser)
	case *OutputSubscribe:
		return ValidateOutputSubscribe(*b, browser)
	case *OutputSnapshot:
		return ValidateOutputSnapshot(*b, browser)
	case *OutputUnsubscribe:
		if !IsUUIDv7(b.SubscriptionID) || (browser && !IsUUIDv7(b.SessionID)) {
			return errors.New("invalid unsubscribe")
		}
		return nil
	case *StateResync:
		if !browser || !IsUUIDv7(b.SessionID) {
			return errors.New("invalid resync")
		}
		if b.ExpectedEpoch != nil && !IsUUIDv7(*b.ExpectedEpoch) {
			return errors.New("invalid resync epoch")
		}
		if !slices.Contains([]string{"gap", "epoch_mismatch", "unknown_remove", "connector_epoch_changed", "operator_refresh"}, b.Reason) {
			return errors.New("invalid resync reason")
		}
		return nil
	case *ConnectorStateResync:
		if browser || !instanceRE.MatchString(b.InstanceID) {
			return errors.New("invalid connector resync")
		}
		if b.ExpectedEpoch != nil && !IsUUIDv7(*b.ExpectedEpoch) {
			return errors.New("invalid resync epoch")
		}
		if !slices.Contains([]string{"gap", "epoch_mismatch", "unknown_remove", "operator_refresh"}, b.Reason) {
			return errors.New("invalid resync reason")
		}
		return nil
	case *StateDelta:
		if browser {
			return validateBrowserDelta(*b)
		}
		return ValidateConnectorDelta(*b)
	case *ProtocolError:
		if browser {
			if !IsUUIDv7(b.SessionID) || b.Message != "" || b.Fatal == nil {
				return errors.New("invalid browser protocol error")
			}
		} else if b.SessionID != "" || b.Fatal != nil || boundedUntrusted(b.Message, 1, 512, true) != nil {
			return errors.New("invalid connector protocol error")
		}
		if b.InReplyTo != nil && !IsUUIDv7(*b.InReplyTo) {
			return errors.New("invalid protocol reply ID")
		}
		if b.Code == "" {
			return errors.New("missing protocol code")
		}
		return nil
	case *ActionRequest:
		return ValidateAction(*b, false)
	case *BrowserActionRequest:
		if !IsUUIDv7(b.SessionID) {
			return errors.New("invalid session")
		}
		return ValidateAction(b.ActionRequest, true)
	case *ActionReceived:
		if !IsUUIDv7(b.ActionID) || (browser && !IsUUIDv7(b.SessionID)) {
			return errors.New("invalid receipt")
		}
	case *ActionResult:
		if !browser && len(b.Message) > 0 && !bytes.Equal(b.Message, []byte("null")) {
			var message string
			if err := json.Unmarshal(b.Message, &message); err != nil || boundedUntrusted(message, 0, 512, true) != nil {
				return errors.New("invalid result message")
			}
		}
		return ValidateResult(*b, browser)
	case *SessionSnapshot:
		return ValidateSessionSnapshot(*b)
	}
	return nil
}

func strictDecode(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing JSON")
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func validateCapabilityNames(c []string) error {
	if len(c) > 16 {
		return errors.New("too many capabilities")
	}
	seen := map[string]bool{}
	for _, v := range c {
		if !capabilityNameRE.MatchString(v) || seen[v] {
			return errors.New("invalid or duplicate capability")
		}
		seen[v] = true
	}
	return nil
}

func validateTimestamp(s string) error {
	if !strings.HasSuffix(s, "Z") {
		return errors.New("timestamp must be UTC")
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		return errors.New("invalid timestamp")
	}
	return nil
}

func MarshalEnvelope(protocol int, typ string, body any) ([]byte, error) {
	id, err := NewUUIDv7()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Protocol: protocol, MessageID: id, Type: typ, SentAt: time.Now().UTC().Format(time.RFC3339Nano), Body: raw})
}

// UUIDv7Time extracts the millisecond timestamp for retention diagnostics.
func UUIDv7Time(id string) (time.Time, error) {
	if !IsUUIDv7(id) {
		return time.Time{}, errors.New("invalid UUIDv7")
	}
	b, err := hex.DecodeString(strings.ReplaceAll(id[:18], "-", ""))
	if err != nil {
		return time.Time{}, err
	}
	var p [8]byte
	copy(p[2:], b[:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(p[:]))), nil
}
