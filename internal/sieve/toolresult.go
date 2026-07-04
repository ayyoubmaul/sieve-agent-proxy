package sieve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Tool-result compaction.
//
// Coding agents resend the entire conversation on every turn, and the heaviest,
// least information-dense part of it is tool output: bloated JSON where only a
// few fields matter, log spew with the same line repeated hundreds of times,
// SQL/table dumps of near-identical rows. The generic text compressor in
// compress.go deliberately ignores all of this — Message.Text() skips
// tool_result blocks entirely — so it passes through verbatim and is re-billed
// in full on every subsequent turn.
//
// This file compacts that output. Two properties are load-bearing:
//
//  1. DETERMINISM. Every compactor is a pure function of the input bytes: the
//     same tool result always compacts to exactly the same output, independent
//     of conversation state or wall-clock. That keeps each already-seen tool
//     result byte-identical across turns, which preserves the Anthropic
//     prompt-cache prefix a growing agent conversation depends on for its
//     discount. Compaction that varied per turn would bust the very cache we're
//     trying to help. (Go's encoding/json marshals map keys in sorted order and
//     json.Number round-trips number text exactly, so re-encoded JSON is stable.)
//
//  2. SIZE-GATING. Results at or below MinBytes are returned byte-identical, so
//     small/normal tool output is never altered — only genuinely bloated output
//     pays the (tiny) cost of being reshaped.
//
// The design choice for JSON is to preserve every key and the full structure
// and only elide bulky *values* (long strings, large arrays). Dropping keys
// would save little — the bloat lives in values, not key names — and risks
// discarding the one field the model needs. Intent-driven key selection is a
// separate, opt-in layer built on the retrieval store, not done blindly here.

// compactOpts tunes the compaction heuristics. Build one with defaultCompactOpts().
type compactOpts struct {
	MinBytes       int // only compact tool results larger than this (bytes)
	MaxStringRunes int // truncate JSON string leaves longer than this (runes)
	MaxArrayItems  int // cap JSON arrays longer than this
	HeadItems      int // JSON array: items kept from the front
	TailItems      int // JSON array: items kept from the back
	MaxLines       int // cap line-oriented text longer than this (lines)
	HeadLines      int // lines/rows kept from the front
	TailLines      int // lines/rows kept from the back

	// Intent terms (lowercased) bias what is kept: JSON values under a matching
	// key are kept fuller, and log lines / table rows matching a term are
	// protected from elision. Empty = pure structural compaction. Set per-request
	// (transient), never from config; stability across turns comes from pinning.
	Intent []string
}

// markerFetchToolDefault is the tool name written into a compaction marker when
// the context carries none. It is the name the model must call to fetch an
// original. An MCP host prefixes the server name onto the bare "fetch" tool, so
// a server named "sieve" surfaces it as "sieve_fetch" — the default here.
const markerFetchToolDefault = "sieve_fetch"

func defaultCompactOpts() compactOpts {
	return compactOpts{
		MinBytes:       1024,
		MaxStringRunes: 512,
		MaxArrayItems:  24,
		HeadItems:      12,
		TailItems:      6,
		MaxLines:       60,
		HeadLines:      30,
		TailLines:      15,
	}
}

// compactToolText compacts a single tool result's text payload. It returns the
// (possibly shrunk) text and whether it changed. Inputs at or below MinBytes,
// and inputs it cannot actually shrink, are returned byte-identical with
// changed=false.
//
// When ctx.store is set (retrieval), the original is saved under a
// content-addressed ref and the output is prefixed with a marker advertising
// sieve_fetch(ref). When ctx.pin is set (intent mode), the compacted bytes for a
// given original are frozen on first computation and reused verbatim thereafter,
// so intent-shaped output stays byte-stable across turns.
func compactToolText(s string, opt compactOpts, ctx *compactCtx) (string, bool) {
	if len(s) <= opt.MinBytes {
		return s, false
	}

	// Pin lookup: reuse the frozen bytes for this original if we've seen it.
	var pinKey string
	if ctx != nil && ctx.pin != nil {
		pinKey = refOf([]byte(s))
		if pinned, ok := ctx.pin.get(pinKey); ok {
			return pinned, pinned != s
		}
	}

	out := compactByKind(s, opt)
	if out == s || len(out) >= len(s) {
		if pinKey != "" {
			ctx.pin.put(pinKey, s) // freeze the no-op too, so it stays stable
		}
		return s, false
	}
	if ctx != nil && ctx.store != nil {
		if ref, err := ctx.store.Put(s); err == nil {
			fetchTool := markerFetchToolDefault
			if ctx.fetchTool != "" {
				fetchTool = ctx.fetchTool
			}
			marked := fmt.Sprintf(
				"[sieve compacted %dB→%dB · call %s(\"%s\") for the full original]\n%s",
				len(s), len(out), fetchTool, ref, out)
			if len(marked) < len(s) { // keep the marker only while it still pays
				out = marked
			}
		}
	}
	if pinKey != "" {
		ctx.pin.put(pinKey, out)
	}
	return out, true
}

// compactByKind dispatches on the detected content kind. JSON is tried first
// (cheap structural check + a real parse); everything else is treated as
// line-oriented text, which internally splits into the table and log paths.
func compactByKind(s string, opt compactOpts) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		if out, ok := compactJSON(trimmed, opt); ok {
			return out
		}
	}
	return compactLines(s, opt)
}

// ── JSON ─────────────────────────────────────────────────────────────────

// compactJSON parses a single JSON value, elides bulky leaves, and re-encodes
// compactly. Returns ok=false if the input isn't exactly one JSON value, so the
// caller can fall back to text handling. UseNumber keeps numeric tokens exact;
// sorted map-key encoding plus exact numbers make the output deterministic.
func compactJSON(s string, opt compactOpts) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	if dec.More() { // trailing tokens → not a single JSON value (e.g. NDJSON)
		return "", false
	}

	v = compactValue(v, opt)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // avoid < bloat; keep bytes clean & stable
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

// compactValue recursively elides long strings and large arrays while keeping
// all object keys and the overall structure intact.
func compactValue(v interface{}, opt compactOpts) interface{} {
	switch t := v.(type) {
	case string:
		return truncateStringValue(t, opt.MaxStringRunes)
	case []interface{}:
		return compactArray(t, opt)
	case map[string]interface{}:
		for k, val := range t {
			// A string value under a key the user asked about is kept much fuller
			// (still bounded); everything else is elided at the normal limit.
			if sv, ok := val.(string); ok {
				limit := opt.MaxStringRunes
				if matchAnyTerm(k, opt.Intent) {
					limit *= 6
				}
				t[k] = truncateStringValue(sv, limit)
			} else {
				t[k] = compactValue(val, opt)
			}
		}
		return t
	default:
		return v // numbers (json.Number), bool, nil
	}
}

// compactArray recurses into elements, then — if the array is longer than the
// cap — keeps a head and tail slice with a marker element in between. The marker
// is short and the kept count stays under the cap, so re-compacting is a no-op.
func compactArray(arr []interface{}, opt compactOpts) interface{} {
	for i := range arr {
		arr[i] = compactValue(arr[i], opt)
	}
	if len(arr) <= opt.MaxArrayItems || opt.HeadItems+opt.TailItems >= len(arr) {
		return arr
	}
	omitted := len(arr) - opt.HeadItems - opt.TailItems
	out := make([]interface{}, 0, opt.HeadItems+opt.TailItems+1)
	out = append(out, arr[:opt.HeadItems]...)
	out = append(out, fmt.Sprintf("…[%d more items]…", omitted))
	out = append(out, arr[len(arr)-opt.TailItems:]...)
	return out
}

// truncateStringValue cuts a string to maxRunes runes (never mid-codepoint) and
// appends a count of what was elided. It reserves room for the marker so the
// result stays within maxRunes — making truncation idempotent (a second pass
// finds the value already short enough and leaves it alone), which is required
// for byte-stable output across turns.
func truncateStringValue(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	const reserve = 24 // upper bound on the "…[+NNNNNNN chars]" marker
	head := maxRunes - reserve
	if head < 1 {
		head = 1
	}
	return string(r[:head]) + fmt.Sprintf("…[+%d chars]", len(r)-head)
}

// ── Applying compaction to a message array ─────────────────────────────────

// compactToolResults walks a message array and compacts every tool result it
// finds, in both wire formats:
//
//   - Anthropic: tool_result content blocks inside (user) messages. Each block's
//     `content` may be a string or an array of text/image blocks.
//   - OpenAI: whole messages with role "tool", whose content is a string (or an
//     array of text blocks).
//
// Only tool-result text is touched. Block order, non-text blocks, and every
// other field are preserved byte-for-byte. The input slice and its messages are
// not mutated; changed messages are rebuilt. Returns the new slice and the total
// characters saved.
func compactToolResults(messages []Message, opt compactOpts, ctx *compactCtx) ([]Message, int) {
	out := make([]Message, len(messages))
	copy(out, messages)
	total := 0

	for i := range out {
		m := out[i]

		// OpenAI tool message: content is the result payload directly.
		if m.Role == "tool" {
			if nb, saved := compactResultPayload(m.Content, opt, ctx); saved > 0 {
				m.Content = nb
				out[i] = m
				total += saved
			}
			continue
		}

		// Anthropic: look for tool_result blocks within the content array.
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		changed := false
		for bi := range blocks {
			if blockType(blocks[bi]) != "tool_result" {
				continue
			}
			if c, ok := blocks[bi]["content"]; ok {
				if nb, saved := compactResultPayload(c, opt, ctx); saved > 0 {
					blocks[bi]["content"] = nb
					total += saved
					changed = true
				}
			}
		}
		if changed {
			if nb, err := json.Marshal(blocks); err == nil {
				m.Content = nb
				out[i] = m
			}
		}
	}
	return out, total
}

func blockType(block map[string]json.RawMessage) string {
	var typ string
	if t, ok := block["type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	return typ
}

// compactResultPayload compacts a tool-result content value, which may be a bare
// JSON string or an array of content blocks (compacting each text block). It
// returns the rewritten raw value and characters saved; saved=0 means unchanged
// and the original raw should be kept.
func compactResultPayload(raw json.RawMessage, opt compactOpts, ctx *compactCtx) (json.RawMessage, int) {
	// String form.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		out, changed := compactToolText(s, opt, ctx)
		if !changed {
			return raw, 0
		}
		nb, _ := json.Marshal(out)
		return nb, len(s) - len(out)
	}

	// Array-of-blocks form.
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) == nil {
		saved := 0
		for i := range blocks {
			if blockType(blocks[i]) != "text" {
				continue
			}
			var txt string
			if tx, ok := blocks[i]["text"]; ok && json.Unmarshal(tx, &txt) == nil {
				if out, changed := compactToolText(txt, opt, ctx); changed {
					nb, _ := json.Marshal(out)
					blocks[i]["text"] = nb
					saved += len(txt) - len(out)
				}
			}
		}
		if saved == 0 {
			return raw, 0
		}
		nb, _ := json.Marshal(blocks)
		return nb, saved
	}

	return raw, 0
}

// ── Line-oriented text (tables and logs) ───────────────────────────────────

func compactLines(s string, opt compactOpts) string {
	lines := strings.Split(s, "\n")
	if out, ok := compactTable(lines, opt); ok {
		return out
	}
	return collapseLines(lines, opt)
}

// compactTable detects delimiter-separated tabular output (markdown/psql tables,
// SQL result dumps, TSV) and keeps the header + first and last rows, eliding the
// middle. "First and last rows kept" matches what's usually wanted from a long
// result set: the boundary rows, with a count of how many were dropped.
func compactTable(lines []string, opt compactOpts) (string, bool) {
	nonEmpty, pipe, tab := 0, 0, 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		nonEmpty++
		if strings.Contains(ln, "|") {
			pipe++
		}
		if strings.Contains(ln, "\t") {
			tab++
		}
	}
	// Need a real result set, a strong (≥75%) column-separator signal, and more
	// rows than the cap (so the output is ≤ MaxLines and re-running is a no-op).
	if nonEmpty < 8 || len(lines) <= opt.MaxLines || (pipe*4 < nonEmpty*3 && tab*4 < nonEmpty*3) {
		return "", false
	}
	head, tail := opt.HeadLines, opt.TailLines
	if head+tail >= len(lines) {
		return "", false
	}
	middle := lines[head : len(lines)-tail]

	// Keep intent-matching rows from the middle (bounded so output ≤ MaxLines).
	maxKept := opt.MaxLines - head - tail - 1
	if maxKept < 0 {
		maxKept = 0
	}
	var kept []string
	for _, ln := range middle {
		if len(kept) >= maxKept {
			break
		}
		if matchAnyTerm(ln, opt.Intent) {
			kept = append(kept, ln)
		}
	}
	omitted := len(middle) - len(kept)
	if omitted <= 0 {
		return "", false
	}
	out := make([]string, 0, head+len(kept)+tail+1)
	out = append(out, lines[:head]...)
	out = append(out, kept...)
	out = append(out, fmt.Sprintf("…[%d rows omitted]…", omitted))
	out = append(out, lines[len(lines)-tail:]...)
	return strings.Join(out, "\n"), true
}

var (
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	reClock     = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b`)
	reUUID      = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reHex       = regexp.MustCompile(`0x[0-9a-fA-F]+|\b[0-9a-fA-F]{12,}\b`)
	// No word boundaries: digits glued to a unit (13ms, v2, users/42) must still
	// normalize, so two log lines differing only in such numbers share a shape.
	reNumber = regexp.MustCompile(`\d+`)
)

// lineShape normalizes the volatile tokens in a line (timestamps, ids, counters)
// so that lines which differ only in those tokens collapse to one shape. This is
// what lets repeated log lines — same message, different timestamp — be deduped.
func lineShape(s string) string {
	s = reTimestamp.ReplaceAllString(s, "<ts>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reClock.ReplaceAllString(s, "<ts>")
	s = reNumber.ReplaceAllString(s, "<n>")
	return strings.TrimSpace(s)
}

// collapseLines compacts log-like text in three passes: cap pathological single
// long lines, collapse consecutive runs of same-shape lines into one
// representative with a ×N count, then apply a global head/tail cap. Each pass
// is idempotent, so the whole function is.
func collapseLines(lines []string, opt compactOpts) string {
	maxLineRunes := opt.MaxStringRunes * 4

	// Pass 1: cap individual runaway lines (minified blobs, base64, etc.).
	capped := make([]string, len(lines))
	for i, ln := range lines {
		capped[i] = truncateStringValue(ln, maxLineRunes)
	}

	// Pass 2: collapse consecutive runs (3+) of lines sharing a shape.
	var collapsed []string
	for i := 0; i < len(capped); {
		shape := lineShape(capped[i])
		j := i + 1
		for shape != "" && j < len(capped) && lineShape(capped[j]) == shape {
			j++
		}
		if run := j - i; run >= 3 {
			collapsed = append(collapsed, fmt.Sprintf("%s …[×%d]", capped[i], run))
		} else {
			collapsed = append(collapsed, capped[i:j]...)
		}
		i = j
	}

	// Pass 3: global head/tail cap, keeping any intent-matching middle lines.
	if len(collapsed) > opt.MaxLines && opt.HeadLines+opt.TailLines < len(collapsed) {
		head, tail := opt.HeadLines, opt.TailLines
		middle := collapsed[head : len(collapsed)-tail]

		// Bound kept middle lines so the result stays ≤ MaxLines — that keeps the
		// pass idempotent (a re-run won't re-trigger the cap).
		maxKept := opt.MaxLines - head - tail - 1
		var kept []string
		for _, ln := range middle {
			if len(kept) >= maxKept {
				break
			}
			if matchAnyTerm(ln, opt.Intent) {
				kept = append(kept, ln)
			}
		}

		omitted := len(middle) - len(kept)
		out := make([]string, 0, head+len(kept)+tail+1)
		out = append(out, collapsed[:head]...)
		out = append(out, kept...)
		if omitted > 0 {
			out = append(out, fmt.Sprintf("…[%d lines omitted]…", omitted))
		}
		out = append(out, collapsed[len(collapsed)-tail:]...)
		collapsed = out
	}

	return strings.Join(collapsed, "\n")
}
