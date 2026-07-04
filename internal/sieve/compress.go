package sieve

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Compressor shrinks a message array without changing its meaning.
type Compressor struct {
	cfg *Config
}

func NewCompressor(cfg *Config) *Compressor { return &Compressor{cfg: cfg} }

// CompressResult reports what the pipeline did.
type CompressResult struct {
	Messages   []Message
	Original   int
	Compressed int
	Saved      int
	Ratio      string
}

var (
	reCRLF       = regexp.MustCompile(`\r\n`)
	reTrailWS    = regexp.MustCompile(`(?m)[ \t]+$`)
	reBlankLines = regexp.MustCompile(`\n{3,}`)
)

// normalizeWhitespace performs lossless-for-code textual cleanup.
//
// It deliberately does NOT collapse runs of inline spaces, because leading
// indentation is semantically significant in many languages (e.g. Python) and
// inline alignment matters in code and tables. Only unambiguously safe edits
// are applied: CRLF→LF, trailing whitespace removal, and collapsing 3+ blank
// lines to 2. This keeps "no sacrificing context" true for source code.
func normalizeWhitespace(s string) string {
	s = reCRLF.ReplaceAllString(s, "\n")
	s = reTrailWS.ReplaceAllString(s, "")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// truncateRunes cuts a string to n runes (never mid-codepoint).
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Compress applies all enabled strategies in order.
func (c *Compressor) Compress(messages []Message) CompressResult {
	if len(messages) == 0 {
		return CompressResult{Messages: messages, Ratio: "0.0"}
	}

	original := sizeOf(messages)
	msgs := messages

	msgs = c.normalizeWS(msgs)
	msgs = c.deduplicate(msgs)
	if c.cfg.Compression.SummarizeOldTurns && len(msgs) > c.cfg.Compression.SummarizeAfterTurns {
		msgs = c.summarizeOld(msgs)
	}

	compressed := sizeOf(msgs)
	saved := original - compressed
	if saved < 0 {
		saved = 0
	}
	ratio := "0.0"
	if original > 0 {
		ratio = fmt.Sprintf("%.1f", float64(saved)/float64(original)*100)
	}

	return CompressResult{msgs, original, compressed, saved, ratio}
}

func sizeOf(messages []Message) int {
	n := 0
	for _, m := range messages {
		n += len(m.Text())
	}
	return n
}

// ── Strategy 1: whitespace ───────────────────────────────────────────────

func (c *Compressor) normalizeWS(messages []Message) []Message {
	out := make([]Message, len(messages))
	for i, m := range messages {
		m.Content = normalizeContent(m.Content)
		out[i] = m
	}
	return out
}

// normalizeContent cleans text whether content is a string or block array,
// preserving block structure and any non-text fields.
func normalizeContent(content json.RawMessage) json.RawMessage {
	if len(content) == 0 {
		return content
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		b, _ := json.Marshal(normalizeWhitespace(s))
		return b
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err == nil {
		for i := range blocks {
			if tx, ok := blocks[i]["text"]; ok {
				var txt string
				if json.Unmarshal(tx, &txt) == nil {
					nb, _ := json.Marshal(normalizeWhitespace(txt))
					blocks[i]["text"] = nb
				}
			}
		}
		b, _ := json.Marshal(blocks)
		return b
	}
	return content
}

// ── Strategy 2: exact duplicate removal ──────────────────────────────────

func isToolMessage(m Message) bool {
	if m.Role == "tool" {
		return true
	}
	if _, ok := m.Extra["tool_calls"]; ok {
		return true
	}
	if _, ok := m.Extra["tool_call_id"]; ok {
		return true
	}
	// Anthropic format: tool_use / tool_result are blocks *inside* Content, not
	// top-level fields. A tool_result-only user message has empty Text(), so
	// without this every such turn keys to the same "user\x00" dedup key and all
	// but the first get dropped — orphaning the matching tool_use and producing a
	// 400 from Anthropic. Treat any message carrying a tool block as a tool message.
	return containsToolBlock(m.Content)
}

// containsToolBlock reports whether an Anthropic content array holds a tool_use
// or tool_result block.
func containsToolBlock(content json.RawMessage) bool {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		switch blockType(b) {
		case "tool_use", "tool_result":
			return true
		}
	}
	return false
}

func (c *Compressor) deduplicate(messages []Message) []Message {
	seen := map[string]bool{}
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" || isToolMessage(m) { // never drop system or tool messages
			out = append(out, m)
			continue
		}
		// Key on the FULL message text (not a prefix) so two distinct messages
		// that happen to share a long opening can never be wrongly collapsed.
		// Only byte-for-byte identical messages are treated as duplicates.
		key := m.Role + "\x00" + m.Text()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

// ── Strategy 3: old-turn summarization ───────────────────────────────────

func (c *Compressor) summarizeOld(messages []Message) []Message {
	var system, convo []Message
	for _, m := range messages {
		if m.Role == "system" {
			system = append(system, m)
		} else {
			convo = append(convo, m)
		}
	}
	if len(convo) <= c.cfg.Compression.SummarizeAfterTurns {
		return messages
	}

	keep := c.cfg.Compression.KeepRecentTurns
	if keep < 2 {
		keep = 2
	}
	archive := convo[:len(convo)-keep]
	recent := convo[len(convo)-keep:]

	// Expand keep window to never split a tool call sequence at the boundary.
	// Walk backwards from the split point and pull any tool-related turns into recent.
	for len(archive) > 0 && isToolMessage(archive[len(archive)-1]) {
		recent = append([]Message{archive[len(archive)-1]}, recent...)
		archive = archive[:len(archive)-1]
	}
	if len(archive) == 0 {
		return messages
	}

	lines := make([]string, 0, len(archive))
	for _, m := range archive {
		if isToolMessage(m) {
			// Keep tool call/result lines as opaque JSON so the summary is not misleading.
			lines = append(lines, fmt.Sprintf("[tool] %s", truncateRunes(string(m.Content), 200)))
			continue
		}
		role := "A"
		if m.Role == "user" {
			role = "U"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s", role, truncateRunes(m.Text(), 450)))
	}

	summaryText := fmt.Sprintf(
		"[Context summary — %d earlier messages condensed]\n\n%s\n\n[End of summary — current session continues below]",
		len(archive), strings.Join(lines, "\n"),
	)
	cb, _ := json.Marshal(summaryText)
	summary := Message{Role: "user", Content: cb, Extra: map[string]json.RawMessage{}}

	out := make([]Message, 0, len(system)+1+len(recent))
	out = append(out, system...)
	out = append(out, summary)
	out = append(out, recent...)
	return out
}
