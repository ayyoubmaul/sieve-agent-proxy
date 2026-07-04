package sieve

import (
	"net/http"
	"strings"
)

// Request-source attribution.
//
// Every call carries clues about who sent it and where it's going:
//   - the AGENT (Claude Code, OpenCode, Codex, …) identifies itself in headers —
//     usually User-Agent, sometimes an app marker like x-app.
//   - the PROVIDER (Anthropic, OpenAI, Google, …) is implied by the wire format
//     (Anthropic vs OpenAI path) and confirmed by the model name prefix.
//
// Neither is load-bearing for the proxy's behavior, but surfacing them turns the
// dashboard from "37 requests" into "37 requests — 28 from Claude Code, 9 from
// OpenCode", which is exactly the visibility the monitor is for.

// srcStat counts requests for one (agent, provider) pair.
type srcStat struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider"`
	Requests int64  `json:"requests"`
}

// detectAgent classifies the calling agent from request headers. Detection is
// best-effort: known agents are matched by signature, and anything else falls
// back to the User-Agent product token so an unrecognized client still shows a
// meaningful name instead of "unknown".
func detectAgent(r *http.Request) string {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	xapp := strings.ToLower(r.Header.Get("x-app"))

	switch {
	case strings.Contains(ua, "claude-code") || strings.Contains(ua, "claude-cli") || xapp == "cli":
		return "Claude Code"
	case strings.Contains(ua, "opencode"):
		return "OpenCode"
	case strings.Contains(ua, "codex"):
		return "Codex"
	case strings.Contains(ua, "aider"):
		return "Aider"
	case strings.Contains(ua, "cursor"):
		return "Cursor"
	case strings.Contains(ua, "continue"):
		return "Continue"
	case strings.Contains(ua, "cline"):
		return "Cline"
	case strings.Contains(ua, "python-httpx") || strings.Contains(ua, "anthropic") || strings.Contains(ua, "openai"):
		return "SDK"
	}

	if ua == "" {
		return "unknown"
	}
	return prettyUAToken(ua)
}

// prettyUAToken extracts the product name from a User-Agent (the part before the
// first '/' or space) and title-cases it, so "myagent/1.2.3" → "Myagent".
func prettyUAToken(ua string) string {
	tok := ua
	if i := strings.IndexAny(tok, "/ "); i > 0 {
		tok = tok[:i]
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "other"
	}
	return strings.ToUpper(tok[:1]) + tok[1:]
}

// detectProvider classifies the upstream provider from the model name first
// (most reliable), then the wire format as a fallback.
func detectProvider(isAnthropic bool, model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude"):
		return "Anthropic"
	case strings.HasPrefix(m, "gpt") || strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "chatgpt"):
		return "OpenAI"
	case strings.HasPrefix(m, "gemini"):
		return "Google"
	case strings.Contains(m, "deepseek"):
		return "DeepSeek"
	case strings.Contains(m, "llama"):
		return "Meta"
	case strings.Contains(m, "mistral") || strings.Contains(m, "mixtral"):
		return "Mistral"
	case strings.Contains(m, "grok"):
		return "xAI"
	case strings.Contains(m, "qwen"):
		return "Qwen"
	}
	if isAnthropic {
		return "Anthropic"
	}
	return "OpenAI-compatible"
}
