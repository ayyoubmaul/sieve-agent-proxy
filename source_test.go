package main

import (
	"net/http"
	"testing"
)

func reqWithHeaders(h map[string]string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func TestDetectAgentKnownClients(t *testing.T) {
	cases := []struct {
		headers map[string]string
		want    string
	}{
		{map[string]string{"User-Agent": "claude-cli/1.2.3 (external)"}, "Claude Code"},
		{map[string]string{"x-app": "cli"}, "Claude Code"},
		{map[string]string{"User-Agent": "opencode/0.4.1"}, "OpenCode"},
		{map[string]string{"User-Agent": "codex/0.1.0"}, "Codex"},
		{map[string]string{"User-Agent": "aider/0.50"}, "Aider"},
		{map[string]string{"User-Agent": "cursor-agent/1.0"}, "Cursor"},
		{map[string]string{"User-Agent": "openai-python/1.30"}, "SDK"},
		{map[string]string{"User-Agent": "MyTool/9.9"}, "Mytool"},
		{map[string]string{}, "unknown"},
	}
	for _, c := range cases {
		if got := detectAgent(reqWithHeaders(c.headers)); got != c.want {
			t.Errorf("detectAgent(%v) = %q, want %q", c.headers, got, c.want)
		}
	}
}

func TestDetectProvider(t *testing.T) {
	cases := []struct {
		isAnthropic bool
		model       string
		want        string
	}{
		{true, "claude-sonnet-4", "Anthropic"},
		{false, "claude-3-5-sonnet", "Anthropic"}, // model wins over wire format
		{false, "gpt-4o", "OpenAI"},
		{false, "o3-mini", "OpenAI"},
		{false, "gemini-2.0-flash", "Google"},
		{false, "deepseek-chat", "DeepSeek"},
		{false, "llama-3.1-70b", "Meta"},
		{false, "grok-2", "xAI"},
		{true, "", "Anthropic"},          // fallback to wire format
		{false, "", "OpenAI-compatible"}, // fallback to wire format
	}
	for _, c := range cases {
		if got := detectProvider(c.isAnthropic, c.model); got != c.want {
			t.Errorf("detectProvider(%v, %q) = %q, want %q", c.isAnthropic, c.model, got, c.want)
		}
	}
}

func TestRecordSourceAggregates(t *testing.T) {
	s := &Server{stats: &Stats{}}
	s.recordSource("Claude Code", "Anthropic")
	s.recordSource("Claude Code", "Anthropic")
	s.recordSource("OpenCode", "OpenAI")

	if got := s.stats.Sources["Claude Code · Anthropic"].Requests; got != 2 {
		t.Errorf("Claude Code count = %d, want 2", got)
	}
	if got := s.stats.Sources["OpenCode · OpenAI"].Requests; got != 1 {
		t.Errorf("OpenCode count = %d, want 1", got)
	}
}
