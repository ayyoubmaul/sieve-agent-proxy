package sieve

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// cacheTestServer returns a proxy whose upstream counts how many times it's
// actually hit (a cache hit means the upstream is NOT called). semantic toggles
// L2 so the L1 tests can isolate exact-match behaviour.
func cacheTestServer(t *testing.T, calls *int32, semantic bool) http.Handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		raw, _ := io.ReadAll(r.Body)
		var b struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &b)
		if b.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n")
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
			fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":3}}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &Config{TargetURL: upstream.URL}
	cfg.TokenCache.Enabled = true
	cfg.TokenCache.MaxEntries = 1000
	cfg.TokenCache.TTL = 3600
	cfg.SemanticCache.Enabled = semantic
	cfg.SemanticCache.MaxEntries = 500
	cfg.SemanticCache.Threshold = 0.1 // low, to isolate cache *mechanics* from TF-IDF tuning
	return NewServer(cfg).Handler()
}

func post(t *testing.T, h http.Handler, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// Proof the cache works: the SAME request twice hits the upstream once.
func TestL1CacheHitsOnIdenticalRequest(t *testing.T) {
	var calls int32
	h := cacheTestServer(t, &calls, false) // isolate L1
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hello world"}]}`

	post(t, h, body)
	post(t, h, body)

	if calls != 1 {
		t.Errorf("upstream called %d times, want 1 (2nd request should be an L1 cache hit)", calls)
	}
}

// Why it never hits in a coding agent: the conversation grows every turn, so no
// two requests are ever byte-identical → L1 can't match.
func TestL1CacheMissesWhenConversationGrows(t *testing.T) {
	var calls int32
	h := cacheTestServer(t, &calls, false) // isolate L1

	turn1 := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`
	turn2 := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`

	post(t, h, turn1)
	post(t, h, turn2)

	if calls != 2 {
		t.Errorf("upstream called %d times, want 2 (a grown conversation is a different prompt → no hit)", calls)
	}
}

// The semantic (L2) cache — the one meant to catch *similar* prompts — is gated
// to non-streaming requests. Agents stream, so it never gets consulted.
func TestSemanticCacheSkippedForStreaming(t *testing.T) {
	// Non-stream: two similar-but-not-identical prompts → 2nd is served by L2.
	t.Run("non-stream uses L2", func(t *testing.T) {
		var calls int32
		h := cacheTestServer(t, &calls, true)
		post(t, h, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"reverse a string in python"}]}`)
		post(t, h, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"reverse a string in python please"}]}`)
		if calls != 1 {
			t.Errorf("upstream called %d times, want 1 (2nd similar prompt should hit L2)", calls)
		}
	})

	// Streaming: the same two similar prompts both miss — L2 is never consulted.
	t.Run("stream skips L2", func(t *testing.T) {
		var calls int32
		h := cacheTestServer(t, &calls, true)
		post(t, h, `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"reverse a string in python"}]}`)
		post(t, h, `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"reverse a string in python please"}]}`)
		if calls != 2 {
			t.Errorf("upstream called %d times, want 2 (streaming bypasses L2, so similar prompts miss)", calls)
		}
	})
}

// L1 (exact) does still work for streaming — an identical streamed request is
// replayed from cache. (It just rarely happens in real agent use.)
func TestL1CacheHitsOnIdenticalStream(t *testing.T) {
	var calls int32
	h := cacheTestServer(t, &calls, false) // isolate L1
	body := `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"identical streamed"}]}`

	post(t, h, body)
	post(t, h, body)

	if calls != 1 {
		t.Errorf("upstream called %d times, want 1 (identical stream should replay from L1)", calls)
	}
}
