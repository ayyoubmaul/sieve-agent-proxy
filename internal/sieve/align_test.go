package sieve

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// alignServer builds a Server with PromptAlign fully enabled (the production
// defaults) so tests exercise normalize + reorder + inject together.
func alignServer() *Server {
	cfg := &Config{}
	cfg.PromptAlign.Enabled = true
	cfg.PromptAlign.Inject = true
	cfg.PromptAlign.Reorder = true
	cfg.PromptAlign.Normalize = true
	cfg.PromptAlign.MaxBreakpoints = 3
	cfg.PromptAlign.SetBeta = true
	return &Server{cfg: cfg}
}

func bodyOf(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return body
}

func countCacheControl(body map[string]json.RawMessage) int {
	b, _ := json.Marshal(body)
	return bytes.Count(b, []byte(`"cache_control"`))
}

// 1. No client breakpoints → inject on system + last stable message, never the
// newest user turn, capped at MaxBreakpoints.
func TestAlignAnthropicInjectsWhenClientHasNone(t *testing.T) {
	s := alignServer()
	body := bodyOf(t, `{
		"model":"claude",
		"system":[{"type":"text","text":"You are a coding assistant."}],
		"tools":[{"name":"read","description":"read a file"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"first question"}]},
			{"role":"assistant","content":[{"type":"text","text":"first answer"}]},
			{"role":"user","content":[{"type":"text","text":"latest question"}]}
		]
	}`)

	res := s.applyPromptAlign(body, true)
	if !res.applied {
		t.Fatal("expected align to apply")
	}
	if res.breakpoints != 3 {
		t.Fatalf("expected 3 breakpoints (tools+system+history), got %d", res.breakpoints)
	}
	if got := countCacheControl(body); got != 3 {
		t.Fatalf("expected 3 cache_control markers in body, got %d", got)
	}
	// The newest user turn ("latest question") must NOT be marked.
	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(mustMarshal(body), &parsed)
	last := parsed.Messages[len(parsed.Messages)-1]
	if bytes.Contains(last, []byte(`"cache_control"`)) {
		t.Errorf("newest user message must not carry a breakpoint: %s", last)
	}
}

// 2. Client already set cache_control → respected, zero injection.
func TestAlignAnthropicRespectsClientBreakpoints(t *testing.T) {
	s := alignServer()
	body := bodyOf(t, `{
		"model":"claude",
		"system":[{"type":"text","text":"You are a coding assistant.","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":[{"type":"text","text":"hi"}]},
			{"role":"user","content":[{"type":"text","text":"again"}]}
		]
	}`)

	before := countCacheControl(body)
	res := s.applyPromptAlign(body, true)
	if res.breakpoints != 0 {
		t.Fatalf("expected 0 injected (client manages breakpoints), got %d", res.breakpoints)
	}
	if after := countCacheControl(body); after != before {
		t.Fatalf("breakpoint count changed: before=%d after=%d", before, after)
	}
}

// 3. cache_control must never land on a tool_result block.
func TestAlignAnthropicNeverMarksToolResult(t *testing.T) {
	s := alignServer()
	body := bodyOf(t, `{
		"model":"claude",
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"calling tool"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"big output"}]},
			{"role":"user","content":[{"type":"text","text":"thanks, next?"}]}
		]
	}`)

	s.applyPromptAlign(body, true)

	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(mustMarshal(body), &parsed)
	for i, m := range parsed.Messages {
		var msg struct {
			Content []map[string]json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		for _, blk := range msg.Content {
			var typ string
			_ = json.Unmarshal(blk["type"], &typ)
			if typ == "tool_result" {
				if _, marked := blk["cache_control"]; marked {
					t.Errorf("tool_result block in message %d was marked", i)
				}
			}
		}
	}
}

// 4. Two requests differing only in trailing whitespace / CRLF normalize to
// identical bytes.
func TestAlignNormalizationIsByteStable(t *testing.T) {
	s := alignServer()
	a := bodyOf(t, `{"model":"claude","messages":[{"role":"user","content":"fix the build   \r\n\r\n\r\n"}]}`)
	b := bodyOf(t, `{"model":"claude","messages":[{"role":"user","content":"fix the build\n"}]}`)

	s.applyPromptAlign(a, true)
	s.applyPromptAlign(b, true)

	if !bytes.Equal(a["messages"], b["messages"]) {
		t.Errorf("normalized messages differ:\n  a=%s\n  b=%s", a["messages"], b["messages"])
	}
}

// 5. Aligning an already-aligned body is a no-op (idempotent).
func TestAlignIsIdempotent(t *testing.T) {
	s := alignServer()
	src := `{
		"model":"claude",
		"system":[{"type":"text","text":"sys"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"a"}]},
			{"role":"assistant","content":[{"type":"text","text":"b"}]},
			{"role":"user","content":[{"type":"text","text":"c"}]}
		]
	}`
	body := bodyOf(t, src)

	s.applyPromptAlign(body, true)
	once := mustMarshal(body)
	s.applyPromptAlign(body, true)
	twice := mustMarshal(body)

	if !bytes.Equal(once, twice) {
		t.Errorf("align not idempotent:\n  once=%s\n  twice=%s", once, twice)
	}
}

// 6. OpenAI: a contiguous leading system run is left in place; mid-conversation
// system messages are not reordered (conservative). Content is still normalized.
func TestAlignOpenAIConservativeReorder(t *testing.T) {
	s := alignServer()
	body := bodyOf(t, `{
		"model":"gpt",
		"messages":[
			{"role":"system","content":"you are helpful"},
			{"role":"user","content":"hello there   "},
			{"role":"system","content":"mid-stream system"},
			{"role":"user","content":"again"}
		]
	}`)

	s.applyPromptAlign(body, false)

	var msgs []Message
	if err := json.Unmarshal(body["messages"], &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Order must be unchanged (mid-conversation system blocks reordering).
	wantRoles := []string{"system", "user", "system", "user"}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Errorf("message %d role = %q, want %q (order must be preserved)", i, msgs[i].Role, want)
		}
	}
	// Trailing whitespace on the first user message must be normalized away.
	if got := msgs[1].Text(); got != "hello there" {
		t.Errorf("expected normalized %q, got %q", "hello there", got)
	}
}

// 7. The prompt-caching beta flag is merged in (without dropping client flags).
func TestAlignMergesPromptCachingBeta(t *testing.T) {
	if got := mergeBeta("", "prompt-caching-2024-07-31"); got != "prompt-caching-2024-07-31" {
		t.Errorf("merge into empty: got %q", got)
	}
	got := mergeBeta("oauth-2025-04-20", "prompt-caching-2024-07-31")
	if !strings.Contains(got, "oauth-2025-04-20") || !strings.Contains(got, "prompt-caching-2024-07-31") {
		t.Errorf("merge must keep both flags: got %q", got)
	}
	// Idempotent: merging the same flag twice doesn't duplicate it.
	if again := mergeBeta(got, "prompt-caching-2024-07-31"); again != got {
		t.Errorf("merge not idempotent: %q vs %q", again, got)
	}
}

// 8. Unrecognized / empty shapes pass through unchanged (defensive no-op).
func TestAlignPassesThroughUnknownShapes(t *testing.T) {
	s := alignServer()

	// No messages at all.
	empty := bodyOf(t, `{"model":"claude"}`)
	before := mustMarshal(empty)
	s.applyPromptAlign(empty, true)
	if after := mustMarshal(empty); !bytes.Equal(before, after) {
		t.Errorf("empty body changed:\n  before=%s\n  after=%s", before, after)
	}

	// A single user message (no stable history) → no breakpoint possible.
	single := bodyOf(t, `{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"only turn"}]}]}`)
	res := s.applyPromptAlign(single, true)
	if res.breakpoints != 0 {
		t.Errorf("single message must yield 0 breakpoints, got %d", res.breakpoints)
	}
}
