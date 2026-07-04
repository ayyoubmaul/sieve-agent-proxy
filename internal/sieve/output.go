package sieve

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Output-token reduction (request-side). A proxy cannot un-generate tokens —
// they bill the moment the model writes them — so the only lever for reducing
// output cost is to shape the request so the model generates less. This file
// implements three such levers, each independently gated and all skipped unless
// OUTPUT_POLICY is on:
//
//	1. max_tokens clamp      — caps a runaway output ceiling (guardrail).
//	2. reasoning downgrade   — lowers thinking/effort (thinking bills as output).
//	3. conciseness directive — asks the model to drop ceremony/restated code.
//
// Every lever leaves a request shape it doesn't recognise untouched, so a
// misconfiguration degrades to a no-op rather than an upstream 400. Injections
// are deterministic and appended (never inserted ahead of cached content), so a
// client's prompt-cache prefix and cache_control breakpoints are preserved.

const (
	conciseDirective = "Be concise. Skip preamble and postamble, don't restate the question, " +
		"and don't repeat unchanged code back. Return only what was asked."
	terseDirective = "Respond with the minimum text needed. No preamble, no postamble, no summary, " +
		"no restating the question or unchanged code. When editing code, show only the changed lines " +
		"or a diff — never reproduce whole unchanged files. No apologies or filler."
)

// effortRank orders effort levels so the policy can compare and only lower them.
var effortRank = map[string]int{"low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}

// applyOutputPolicy rewrites the outbound request body in place to reduce
// generated output tokens. Called only when cfg.Output.Enabled.
func (s *Server) applyOutputPolicy(body map[string]json.RawMessage, isAnthropic bool) {
	pol := s.cfg.Output

	if pol.MaxOutputTokens > 0 {
		clampMaxTokens(body, isAnthropic, pol.MaxOutputTokens)
	}
	if pol.Effort != "" && pol.ReasoningProfile != "off" {
		downgradeEffort(body, isAnthropic, pol.ReasoningProfile, pol.Effort)
	}
	if pol.TrimOutput {
		injectDirective(body, isAnthropic, directiveText(pol.Style, pol.Directive))
	}
}

// ── Lever 1: max_tokens clamp ────────────────────────────────────────────

// clampMaxTokens lowers the per-response output ceiling. It never raises it and
// never injects a ceiling the client omitted — injecting one risks silent
// truncation, and picking the wrong field on an OpenAI reasoning model 400s.
func clampMaxTokens(body map[string]json.RawMessage, isAnthropic bool, limit int) {
	clamp := func(field string) bool {
		raw, ok := body[field]
		if !ok {
			return false
		}
		var cur int
		if json.Unmarshal(raw, &cur) == nil && cur > limit {
			body[field], _ = json.Marshal(limit)
		}
		return true
	}
	if isAnthropic {
		clamp("max_tokens") // Anthropic requires max_tokens, so it's always present.
		return
	}
	// OpenAI: newer/reasoning models use max_completion_tokens (and reject
	// max_tokens). Clamp whichever the client sent; never inject one.
	if clamp("max_completion_tokens") {
		return
	}
	clamp("max_tokens")
}

// ── Lever 2: reasoning-effort downgrade ──────────────────────────────────

func downgradeEffort(body map[string]json.RawMessage, isAnthropic bool, profile, effort string) {
	if _, known := effortRank[effort]; !known {
		return
	}
	if profile == "auto" {
		// Derive from the wire format. Still assumes the upstream model accepts
		// the field; use an explicit profile when the target is anything else.
		if isAnthropic {
			profile = "anthropic"
		} else {
			profile = "openai"
		}
	}
	switch profile {
	case "anthropic":
		// Claude effort lives at output_config.effort (GA, no beta header).
		setEffortField(body, "output_config", effort, true)
	case "openai":
		// OpenAI-family reasoning models use a top-level reasoning_effort.
		setEffortField(body, "reasoning_effort", effort, false)
	}
}

// setEffortField sets an effort value, downgrade-only. When nested is true the
// value lives under body[key].effort (Anthropic's output_config object);
// otherwise body[key] is the effort string directly (OpenAI's reasoning_effort).
// If the request already specifies an effort at or below the target, it is left
// as-is — the policy never raises effort.
func setEffortField(body map[string]json.RawMessage, key, effort string, nested bool) {
	want := effortRank[effort]

	if nested {
		obj := map[string]json.RawMessage{}
		if raw, ok := body[key]; ok {
			// Preserve any sibling fields the client set (e.g. task_budget).
			// If it isn't an object we can't safely merge, so leave it alone.
			if json.Unmarshal(raw, &obj) != nil {
				return
			}
		}
		if cur, ok := obj["effort"]; ok && !aboveTarget(cur, want) {
			return
		}
		obj["effort"], _ = json.Marshal(effort)
		body[key], _ = json.Marshal(obj)
		return
	}

	if cur, ok := body[key]; ok && !aboveTarget(cur, want) {
		return
	}
	body[key], _ = json.Marshal(effort)
}

// aboveTarget reports whether the current effort (raw JSON string) is a known
// level strictly above the target rank — i.e. whether downgrading it is a real
// reduction. An unknown/absent current value returns true so the policy applies
// (the model's default effort is at least as high as any level we'd set).
func aboveTarget(cur json.RawMessage, want int) bool {
	var curStr string
	if json.Unmarshal(cur, &curStr) != nil {
		return true
	}
	r, known := effortRank[curStr]
	if !known {
		return true
	}
	return r > want
}

// ── Lever 3: conciseness directive ───────────────────────────────────────

func directiveText(style, custom string) string {
	if custom != "" {
		return custom
	}
	if style == "terse" {
		return terseDirective
	}
	return conciseDirective
}

// injectDirective adds a conciseness instruction without disturbing the cached
// prompt prefix: for Anthropic it appends a trailing block to the top-level
// `system` (after any client cache_control breakpoint); for OpenAI it appends a
// trailing system message after the conversation. Appending — rather than
// inserting ahead of existing content — keeps the client's cached prefix and
// breakpoint hashes byte-identical.
func injectDirective(body map[string]json.RawMessage, isAnthropic bool, directive string) {
	if directive == "" {
		return
	}
	if isAnthropic {
		injectAnthropicSystem(body, directive)
	} else {
		injectOpenAISystem(body, directive)
	}
}

func injectAnthropicSystem(body map[string]json.RawMessage, directive string) {
	block, _ := json.Marshal(map[string]string{"type": "text", "text": directive})

	raw, ok := body["system"]
	if !ok || len(raw) == 0 {
		body["system"], _ = json.Marshal([]json.RawMessage{block})
		return
	}
	// String form → append as text.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		body["system"], _ = json.Marshal(s + "\n\n" + directive)
		return
	}
	// Block-array form → append a trailing block (after any cache_control).
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) == nil {
		body["system"], _ = json.Marshal(append(blocks, block))
		return
	}
	// Unknown shape: leave untouched.
}

func injectOpenAISystem(body map[string]json.RawMessage, directive string) {
	raw, ok := body["messages"]
	if !ok {
		return
	}
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return
	}
	m, _ := json.Marshal(map[string]string{"role": "system", "content": directive})
	body["messages"], _ = json.Marshal(append(msgs, m))
}

// outputSummary renders the active output-policy levers for the startup banner.
func (cfg *Config) outputSummary() string {
	if !cfg.Output.Enabled {
		return "❌ off"
	}
	var parts []string
	if cfg.Output.Effort != "" && cfg.Output.ReasoningProfile != "off" {
		parts = append(parts, fmt.Sprintf("effort=%s(%s)", cfg.Output.Effort, cfg.Output.ReasoningProfile))
	}
	if cfg.Output.MaxOutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("max_tokens≤%d", cfg.Output.MaxOutputTokens))
	}
	if cfg.Output.TrimOutput {
		parts = append(parts, "trim="+cfg.Output.Style)
	}
	if cfg.Output.TrimResponse {
		parts = append(parts, "trim-resp")
	}
	if len(parts) == 0 {
		return "✅ on (measure only)"
	}
	return "✅ " + strings.Join(parts, " · ")
}
