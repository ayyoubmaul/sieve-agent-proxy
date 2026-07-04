package sieve

import (
	"bytes"
	"encoding/json"
)

// PromptAlign — provider-side prompt-cache optimizer.
//
// The L1/L1n/L2 caches in cache.go save an upstream *call* by replaying a stored
// response. PromptAlign is the complementary lever: it shapes the *outbound*
// request so the provider's own prompt cache hits more often, discounting the
// input tokens sieve still has to send. It is the primary saver when the
// response cache is disabled.
//
// Three deterministic, independently-gated transforms run in order:
//
//  1. NORMALIZE — byte-stabilize the prefix. Re-marshalling through
//     encoding/json sorts object keys, and normalizeWhitespace (CRLF→LF,
//     trailing-space strip, blank-line collapse — already code-safe) cleans
//     every text block. Two requests that differ only in incidental whitespace
//     or key order then produce identical bytes, so the provider sees the same
//     prefix.
//
//  2. REORDER — hoist a leading run of system content to the front (OpenAI only;
//     Anthropic's system is already a top-level, prompt-leading field). Done
//     conservatively: if a system message sits mid-conversation it is left in
//     place, because moving it could change meaning.
//
//  3. INJECT — add Anthropic cache_control breakpoints, but ONLY when the client
//     set none (client intent always wins). Breakpoints mark the boundary
//     between the stable prefix and the volatile latest turn: end of tools, end
//     of system, and the last stable history message — never the newest user
//     message (it changes every turn) and never a tool_result block.
//
// Every transform leaves a shape it doesn't recognise untouched, so a
// misconfiguration or novel wire format degrades to a no-op rather than a
// corrupted request.

// alignResult reports what PromptAlign did, for stats.
type alignResult struct {
	applied     bool
	breakpoints int
}

// ephemeralCacheControl is the cache_control value Anthropic uses to mark a
// prompt-cache breakpoint.
var ephemeralCacheControl = json.RawMessage(`{"type":"ephemeral"}`)

// applyPromptAlign rewrites the outbound request body in place to raise the
// provider's prompt-cache hit rate. Called only when cfg.PromptAlign.Enabled.
// Returns whether the upstream beta header should be set (Anthropic injection).
func (s *Server) applyPromptAlign(body map[string]json.RawMessage, isAnthropic bool) alignResult {
	if isAnthropic {
		return s.alignAnthropic(body)
	}
	return s.alignOpenAI(body)
}

// ── Anthropic ─────────────────────────────────────────────────────────────

func (s *Server) alignAnthropic(body map[string]json.RawMessage) alignResult {
	pa := s.cfg.PromptAlign
	res := alignResult{}

	if pa.Normalize {
		if raw, ok := body["system"]; ok {
			body["system"] = normalizeAlignContent(raw)
		}
		if raw, ok := body["messages"]; ok {
			body["messages"] = normalizeMessagesRaw(raw)
		}
	}

	// Conservative injection: only when the client set no breakpoints anywhere.
	if pa.Inject && pa.MaxBreakpoints > 0 && !clientHasCacheControl(body) {
		res.breakpoints = injectAnthropicBreakpoints(body, pa.MaxBreakpoints)
	}

	res.applied = pa.Normalize || res.breakpoints > 0
	return res
}

// clientHasCacheControl reports whether the request already carries any
// cache_control marker in system, tools, or messages. If so, the client manages
// its own breakpoints and PromptAlign must not inject (avoids double-marking and
// blowing Anthropic's 4-breakpoint budget).
func clientHasCacheControl(body map[string]json.RawMessage) bool {
	for _, k := range []string{"system", "tools", "messages"} {
		if raw, ok := body[k]; ok && bytes.Contains(raw, []byte(`"cache_control"`)) {
			return true
		}
	}
	return false
}

// injectAnthropicBreakpoints marks up to max boundaries with an ephemeral
// cache_control and returns the count injected. Boundaries, in prefix order:
//
//  1. last entry of tools
//  2. last block of system
//  3. last content block of the last *stable* message (not the newest user turn)
//
// A breakpoint caches everything from the start of the prompt through the marked
// block, so marking the boundary just before the volatile latest turn maximizes
// the cached prefix while leaving the changing tail uncached.
func injectAnthropicBreakpoints(body map[string]json.RawMessage, max int) int {
	injected := 0

	// 1. tools: mark the last tool definition.
	if injected < max {
		if raw, ok := body["tools"]; ok {
			if marked, ok := markLastArrayElem(raw); ok {
				body["tools"] = marked
				injected++
			}
		}
	}

	// 2. system: mark the last system block (array form only; a plain-string
	//    system can't carry cache_control, so it's promoted to a one-block array).
	if injected < max {
		if raw, ok := body["system"]; ok {
			if marked, ok := markSystem(raw); ok {
				body["system"] = marked
				injected++
			}
		}
	}

	// 3. messages: mark the last stable message (skip the newest user turn).
	if injected < max {
		if raw, ok := body["messages"]; ok {
			if marked, ok := markLastStableMessage(raw); ok {
				body["messages"] = marked
				injected++
			}
		}
	}

	return injected
}

// markLastArrayElem adds cache_control to the last object in a JSON array.
// Returns false (and the input unchanged) for non-arrays or empty arrays.
func markLastArrayElem(raw json.RawMessage) (json.RawMessage, bool) {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil || len(arr) == 0 {
		return raw, false
	}
	marked, ok := withCacheControl(arr[len(arr)-1])
	if !ok {
		return raw, false
	}
	arr[len(arr)-1] = marked
	out, err := json.Marshal(arr)
	if err != nil {
		return raw, false
	}
	return out, true
}

// markSystem marks the last block of an Anthropic system field. A plain string
// is promoted to a single text block so it can carry the breakpoint.
func markSystem(raw json.RawMessage) (json.RawMessage, bool) {
	// String form → promote to one text block with cache_control.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		block := map[string]json.RawMessage{
			"type":          json.RawMessage(`"text"`),
			"text":          mustMarshal(s),
			"cache_control": ephemeralCacheControl,
		}
		out, err := json.Marshal([]map[string]json.RawMessage{block})
		if err != nil {
			return raw, false
		}
		return out, true
	}
	// Block-array form → mark the last block.
	return markLastArrayElem(raw)
}

// markLastStableMessage marks the last message that is not the newest user turn.
// The newest user message is the volatile tail (it changes every turn), so
// marking it would cache nothing useful; we mark the message before it instead.
// For a block-array content the last block is marked; a plain-string content is
// promoted to a one-text-block array.
func markLastStableMessage(raw json.RawMessage) (json.RawMessage, bool) {
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil || len(msgs) < 2 {
		return raw, false
	}

	// The last element is the current turn; the stable boundary is the one
	// before it. Walking back also skips any trailing user message.
	idx := len(msgs) - 2
	for idx >= 0 {
		if marked, ok := markMessageContent(msgs[idx]); ok {
			msgs[idx] = marked
			out, err := json.Marshal(msgs)
			if err != nil {
				return raw, false
			}
			return out, true
		}
		idx--
	}
	return raw, false
}

// markMessageContent adds cache_control to the last content block of a single
// message. Refuses tool_result blocks (Anthropic disallows caching them) by
// marking the last non-tool_result block; if every block is a tool_result the
// message is left unmarked.
func markMessageContent(raw json.RawMessage) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return raw, false
	}
	content, ok := msg["content"]
	if !ok {
		return raw, false
	}

	// String content → promote to a single text block carrying the breakpoint.
	var s string
	if json.Unmarshal(content, &s) == nil {
		block := map[string]json.RawMessage{
			"type":          json.RawMessage(`"text"`),
			"text":          mustMarshal(s),
			"cache_control": ephemeralCacheControl,
		}
		msg["content"], _ = json.Marshal([]map[string]json.RawMessage{block})
		out, err := json.Marshal(msg)
		if err != nil {
			return raw, false
		}
		return out, true
	}

	// Block-array content → mark the last non-tool_result block.
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
		return raw, false
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if rawBlockType(blocks[i]) == "tool_result" {
			continue
		}
		marked, ok := withCacheControl(blocks[i])
		if !ok {
			return raw, false
		}
		blocks[i] = marked
		msg["content"], _ = json.Marshal(blocks)
		out, err := json.Marshal(msg)
		if err != nil {
			return raw, false
		}
		return out, true
	}
	return raw, false
}

// withCacheControl sets cache_control on a JSON object, preserving its other
// fields. Returns false for non-objects.
func withCacheControl(raw json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return raw, false
	}
	obj["cache_control"] = ephemeralCacheControl
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, false
	}
	return out, true
}

// rawBlockType returns the "type" field of a raw JSON content block, or "" if
// absent. (blockType in toolresult.go takes an already-decoded map; this variant
// works on the raw bytes PromptAlign carries.)
func rawBlockType(raw json.RawMessage) string {
	var b struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &b)
	return b.Type
}

// ── OpenAI ──────────────────────────────────────────────────────────────

func (s *Server) alignOpenAI(body map[string]json.RawMessage) alignResult {
	pa := s.cfg.PromptAlign
	res := alignResult{}

	raw, ok := body["messages"]
	if !ok {
		return res
	}
	var msgs []Message
	if json.Unmarshal(raw, &msgs) != nil {
		return res
	}

	changed := false
	if pa.Reorder {
		if hoisted, ok := hoistLeadingSystem(msgs); ok {
			msgs = hoisted
			changed = true
		}
	}
	if pa.Normalize {
		for i := range msgs {
			if nc := normalizeAlignContent(msgs[i].Content); !bytes.Equal(nc, msgs[i].Content) {
				msgs[i].Content = nc
				changed = true
			}
		}
	}

	if changed {
		if out, err := json.Marshal(msgs); err == nil {
			body["messages"] = out
			res.applied = true
		}
	}
	return res
}

// hoistLeadingSystem moves a leading run of system messages to the front,
// preserving their relative order. It only acts when every system message is
// already part of that leading run (i.e. none sit mid-conversation) — a system
// message placed deliberately later is left untouched, since relocating it could
// change behavior. Returns false when nothing needed moving.
func hoistLeadingSystem(msgs []Message) ([]Message, bool) {
	// Find all system indices.
	var sysIdx []int
	for i, m := range msgs {
		if m.Role == "system" {
			sysIdx = append(sysIdx, i)
		}
	}
	if len(sysIdx) == 0 {
		return msgs, false
	}
	// If the system messages are already a contiguous prefix, nothing to do.
	contiguousPrefix := true
	for i, idx := range sysIdx {
		if idx != i {
			contiguousPrefix = false
			break
		}
	}
	if contiguousPrefix {
		return msgs, false
	}
	// A system message sits mid-conversation. Conservative: do not reorder.
	return msgs, false
}

// ── Shared normalization ───────────────────────────────────────────────────

// normalizeMessagesRaw applies normalizeAlignContent to every message's content
// in a raw messages array, re-marshalling deterministically. Non-arrays pass
// through unchanged.
func normalizeMessagesRaw(raw json.RawMessage) json.RawMessage {
	var msgs []Message
	if json.Unmarshal(raw, &msgs) != nil {
		return raw
	}
	for i := range msgs {
		msgs[i].Content = normalizeAlignContent(msgs[i].Content)
	}
	out, err := json.Marshal(msgs)
	if err != nil {
		return raw
	}
	return out
}

// normalizeAlignContent byte-stabilizes a content field: it strips code-safe
// whitespace from every text leaf (plain-string or block-array form) and
// re-marshals so keys are deterministically ordered. This is the same two-shape
// handling normalizeContent (compress.go) and normalizeContentForKey (cache.go)
// use, kept separate so PromptAlign can normalize independently of compression.
func normalizeAlignContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	// Plain-string form.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return mustMarshal(normalizeWhitespace(s))
	}
	// Block-array form: normalize each block's text, re-marshal (sorts keys).
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) == nil {
		for i := range blocks {
			if tx, ok := blocks[i]["text"]; ok {
				var txt string
				if json.Unmarshal(tx, &txt) == nil {
					blocks[i]["text"] = mustMarshal(normalizeWhitespace(txt))
				}
			}
		}
		if out, err := json.Marshal(blocks); err == nil {
			return out
		}
	}
	return raw
}

// mustMarshal marshals a value that is known to be encodable (a string), so the
// error is structurally impossible and dropped to keep call sites clean.
func mustMarshal(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
