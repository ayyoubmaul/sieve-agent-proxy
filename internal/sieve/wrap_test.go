package sieve

import (
	"slices"
	"testing"
)

func TestWrapSpec(t *testing.T) {
	// copilot is rejected with an explanation.
	if _, err := wrapSpec("copilot"); err == nil {
		t.Error("wrapSpec(copilot) should return an error")
	}

	// Known agents resolve to their binary.
	if s, _ := wrapSpec("claude"); s.bin != "claude" || !s.anthropic || s.openai {
		t.Errorf("claude spec = %+v, want anthropic-only", s)
	}
	if s, _ := wrapSpec("codex"); s.bin != "codex" || !s.openai || s.anthropic {
		t.Errorf("codex spec = %+v, want openai-only", s)
	}
	if s, _ := wrapSpec("cursor"); s.bin != "cursor-agent" {
		t.Errorf("cursor bin = %q, want cursor-agent", s.bin)
	}

	// Unknown agent: best-effort with both base URLs and the binary = name.
	s, err := wrapSpec("mystery")
	if err != nil {
		t.Fatalf("unknown agent should not error: %v", err)
	}
	if s.bin != "mystery" || !s.anthropic || !s.openai {
		t.Errorf("unknown spec = %+v, want both base URLs, bin=mystery", s)
	}
}

func TestBuildWrapEnv(t *testing.T) {
	const origin = "http://localhost:4141"

	// Claude: ANTHROPIC_BASE_URL only, no /v1 suffix.
	env, names := buildWrapEnv(wrapAgents["claude"], origin)
	if !slices.Contains(env, "ANTHROPIC_BASE_URL=http://localhost:4141") {
		t.Errorf("claude env = %v, want ANTHROPIC_BASE_URL without /v1", env)
	}
	if slices.ContainsFunc(names, func(n string) bool { return n == "OPENAI_BASE_URL" }) {
		t.Errorf("claude should not set OPENAI_BASE_URL; names = %v", names)
	}

	// Codex: OPENAI_BASE_URL with /v1 suffix.
	env, _ = buildWrapEnv(wrapAgents["codex"], origin)
	if !slices.Contains(env, "OPENAI_BASE_URL=http://localhost:4141/v1") {
		t.Errorf("codex env = %v, want OPENAI_BASE_URL with /v1", env)
	}

	// Aider: both vendor base URLs plus the litellm *_API_BASE aliases.
	env, names = buildWrapEnv(wrapAgents["aider"], origin)
	want := []string{
		"ANTHROPIC_BASE_URL=http://localhost:4141",
		"OPENAI_BASE_URL=http://localhost:4141/v1",
		"OPENAI_API_BASE=http://localhost:4141/v1",
		"ANTHROPIC_API_BASE=http://localhost:4141",
	}
	for _, w := range want {
		if !slices.Contains(env, w) {
			t.Errorf("aider env missing %q; got %v", w, env)
		}
	}
	if len(names) != 4 {
		t.Errorf("aider set %d names, want 4: %v", len(names), names)
	}
}
