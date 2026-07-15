// Package prompt provides deterministic, versioned prompt extraction. Prompt
// content is returned to callers and is never persisted by this package.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dcolinmorgan/herdr-remote/internal/protocol"
)

const Version = "1.0.0"

type Input struct {
	Text       string
	HostID     string
	InstanceID string
	TerminalID string
}
type BoundOption struct {
	protocol.PromptOption
	Text string
	Keys []string
}
type Snapshot struct {
	Canonical   string
	ContentHash string
	Fingerprint string
	Excerpt     string
	Truncated   bool
	Options     []BoundOption
}

var (
	numbered  = regexp.MustCompile(`(?m)^[ \t]*[❯>]?[ \t]*(\d+)\.\s+(\S[^\n]*)`)
	bullet    = regexp.MustCompile(`(?m)^[ \t]*(?:[❯>•*-]|\[[ \t]?\])[ \t]+([[:alpha:]][^\n]{0,80})`)
	optionID  = regexp.MustCompile(`[^a-z0-9._-]+`)
	trimPunct = regexp.MustCompile(`[.,;]+$`)
)

func Extract(in Input) Snapshot {
	normalized := strings.ReplaceAll(strings.ReplaceAll(in.Text, "\r\n", "\n"), "\r", "\n")
	window := lastBytesUTF8(normalized, 64*1024)
	options := detect(window)
	canonicalPrompt := strings.TrimSpace(window)
	var publicOptions []protocol.PromptOption
	for _, o := range options {
		publicOptions = append(publicOptions, o.PromptOption)
	}
	canonical := canonicalDocument(in, canonicalPrompt, publicOptions)
	excerpt, truncated := firstBytesUTF8(canonicalPrompt, 8*1024)
	return Snapshot{Canonical: canonical, ContentHash: hash(window), Fingerprint: hash(canonical), Excerpt: excerpt, Truncated: truncated, Options: options}
}

// canonicalDocument is the RFC 8785 serialization for this fixed document.
// Its object keys are lexicographically ordered and strings retain Unicode.
func canonicalDocument(in Input, prompt string, options []protocol.PromptOption) string {
	var b strings.Builder
	b.WriteString(`{"adapter_version":`)
	writeJCSString(&b, Version)
	b.WriteString(`,"host_id":`)
	writeJCSString(&b, in.HostID)
	b.WriteString(`,"instance_id":`)
	writeJCSString(&b, in.InstanceID)
	b.WriteString(`,"options":[`)
	for i, o := range options {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":`)
		writeJCSString(&b, o.ID)
		b.WriteString(`,"label":`)
		writeJCSString(&b, o.Label)
		b.WriteByte('}')
	}
	b.WriteString(`],"prompt":`)
	writeJCSString(&b, prompt)
	b.WriteString(`,"terminal_id":`)
	writeJCSString(&b, in.TerminalID)
	b.WriteString(`,"v":1}`)
	return b.String()
}
func writeJCSString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[byte(r)>>4])
				b.WriteByte(hex[byte(r)&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func detect(text string) []BoundOption {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "yes, single permission") {
		return textOptions([]string{"yes, single permission", "trust, always allow", "no (tab to edit)"})
	}
	if strings.Contains(lower, "approve all pending") || strings.Contains(lower, "pending from subagents") {
		return textOptions([]string{"approve all pending", "configure individually", "exit (cancel subagents)"})
	}
	if strings.Contains(lower, "permission required") || (strings.Contains(lower, "allow once") && strings.Contains(lower, "allow always") && strings.Contains(lower, "reject")) {
		return []BoundOption{keyOption("allow_once", "Allow once", []string{"enter"}), keyOption("allow_always", "Allow always", []string{"right", "enter", "enter"}), keyOption("reject", "Reject", []string{"esc"})}
	}
	matches := numbered.FindAllStringSubmatch(text, -1)
	if len(matches) >= 2 {
		out := make([]BoundOption, 0, len(matches))
		seen := map[string]bool{}
		for _, m := range matches {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			label := m[1] + ". " + strings.TrimSpace(m[2])
			out = append(out, textOption("option_"+m[1], label, m[1]))
		}
		return out
	}
	if options := bulletOptions(text); len(options) >= 2 {
		return options
	}
	if strings.Contains(lower, "do you want to proceed") || strings.Contains(lower, "do you want to allow") || strings.Contains(lower, "ask rule") || strings.Contains(lower, "/permissions to let auto mode decide") {
		return []BoundOption{textOption("option_1", "1. Yes", "1"), textOption("option_2", "2. No", "2")}
	}
	if strings.Contains(lower, "[y/n]") || strings.Contains(lower, "yes (y)") || strings.Contains(lower, "proceed (y)") {
		return textOptions([]string{"y", "n"})
	}
	if strings.Contains(lower, "allow once") && (strings.Contains(lower, "deny") || strings.Contains(lower, "allow for this session")) {
		return textOptions([]string{"allow once", "allow for this session", "deny"})
	}
	return nil
}

func bulletOptions(text string) []BoundOption {
	matches := bullet.FindAllStringSubmatch(text, -1)
	options := make([]BoundOption, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		label := strings.TrimSpace(trimPunct.ReplaceAllString(match[1], ""))
		key := strings.ToLower(label)
		if len(label) < 2 || seen[key] || strings.Contains(key, "esc to") || strings.Contains(key, "tab to") || strings.Contains(key, "ctrl+") || strings.Contains(key, "type to") || strings.Contains(key, "press ") {
			continue
		}
		seen[key] = true
		options = append(options, textOption(slug(key), label, label))
	}
	return options
}

func textOptions(labels []string) []BoundOption {
	out := make([]BoundOption, 0, len(labels))
	for _, l := range labels {
		out = append(out, textOption(slug(l), l, l))
	}
	return out
}
func textOption(id, label, text string) BoundOption {
	return BoundOption{PromptOption: protocol.PromptOption{ID: id, Label: label}, Text: text}
}
func keyOption(id, label string, keys []string) BoundOption {
	return BoundOption{PromptOption: protocol.PromptOption{ID: id, Label: label}, Keys: keys}
}
func slug(value string) string {
	id := strings.Trim(optionID.ReplaceAllString(strings.ToLower(value), "_"), "_.-")
	if id == "" {
		id = "option"
	}
	if len(id) > 80 {
		id = strings.TrimRight(id[:80], "_.-")
	}
	return id
}
func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}
func lastBytesUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	b := []byte(s)
	start := len(b) - n
	for start < len(b) && !utf8.RuneStart(b[start]) {
		start++
	}
	return string(b[start:])
}
func firstBytesUTF8(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	b := []byte(s)
	end := n
	for end > 0 && !utf8.RuneStart(b[end]) {
		end--
	}
	return string(b[:end]), true
}
