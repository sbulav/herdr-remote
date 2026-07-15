package prompt

import (
	"testing"

	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
)

func TestPromptHashAndAdapterBinding(t *testing.T) {
	in := Input{Text: "header\r\nPermission required: run tests?\r\nAllow once Allow always Reject", HostID: "019f64ca-1000-7000-8000-000000000002", InstanceID: "default", TerminalID: "term"}
	first := Extract(in)
	second := Extract(in)
	if first.Fingerprint != second.Fingerprint || first.ContentHash != second.ContentHash {
		t.Fatal("prompt hashing is not deterministic")
	}
	if len(first.Options) != 3 || first.Options[0].ID != "allow_once" {
		t.Fatalf("adapter options = %#v", first.Options)
	}
	in.TerminalID = "other"
	other := Extract(in)
	if other.Fingerprint == first.Fingerprint {
		t.Fatal("fingerprint is not target-bound")
	}
	if other.ContentHash != first.ContentHash {
		t.Fatal("content hash should bind only normalized Herdr content")
	}
}
func TestPromptWindowAndExcerptAreBounded(t *testing.T) {
	text := make([]byte, 70*1024)
	for i := range text {
		text[i] = 'x'
	}
	p := Extract(Input{Text: string(text), HostID: "h", InstanceID: "i", TerminalID: "t"})
	if len(p.Excerpt) > 8*1024 || !p.Truncated {
		t.Fatalf("excerpt was not byte bounded: %d", len(p.Excerpt))
	}
}
func TestCanonicalDocumentUsesJCSKeyOrder(t *testing.T) {
	got := canonicalDocument(Input{HostID: "h", InstanceID: "i", TerminalID: "t"}, "x\u2028y", []protocol.PromptOption{{ID: "a", Label: "A"}})
	want := "{\"adapter_version\":\"1.0.0\",\"host_id\":\"h\",\"instance_id\":\"i\",\"options\":[{\"id\":\"a\",\"label\":\"A\"}],\"prompt\":\"x\u2028y\",\"terminal_id\":\"t\",\"v\":1}"
	if got != want {
		t.Fatalf("canonical document\n got: %s\nwant: %s", got, want)
	}
}
