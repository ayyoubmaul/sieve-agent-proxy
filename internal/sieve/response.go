package sieve

import "encoding/json"

// Response-side trimming (Phase 4). A proxy can't reduce the output tokens the
// model already generated — they're billed at generation time — but in an agent
// loop the assistant's reply is appended to the conversation and re-sent as
// *input* next turn. Trimming the reply before returning it therefore shrinks
// future input tokens (and what the client stores/displays), compounding over a
// long session.
//
// Safety is the whole game here. This reuses normalizeWhitespace — the same
// code-safe cleanup sieve applies to inputs (CRLF→LF, trailing-space strip,
// collapse 3+ blank lines, trim) — which leaves indentation and inline spacing
// untouched, so source code is never altered. It only ever rewrites plain
// assistant *text*: tool calls, structured output, thinking blocks, and token
// usage are passed through byte-for-byte. Any unrecognised shape returns the
// original bytes unchanged. It runs on the non-streamed path only — trimming a
// live stream would require buffering the whole response and defeat streaming.

// trimResponse applies code-safe whitespace trimming to a non-streamed response
// body when the policy is enabled, else returns it unchanged.
func (s *Server) trimResponse(body []byte, isAnthropic bool) []byte {
	if !s.cfg.Output.Enabled || !s.cfg.Output.TrimResponse {
		return body
	}
	if isAnthropic {
		return trimAnthropicResponse(body)
	}
	return trimOpenAIResponse(body)
}

func trimAnthropicResponse(body []byte) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	raw, ok := obj["content"]
	if !ok {
		return body
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return body
	}

	changed := false
	for i := range blocks {
		var typ string
		_ = json.Unmarshal(blocks[i]["type"], &typ)
		if typ != "text" { // never touch tool_use, thinking, etc.
			continue
		}
		var txt string
		if json.Unmarshal(blocks[i]["text"], &txt) != nil {
			continue
		}
		if nt := normalizeWhitespace(txt); nt != txt {
			blocks[i]["text"], _ = json.Marshal(nt)
			changed = true
		}
	}
	if !changed {
		return body
	}

	obj["content"], _ = json.Marshal(blocks)
	if out, err := json.Marshal(obj); err == nil {
		return out
	}
	return body
}

func trimOpenAIResponse(body []byte) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	raw, ok := obj["choices"]
	if !ok {
		return body
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(raw, &choices) != nil {
		return body
	}

	changed := false
	for i := range choices {
		msgRaw, ok := choices[i]["message"]
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal(msgRaw, &msg) != nil {
			continue
		}
		// content may be null (tool-only reply) or an array (multimodal) — both
		// fail the string unmarshal and are skipped, leaving tool_calls intact.
		var txt string
		if json.Unmarshal(msg["content"], &txt) != nil {
			continue
		}
		if nt := normalizeWhitespace(txt); nt != txt {
			msg["content"], _ = json.Marshal(nt)
			choices[i]["message"], _ = json.Marshal(msg)
			changed = true
		}
	}
	if !changed {
		return body
	}

	obj["choices"], _ = json.Marshal(choices)
	if out, err := json.Marshal(obj); err == nil {
		return out
	}
	return body
}
