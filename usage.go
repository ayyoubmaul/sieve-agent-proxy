package main

import (
	"encoding/json"
	"strings"
)

// Output-token measurement (Phase 0) + provider prompt-cache measurement (Phase 1).
// Output tokens dominate cost, so the proxy reports what generation actually
// costs. It also reads the provider's prompt-cache fields (cache_read /
// cached_tokens) — the cache that *does* help agents (it reuses the stable
// prompt prefix across a growing conversation), which sieve's own response cache
// can't. Measurement is read-only: it never alters the bytes forwarded.

// tokenUsage is one response's token accounting. Cached is reported by the
// provider *separately* from In (not a subset): In counts only the fresh,
// uncached prompt tokens, while Cached is what the prompt cache served (billed
// at a steep discount). Total prompt size is In + Cached (+ cache-creation).
type tokenUsage struct {
	In     int
	Out    int
	Cached int
}

func (u tokenUsage) any() bool { return u.In > 0 || u.Out > 0 || u.Cached > 0 }

// recordUsage accumulates measured token counts into Stats.
func (s *Server) recordUsage(u tokenUsage) {
	s.incr(func(st *Stats) {
		st.TotalInputTokens += int64(u.In)
		st.TotalOutputTokens += int64(u.Out)
		st.TotalCachedTokens += int64(u.Cached)
	})
}

// usageFromResponse extracts token usage from a complete (non-streamed) body.
func usageFromResponse(body []byte, isAnthropic bool) tokenUsage {
	if isAnthropic {
		var r struct {
			Usage struct {
				InputTokens          int `json:"input_tokens"`
				OutputTokens         int `json:"output_tokens"`
				CacheReadInputTokens int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &r) == nil {
			return tokenUsage{r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheReadInputTokens}
		}
		return tokenUsage{}
	}
	var r struct {
		Usage openAIUsage `json:"usage"`
	}
	if json.Unmarshal(body, &r) == nil {
		return r.Usage.toTokenUsage()
	}
	return tokenUsage{}
}

// openAIUsage models the OpenAI-shaped usage block, including the two ways a
// gateway may report prompt-cache reads (nested prompt_tokens_details, or a
// top-level cache_read_tokens — some gateways emit both).
type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u openAIUsage) toTokenUsage() tokenUsage {
	cached := u.PromptTokensDetails.CachedTokens
	if cached == 0 {
		cached = u.CacheReadTokens
	}
	return tokenUsage{u.PromptTokens, u.CompletionTokens, cached}
}

// streamUsage scans a raw SSE transcript for token usage. Anthropic reports it
// across message_start/message_delta; OpenAI reports it in a final usage chunk
// (only when the client set stream_options.include_usage), else output is
// estimated from the streamed text at ~4 chars/token.
func streamUsage(raw string, isAnthropic bool) tokenUsage {
	var u tokenUsage
	var estChars int
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var d map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &d) != nil {
			continue
		}

		if isAnthropic {
			if msg, ok := d["message"]; ok {
				var m struct {
					Usage struct {
						InputTokens          int `json:"input_tokens"`
						OutputTokens         int `json:"output_tokens"`
						CacheReadInputTokens int `json:"cache_read_input_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal(msg, &m) == nil {
					u.In = max(u.In, m.Usage.InputTokens)
					u.Out = max(u.Out, m.Usage.OutputTokens)
					u.Cached = max(u.Cached, m.Usage.CacheReadInputTokens)
				}
			}
			if raw, ok := d["usage"]; ok {
				var us struct {
					InputTokens          int `json:"input_tokens"`
					OutputTokens         int `json:"output_tokens"`
					CacheReadInputTokens int `json:"cache_read_input_tokens"`
				}
				if json.Unmarshal(raw, &us) == nil {
					u.In = max(u.In, us.InputTokens)
					u.Out = max(u.Out, us.OutputTokens)
					u.Cached = max(u.Cached, us.CacheReadInputTokens)
				}
			}
			continue
		}

		if raw, ok := d["usage"]; ok && string(raw) != "null" {
			var us openAIUsage
			if json.Unmarshal(raw, &us) == nil {
				got := us.toTokenUsage()
				u.In = max(u.In, got.In)
				u.Out = max(u.Out, got.Out)
				u.Cached = max(u.Cached, got.Cached)
			}
		}
		if choices, ok := d["choices"]; ok {
			var chs []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			}
			if json.Unmarshal(choices, &chs) == nil {
				for _, c := range chs {
					estChars += len([]rune(c.Delta.Content))
				}
			}
		}
	}

	if !isAnthropic && u.Out == 0 && estChars > 0 {
		u.Out = (estChars + 3) / 4
	}
	return u
}
