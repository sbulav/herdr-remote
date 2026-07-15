package prompt

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
)

type goldenOption struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Text  string   `json:"text,omitempty"`
	Keys  []string `json:"keys,omitempty"`
}

type goldenPrompt struct {
	Name    string         `json:"name"`
	Prompt  string         `json:"prompt"`
	Options []goldenOption `json:"options"`
}

func TestPromptResponseMappingsGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/response_mappings.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenPrompt
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			snapshot := Extract(Input{Text: test.Prompt, HostID: "host", InstanceID: "default", TerminalID: "term"})
			got := make([]goldenOption, 0, len(snapshot.Options))
			for _, option := range snapshot.Options {
				got = append(got, goldenOption{ID: option.ID, Label: option.Label, Text: option.Text, Keys: option.Keys})
			}
			if !reflect.DeepEqual(got, test.Options) {
				t.Fatalf("response mapping\n got: %#v\nwant: %#v", got, test.Options)
			}
		})
	}
}

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
