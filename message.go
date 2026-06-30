package main

import (
	"encoding/json"
	"strings"
)

// Message represents a single chat message in either the OpenAI or Anthropic
// wire format. Content is kept as raw JSON because it may be a plain string or
// an array of typed content blocks. Extra preserves any other per-message
// fields (e.g. tool_calls, tool_call_id, name) so nothing is lost on re-encode.
type Message struct {
	Role    string
	Content json.RawMessage
	Extra   map[string]json.RawMessage
}

// UnmarshalJSON flattens role + content out and stashes everything else in Extra.
func (m *Message) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if r, ok := raw["role"]; ok {
		_ = json.Unmarshal(r, &m.Role)
		delete(raw, "role")
	}
	if c, ok := raw["content"]; ok {
		m.Content = c
		delete(raw, "content")
	}
	m.Extra = raw
	return nil
}

// MarshalJSON re-combines role, content, and any preserved extra fields.
func (m Message) MarshalJSON() ([]byte, error) {
	out := map[string]json.RawMessage{}
	for k, v := range m.Extra {
		out[k] = v
	}
	rb, _ := json.Marshal(m.Role)
	out["role"] = rb
	if m.Content != nil {
		out["content"] = m.Content
	}
	return json.Marshal(out)
}

// Text returns the concatenated text content, regardless of whether content is
// a plain string or an array of blocks. Non-text blocks (images, tool results)
// are ignored for sizing/compression purposes.
func (m Message) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			var typ string
			if t, ok := b["type"]; ok {
				_ = json.Unmarshal(t, &typ)
			}
			if typ == "text" {
				var txt string
				if tx, ok := b["text"]; ok {
					_ = json.Unmarshal(tx, &txt)
				}
				sb.WriteString(txt)
			}
		}
		return sb.String()
	}
	return ""
}
