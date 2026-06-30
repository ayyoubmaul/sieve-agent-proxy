package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func toBody(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var b map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		t.Fatalf("bad test body: %v", err)
	}
	return b
}

// effortOf reads body[key].effort (nested) or body[key] (flat), "" if absent.
func effortOf(t *testing.T, body map[string]json.RawMessage, key string, nested bool) string {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		return ""
	}
	if !nested {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var obj struct {
		Effort string `json:"effort"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.Effort
}

func TestApplyOutputPolicy(t *testing.T) {
	tests := []struct {
		name        string
		profile     string
		effort      string
		isAnthropic bool
		body        string
		key         string
		nested      bool
		want        string
	}{
		{"profile off is a no-op", "off", "low", true, `{"model":"x"}`, "output_config", true, ""},
		{"empty effort is a no-op", "anthropic", "", true, `{"model":"x"}`, "output_config", true, ""},
		{"anthropic sets effort when absent", "anthropic", "low", true, `{"model":"x"}`, "output_config", true, "low"},
		{"anthropic downgrades high to low", "anthropic", "low", true, `{"output_config":{"effort":"high"}}`, "output_config", true, "low"},
		{"anthropic never raises low to high", "anthropic", "high", true, `{"output_config":{"effort":"low"}}`, "output_config", true, "low"},
		{"openai sets reasoning_effort", "openai", "low", false, `{"model":"o1"}`, "reasoning_effort", false, "low"},
		{"openai never raises", "openai", "medium", false, `{"reasoning_effort":"low"}`, "reasoning_effort", false, "low"},
		{"auto picks anthropic on /v1/messages", "auto", "low", true, `{"model":"x"}`, "output_config", true, "low"},
		{"auto picks openai otherwise", "auto", "low", false, `{"model":"x"}`, "reasoning_effort", false, "low"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{cfg: &Config{}}
			srv.cfg.Output.Enabled = true
			srv.cfg.Output.ReasoningProfile = tt.profile
			srv.cfg.Output.Effort = tt.effort

			body := toBody(t, tt.body)
			srv.applyOutputPolicy(body, tt.isAnthropic)

			if got := effortOf(t, body, tt.key, tt.nested); got != tt.want {
				t.Errorf("effort = %q, want %q", got, tt.want)
			}
		})
	}
}

// A downgrade must not clobber sibling fields the client set in output_config.
func TestApplyOutputPolicyPreservesSiblings(t *testing.T) {
	srv := &Server{cfg: &Config{}}
	srv.cfg.Output.Enabled = true
	srv.cfg.Output.ReasoningProfile = "anthropic"
	srv.cfg.Output.Effort = "low"

	body := toBody(t, `{"output_config":{"effort":"high","task_budget":{"type":"tokens","total":40000}}}`)
	srv.applyOutputPolicy(body, true)

	var oc struct {
		Effort     string          `json:"effort"`
		TaskBudget json.RawMessage `json:"task_budget"`
	}
	if err := json.Unmarshal(body["output_config"], &oc); err != nil {
		t.Fatal(err)
	}
	if oc.Effort != "low" {
		t.Errorf("effort = %q, want low", oc.Effort)
	}
	if len(oc.TaskBudget) == 0 {
		t.Error("task_budget was dropped during downgrade")
	}
}

func intField(t *testing.T, body map[string]json.RawMessage, key string) (int, bool) {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		return 0, false
	}
	var n int
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

func TestClampMaxTokens(t *testing.T) {
	tests := []struct {
		name        string
		isAnthropic bool
		body        string
		field       string
		want        int  // expected value of field
		wantPresent bool // whether field should exist
	}{
		{"anthropic clamps down", true, `{"max_tokens":8000}`, "max_tokens", 4096, true},
		{"anthropic leaves lower cap", true, `{"max_tokens":2000}`, "max_tokens", 2000, true},
		{"openai clamps max_completion_tokens", false, `{"max_completion_tokens":9000}`, "max_completion_tokens", 4096, true},
		{"openai clamps max_tokens when no completion field", false, `{"max_tokens":9000}`, "max_tokens", 4096, true},
		{"openai never injects when absent", false, `{"model":"gpt"}`, "max_tokens", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := toBody(t, tt.body)
			clampMaxTokens(body, tt.isAnthropic, 4096)
			got, present := intField(t, body, tt.field)
			if present != tt.wantPresent {
				t.Fatalf("%s present = %v, want %v", tt.field, present, tt.wantPresent)
			}
			if present && got != tt.want {
				t.Errorf("%s = %d, want %d", tt.field, got, tt.want)
			}
		})
	}
	// max_completion_tokens takes precedence: max_tokens must be left untouched.
	body := toBody(t, `{"max_completion_tokens":9000,"max_tokens":9000}`)
	clampMaxTokens(body, false, 4096)
	if n, _ := intField(t, body, "max_completion_tokens"); n != 4096 {
		t.Errorf("max_completion_tokens = %d, want 4096", n)
	}
	if n, _ := intField(t, body, "max_tokens"); n != 9000 {
		t.Errorf("max_tokens = %d, want untouched 9000", n)
	}
}

func TestInjectDirective(t *testing.T) {
	// Anthropic: string system → directive appended to the string.
	body := toBody(t, `{"system":"You are helpful."}`)
	injectDirective(body, true, "BE CONCISE")
	var s string
	_ = json.Unmarshal(body["system"], &s)
	if !strings.Contains(s, "You are helpful.") || !strings.Contains(s, "BE CONCISE") {
		t.Errorf("anthropic string system = %q, want original + directive", s)
	}

	// Anthropic: array system → directive appended as a trailing block, prior
	// blocks left byte-identical (so a cache_control breakpoint still hits).
	body = toBody(t, `{"system":[{"type":"text","text":"core","cache_control":{"type":"ephemeral"}}]}`)
	injectDirective(body, true, "BE CONCISE")
	var blocks []map[string]json.RawMessage
	_ = json.Unmarshal(body["system"], &blocks)
	if len(blocks) != 2 {
		t.Fatalf("anthropic array system has %d blocks, want 2", len(blocks))
	}
	if _, ok := blocks[0]["cache_control"]; !ok {
		t.Error("first block lost its cache_control (cache prefix disturbed)")
	}
	var last string
	_ = json.Unmarshal(blocks[1]["text"], &last)
	if last != "BE CONCISE" {
		t.Errorf("trailing block text = %q, want directive", last)
	}

	// Anthropic: no system → directive becomes a single-block system array.
	body = toBody(t, `{"model":"claude"}`)
	injectDirective(body, true, "BE CONCISE")
	if _, ok := body["system"]; !ok {
		t.Error("anthropic no-system case did not create system")
	}

	// OpenAI: directive appended as a trailing system message; the earlier
	// messages (the cached prefix) are left in place.
	body = toBody(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	injectDirective(body, false, "BE CONCISE")
	var msgs []map[string]string
	_ = json.Unmarshal(body["messages"], &msgs)
	if len(msgs) != 2 {
		t.Fatalf("openai messages has %d entries, want 2", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Error("openai prefix message was disturbed")
	}
	if msgs[1]["role"] != "system" || msgs[1]["content"] != "BE CONCISE" {
		t.Errorf("openai trailing message = %+v, want system/BE CONCISE", msgs[1])
	}
}

func TestDirectiveText(t *testing.T) {
	if directiveText("concise", "") != conciseDirective {
		t.Error("concise style mismatch")
	}
	if directiveText("terse", "") != terseDirective {
		t.Error("terse style mismatch")
	}
	if got := directiveText("concise", "CUSTOM"); got != "CUSTOM" {
		t.Errorf("custom override = %q, want CUSTOM", got)
	}
}

func TestUsageFromResponse(t *testing.T) {
	u := usageFromResponse([]byte(`{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":4}}`), true)
	if u.In != 10 || u.Out != 20 || u.Cached != 4 {
		t.Errorf("anthropic = %+v, want {10 20 4}", u)
	}
	// OpenAI: cached via nested prompt_tokens_details.
	u = usageFromResponse([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`), false)
	if u.In != 5 || u.Out != 7 || u.Cached != 3 {
		t.Errorf("openai (nested) = %+v, want {5 7 3}", u)
	}
	// OpenAI/gateway: cached via top-level cache_read_tokens fallback.
	u = usageFromResponse([]byte(`{"usage":{"prompt_tokens":9,"completion_tokens":2,"cache_read_tokens":6}}`), false)
	if u.Cached != 6 {
		t.Errorf("openai (top-level) cached = %d, want 6", u.Cached)
	}
}

func TestStreamUsage(t *testing.T) {
	anthropic := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42,\"output_tokens\":1,\"cache_read_input_tokens\":40}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":99}}\n\n"
	if u := streamUsage(anthropic, true); u.In != 42 || u.Out != 99 || u.Cached != 40 {
		t.Errorf("anthropic stream = %+v, want {42 99 40}", u)
	}

	openaiReal := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":5}}}\n\n" +
		"data: [DONE]\n\n"
	if u := streamUsage(openaiReal, false); u.In != 8 || u.Out != 12 || u.Cached != 5 {
		t.Errorf("openai stream (real usage) = %+v, want {8 12 5}", u)
	}

	// No usage block → estimate output from streamed text at ~4 chars/token.
	// "12345678" is 8 runes → (8+3)/4 = 2.
	openaiEst := "data: {\"choices\":[{\"delta\":{\"content\":\"1234\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"5678\"}}]}\n\n" +
		"data: [DONE]\n\n"
	if u := streamUsage(openaiEst, false); u.Out != 2 {
		t.Errorf("openai stream (estimate) out = %d, want 2", u.Out)
	}
}
