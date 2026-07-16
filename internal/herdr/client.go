// Package herdr implements the bounded NDJSON Unix-socket client used by the
// connector. It never invokes a shell or Herdr CLI command.
package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const maxLine = 256 * 1024

type Caller interface {
	Call(context.Context, string, any, any) error
	Subscribe(context.Context, []SubscriptionSpec) (*Subscription, error)
}

type UnixClient struct {
	SocketPath string
	Dial       func(context.Context, string, string) (net.Conn, error)
	next       atomic.Uint64
}

func NewUnixClient(path string) (*UnixClient, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("Herdr socket path must be absolute")
	}
	return &UnixClient{SocketPath: path}, nil
}

func (c *UnixClient) dial(ctx context.Context) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial(ctx, "unix", c.SocketPath)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.SocketPath)
}

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}
type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}
type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type RejectedError struct{ Code string }

func (e *RejectedError) Error() string   { return "Herdr rejected request (" + e.Code + ")" }
func DefinitiveRejection(err error) bool { var e *RejectedError; return errors.As(err, &e) }

func (c *UnixClient) Call(ctx context.Context, method string, params, out any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("dial Herdr: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	id := fmt.Sprintf("connector:%d", c.next.Add(1))
	if err := json.NewEncoder(conn).Encode(request{ID: id, Method: method, Params: params}); err != nil {
		return fmt.Errorf("write Herdr request: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read Herdr response: %w", err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("decode Herdr response: %w", err)
	}
	if resp.ID != id {
		return errors.New("Herdr response ID mismatch")
	}
	if resp.Error != nil {
		return &RejectedError{Code: resp.Error.Code}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("decode Herdr result: %w", err)
	}
	return nil
}

type SubscriptionSpec struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}
type Event struct {
	Type   string          `json:"type"`
	PaneID string          `json:"pane_id,omitempty"`
	Raw    json.RawMessage `json:"-"`
}
type Subscription struct {
	Events <-chan Event
	Errors <-chan error
	close  func() error
}

func (s *Subscription) Close() error {
	if s == nil || s.close == nil {
		return nil
	}
	return s.close()
}

func (c *UnixClient) Subscribe(ctx context.Context, specs []SubscriptionSpec) (*Subscription, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("connector:subscribe:%d", c.next.Add(1))
	if err := json.NewEncoder(conn).Encode(request{ID: id, Method: "events.subscribe", Params: map[string]any{"subscriptions": specs}}); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 32*1024)
	line, err := readBufferedLine(reader)
	if err != nil {
		conn.Close()
		return nil, err
	}
	var ack response
	if err := json.Unmarshal(line, &ack); err != nil || ack.ID != id || ack.Error != nil {
		conn.Close()
		return nil, errors.New("Herdr subscription rejected")
	}
	events := make(chan Event, 64)
	errs := make(chan error, 1)
	var intentionallyClosed atomic.Bool
	go func() {
		defer close(events)
		defer close(errs)
		defer conn.Close()
		for {
			line, err := readBufferedLine(reader)
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil && !intentionallyClosed.Load() {
					errs <- err
				}
				return
			}
			var raw struct {
				Event json.RawMessage `json:"event"`
			}
			if err := json.Unmarshal(line, &raw); err != nil || len(raw.Event) == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal(raw.Event, &ev); err != nil {
				continue
			}
			ev.Raw = append([]byte(nil), raw.Event...)
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &Subscription{Events: events, Errors: errs, close: func() error {
		intentionallyClosed.Store(true)
		return conn.Close()
	}}, nil
}

func readLine(r io.Reader) ([]byte, error) {
	br := bufio.NewReaderSize(r, 32*1024)
	return readBufferedLine(br)
}
func readBufferedLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(line) > maxLine {
		return nil, errors.New("Herdr frame too large")
	}
	return bytes.TrimSpace(line), nil
}

type Ping struct {
	Type         string          `json:"type"`
	Version      string          `json:"version"`
	Protocol     int             `json:"protocol"`
	Capabilities map[string]bool `json:"capabilities"`
}
type Agent struct {
	TerminalID    string  `json:"terminal_id"`
	Agent         string  `json:"agent"`
	AgentStatus   string  `json:"agent_status"`
	WorkspaceID   string  `json:"workspace_id"`
	TabID         string  `json:"tab_id"`
	PaneID        string  `json:"pane_id"`
	Revision      uint64  `json:"revision"`
	InputRevision uint64  `json:"input_revision,omitempty"`
	Focused       bool    `json:"focused,omitempty"`
	DisplayName   *string `json:"display_name,omitempty"`
	CWD           string  `json:"cwd,omitempty"`
}

func (a Agent) EffectiveRevision() uint64 {
	if a.InputRevision != 0 {
		return a.InputRevision
	}
	return a.Revision
}

type Snapshot struct {
	Type     string       `json:"type"`
	Snapshot SnapshotData `json:"snapshot"`
}
type SnapshotData struct {
	Version    string            `json:"version"`
	Protocol   int               `json:"protocol"`
	Workspaces []json.RawMessage `json:"workspaces"`
	Tabs       []json.RawMessage `json:"tabs"`
	Panes      []json.RawMessage `json:"panes"`
	Layouts    []json.RawMessage `json:"layouts"`
	Agents     []Agent           `json:"agents"`
}
type AgentInfo struct {
	Type  string `json:"type"`
	Agent Agent  `json:"agent"`
}
type PaneRead struct {
	Type string   `json:"type"`
	Read ReadData `json:"read"`
}
type ReadData struct {
	PaneID        string `json:"pane_id"`
	WorkspaceID   string `json:"workspace_id"`
	TabID         string `json:"tab_id"`
	Source        string `json:"source"`
	Format        string `json:"format"`
	Text          string `json:"text"`
	Revision      uint64 `json:"revision"`
	InputRevision uint64 `json:"input_revision,omitempty"`
	ContentHash   string `json:"content_hash,omitempty"`
	Truncated     bool   `json:"truncated"`
}

func (r ReadData) EffectiveRevision() uint64 {
	if r.InputRevision != 0 {
		return r.InputRevision
	}
	return r.Revision
}

func (c *UnixClient) Ping(ctx context.Context) (Ping, error) {
	var p Ping
	err := c.Call(ctx, "ping", map[string]any{}, &p)
	return p, err
}
func (c *UnixClient) Snapshot(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	err := c.Call(ctx, "session.snapshot", map[string]any{}, &s)
	return s, err
}
func (c *UnixClient) AgentGet(ctx context.Context, terminal string) (AgentInfo, error) {
	var a AgentInfo
	err := c.Call(ctx, "agent.get", map[string]any{"target": terminal}, &a)
	return a, err
}
func (c *UnixClient) Read(ctx context.Context, pane, source string, lines int) (PaneRead, error) {
	var r PaneRead
	err := c.Call(ctx, "pane.read", map[string]any{"pane_id": pane, "source": source, "lines": lines, "format": "text", "strip_ansi": true}, &r)
	return r, err
}
func (c *UnixClient) ReadAgent(ctx context.Context, terminal, source string, lines int) (PaneRead, error) {
	var r PaneRead
	err := c.Call(ctx, "agent.read", map[string]any{"target": terminal, "source": source, "lines": lines, "format": "text", "strip_ansi": true}, &r)
	return r, err
}

type APISchema struct {
	Type    string   `json:"type"`
	Methods []Method `json:"methods"`
}
type Method struct {
	Name           string            `json:"name"`
	Atomic         bool              `json:"atomic,omitempty"`
	Parameters     []string          `json:"parameters,omitempty"`
	ResultFields   []string          `json:"result_fields,omitempty"`
	ParameterTypes map[string]string `json:"parameter_types,omitempty"`
	ResultTypes    map[string]string `json:"result_types,omitempty"`
}

func (c *UnixClient) InspectSchema(ctx context.Context) (APISchema, error) {
	var s APISchema
	err := c.Call(ctx, "api.schema", map[string]any{}, &s)
	return s, err
}

func SupportsCheckedInput(p Ping, s APISchema) bool {
	if p.Version == "0.7.3" || !p.Capabilities["checked_input.v1"] {
		return false
	}
	readOK := false
	writeOK := false
	for _, m := range s.Methods {
		if !m.Atomic {
			continue
		}
		switch m.Name {
		case "agent.read":
			readOK = containsAll(m.Parameters, []string{"target", "source", "lines", "format", "strip_ansi"}) && containsAll(m.ResultFields, []string{"text", "input_revision", "content_hash", "truncated"}) && typesMatch(m.ParameterTypes, map[string]string{"target": "string", "source": "string", "lines": "integer", "format": "string", "strip_ansi": "boolean"}) && typesMatch(m.ResultTypes, map[string]string{"text": "string", "input_revision": "integer", "content_hash": "string", "truncated": "boolean"})
		case "agent.send_input_checked":
			writeOK = containsAll(m.Parameters, []string{"terminal_id", "expected_input_revision", "expected_agent", "expected_status", "expected_content_hash", "text", "keys"}) && containsAll(m.ResultFields, []string{"enqueued", "input_revision"}) && typesMatch(m.ParameterTypes, map[string]string{"terminal_id": "string", "expected_input_revision": "integer", "expected_agent": "string", "expected_status": "string", "expected_content_hash": "string", "text": "string", "keys": "array"}) && typesMatch(m.ResultTypes, map[string]string{"enqueued": "boolean", "input_revision": "integer"})
		}
	}
	return readOK && writeOK
}
func containsAll(got, want []string) bool {
	set := map[string]bool{}
	for _, v := range got {
		set[v] = true
	}
	for _, v := range want {
		if !set[v] {
			return false
		}
	}
	return true
}
func typesMatch(got, want map[string]string) bool {
	for field, kind := range want {
		if got[field] != kind {
			return false
		}
	}
	return true
}

type CheckedInput struct {
	TerminalID            string   `json:"terminal_id"`
	ExpectedInputRevision uint64   `json:"expected_input_revision"`
	ExpectedAgent         string   `json:"expected_agent"`
	ExpectedStatus        string   `json:"expected_status"`
	ExpectedContentHash   string   `json:"expected_content_hash,omitempty"`
	Text                  string   `json:"text,omitempty"`
	Keys                  []string `json:"keys,omitempty"`
}
type CheckedAck struct {
	Type          string `json:"type"`
	Enqueued      bool   `json:"enqueued"`
	InputRevision uint64 `json:"input_revision"`
}

// SendChecked is the only local write path. Callers must first prove the
// inspected schema supports this exact atomic method.
func (c *UnixClient) SendChecked(ctx context.Context, in CheckedInput) (CheckedAck, error) {
	var a CheckedAck
	err := c.Call(ctx, "agent.send_input_checked", in, &a)
	return a, err
}

func RedactedProject(cwd string) *string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil
	}
	base := filepath.Base(filepath.Clean(cwd))
	if base == "." || base == string(filepath.Separator) {
		return nil
	}
	return &base
}

var _ = time.Second
