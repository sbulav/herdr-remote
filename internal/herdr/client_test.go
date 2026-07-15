package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

func TestUnixNDJSONClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadBytes('\n')
		var req request
		_ = json.Unmarshal(line, &req)
		_ = json.NewEncoder(conn).Encode(map[string]any{"id": req.ID, "result": map[string]any{"type": "pong", "version": "0.7.3", "protocol": 16, "capabilities": map[string]bool{}}})
	}()
	client, err := NewUnixClient(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := client.Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "0.7.3" || p.Protocol != 16 {
		t.Fatalf("unexpected ping: %#v", p)
	}
}

func TestCheckedCapabilityRequiresAdvertisedAtomicSchema(t *testing.T) {
	p := Ping{Version: "0.7.3", Capabilities: map[string]bool{"checked_input.v1": true}}
	schema := completeCheckedSchema()
	if SupportsCheckedInput(p, schema) {
		t.Fatal("0.7.3 must remain read-only")
	}
	p.Version = "0.8.0"
	if !SupportsCheckedInput(p, schema) {
		t.Fatal("checked API not recognized")
	}
	schema.Methods[0].Atomic = false
	if SupportsCheckedInput(p, schema) {
		t.Fatal("non-atomic API accepted")
	}
}

func TestCheckedCapabilityRequiresFullReadAndWriteSchema(t *testing.T) {
	ping := Ping{Version: "0.8.0", Capabilities: map[string]bool{"checked_input.v1": true}}
	partial := APISchema{Methods: []Method{{Name: "agent.send_input_checked", Atomic: true, Parameters: []string{"terminal_id", "expected_input_revision", "expected_agent", "expected_status"}}}}
	if SupportsCheckedInput(ping, partial) {
		t.Fatal("partial checked schema enabled writes")
	}
	if !SupportsCheckedInput(ping, completeCheckedSchema()) {
		t.Fatal("complete checked schema was not recognized")
	}
	wrong := completeCheckedSchema()
	wrong.Methods[1].ResultTypes["input_revision"] = "string"
	if SupportsCheckedInput(ping, wrong) {
		t.Fatal("checked schema with wrong response type enabled writes")
	}
}
func completeCheckedSchema() APISchema {
	return APISchema{Methods: []Method{
		{Name: "agent.read", Atomic: true, Parameters: []string{"target", "source", "lines", "format", "strip_ansi"}, ResultFields: []string{"text", "input_revision", "content_hash", "truncated"}, ParameterTypes: map[string]string{"target": "string", "source": "string", "lines": "integer", "format": "string", "strip_ansi": "boolean"}, ResultTypes: map[string]string{"text": "string", "input_revision": "integer", "content_hash": "string", "truncated": "boolean"}},
		{Name: "agent.send_input_checked", Atomic: true, Parameters: []string{"terminal_id", "expected_input_revision", "expected_agent", "expected_status", "expected_content_hash", "text", "keys"}, ResultFields: []string{"enqueued", "input_revision"}, ParameterTypes: map[string]string{"terminal_id": "string", "expected_input_revision": "integer", "expected_agent": "string", "expected_status": "string", "expected_content_hash": "string", "text": "string", "keys": "array"}, ResultTypes: map[string]string{"enqueued": "boolean", "input_revision": "integer"}},
	}}
}
