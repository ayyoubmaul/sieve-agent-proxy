package main

import (
	"encoding/json"
	"testing"
)

// msg builds a Message from a role and a raw content JSON literal.
func msg(role, content string) Message {
	return Message{Role: role, Content: json.RawMessage(content), Extra: map[string]json.RawMessage{}}
}

// Two distinct Anthropic tool_result-only user turns must both survive dedup —
// they have empty Text() and collide on the "user\x00" key, but each pairs with a
// different tool_use and dropping either orphans that tool_use → Anthropic 400.
func TestDeduplicateKeepsAnthropicToolResults(t *testing.T) {
	c := NewCompressor(&Config{})
	in := []Message{
		msg("assistant", `[{"type":"tool_use","id":"a","name":"Read","input":{}}]`),
		msg("user", `[{"type":"tool_result","tool_use_id":"a","content":"file A"}]`),
		msg("assistant", `[{"type":"tool_use","id":"b","name":"Read","input":{}}]`),
		msg("user", `[{"type":"tool_result","tool_use_id":"b","content":"file B"}]`),
	}
	out := c.deduplicate(in)
	if len(out) != len(in) {
		t.Fatalf("dedup dropped a tool turn: got %d messages, want %d (orphans a tool_use → 400)", len(out), len(in))
	}
	// Both tool_use_ids must still be present and paired.
	for _, id := range []string{"a", "b"} {
		found := false
		for _, m := range out {
			if containsToolBlock(m.Content) && string(m.Content) != "" &&
				jsonContains(m.Content, `"tool_use_id":"`+id+`"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("tool_result for id %q was dropped", id)
		}
	}
}

// Genuine non-tool duplicates should still be collapsed.
func TestDeduplicateStillDropsPlainDuplicates(t *testing.T) {
	c := NewCompressor(&Config{})
	in := []Message{
		msg("user", `"same question"`),
		msg("user", `"same question"`),
	}
	if out := c.deduplicate(in); len(out) != 1 {
		t.Fatalf("expected plain duplicate collapsed to 1, got %d", len(out))
	}
}

func jsonContains(raw json.RawMessage, sub string) bool {
	return len(raw) > 0 && containsSub(string(raw), sub)
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
