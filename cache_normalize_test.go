package main

import (
	"encoding/json"
	"testing"
)

// ── normalizeVolatile ────────────────────────────────────────────────────

func TestNormalizeVolatileStripsTimestamp(t *testing.T) {
	in := "deploy ran at 2026-06-30T14:22:05Z and took 3s"
	out := normalizeVolatile(in)
	if contains(out, "2026") {
		t.Errorf("timestamp not stripped: %q", out)
	}
	if !contains(out, "<ts>") {
		t.Errorf("expected <ts> placeholder: %q", out)
	}
}

func TestNormalizeVolatileStripsUUID(t *testing.T) {
	in := "session 550e8400-e29b-41d4-a716-446655440000 started"
	out := normalizeVolatile(in)
	if contains(out, "550e8400") {
		t.Errorf("UUID not stripped: %q", out)
	}
	if !contains(out, "<uuid>") {
		t.Errorf("expected <uuid> placeholder: %q", out)
	}
}

func TestNormalizeVolatileStripsNumbers(t *testing.T) {
	in := "found 42 errors on line 17"
	out := normalizeVolatile(in)
	if contains(out, "42") || contains(out, "17") {
		t.Errorf("numbers not stripped: %q", out)
	}
}

func TestNormalizeVolatilePreservesWords(t *testing.T) {
	in := "fix the authentication bug"
	out := normalizeVolatile(in)
	if out != in {
		t.Errorf("words should be unchanged: got %q", out)
	}
}

// ── queryText normalisation (L2 semantic cache) ──────────────────────────

// Two queries that differ only in date should produce identical queryText
// output, making their TF-IDF vectors identical → guaranteed L2 hit.
func TestQueryTextNormalisesVolatileTokens(t *testing.T) {
	msgs1 := []Message{
		{Role: "user", Content: jsonStr("check commits from 2026-06-30")},
	}
	msgs2 := []Message{
		{Role: "user", Content: jsonStr("check commits from 2026-06-29")},
	}
	q1 := queryText(msgs1)
	q2 := queryText(msgs2)
	if q1 != q2 {
		t.Errorf("expected identical normalised query text:\n  q1=%q\n  q2=%q", q1, q2)
	}
}

func TestQueryTextNormalisesUUID(t *testing.T) {
	msgs1 := []Message{
		{Role: "user", Content: jsonStr("status of job 550e8400-e29b-41d4-a716-446655440000")},
	}
	msgs2 := []Message{
		{Role: "user", Content: jsonStr("status of job aabbccdd-eeff-1122-3344-556677889900")},
	}
	if queryText(msgs1) != queryText(msgs2) {
		t.Error("queries differing only in UUID should produce identical queryText")
	}
}

func TestQueryTextSkipsSystemMessage(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: jsonStr("Today is 2026-06-30. You are an assistant.")},
		{Role: "user", Content: jsonStr("fix the build")},
	}
	// System message should not appear in queryText regardless of its content.
	q := queryText(msgs)
	if contains(q, "Today") || contains(q, "2026") || contains(q, "assistant") {
		t.Errorf("system message leaked into queryText: %q", q)
	}
}

// ── L2 semantic cache hit on date-variant queries ─────────────────────────

func TestSemanticCacheHitsOnDateVariantQuery(t *testing.T) {
	cfg := testCacheConfig()
	cfg.SemanticCache.Threshold = 0.5 // generous threshold to isolate normalisation effect
	c := NewCache(cfg)

	msgs1 := []Message{
		{Role: "user", Content: jsonStr("check commits from 2026-06-30")},
	}
	msgs2 := []Message{
		{Role: "user", Content: jsonStr("check commits from 2026-06-29")},
	}

	payload := []byte(`{"id":"msg_1","content":[{"type":"text","text":"here are the commits"}]}`)
	c.SemanticSet(msgs1, payload)

	got, _, ok := c.SemanticGet(msgs2)
	if !ok {
		t.Fatal("expected L2 hit for date-variant query after normalisation")
	}
	if string(got) != string(payload) {
		t.Errorf("unexpected cached payload: %s", got)
	}
}

func TestSemanticCacheHitsOnUUIDVariantQuery(t *testing.T) {
	cfg := testCacheConfig()
	cfg.SemanticCache.Threshold = 0.5
	c := NewCache(cfg)

	msgs1 := []Message{
		{Role: "user", Content: jsonStr("status of job 550e8400-e29b-41d4-a716-446655440000")},
	}
	msgs2 := []Message{
		{Role: "user", Content: jsonStr("status of job aabbccdd-eeff-1122-3344-556677889900")},
	}

	payload := []byte(`{"id":"msg_2","content":[{"type":"text","text":"job complete"}]}`)
	c.SemanticSet(msgs1, payload)

	_, _, ok := c.SemanticGet(msgs2)
	if !ok {
		t.Fatal("expected L2 hit for UUID-variant query after normalisation")
	}
}

// ── L1n: normalised token cache ───────────────────────────────────────────

func TestL1NMissesWithoutSet(t *testing.T) {
	cfg := testCacheConfig()
	c := NewCache(cfg)

	msgs := []Message{
		{Role: "system", Content: jsonStr("Today is 2026-06-30. You are a coding assistant.")},
		{Role: "user", Content: jsonStr("fix the build")},
	}
	if _, ok := c.NormGet(msgs, "claude", nil); ok {
		t.Fatal("expected L1n miss on empty cache")
	}
}

// Store under one date, retrieve under a different date — should hit L1n
// because both normalize to the same key.
func TestL1NHitsOnDateVariantSystemPrompt(t *testing.T) {
	cfg := testCacheConfig()
	c := NewCache(cfg)

	msgsDay1 := []Message{
		{Role: "system", Content: jsonStr("Today is 2026-06-30. You are a coding assistant.")},
		{Role: "user", Content: jsonStr("fix the build")},
	}
	msgsDay2 := []Message{
		{Role: "system", Content: jsonStr("Today is 2026-07-01. You are a coding assistant.")},
		{Role: "user", Content: jsonStr("fix the build")},
	}

	payload := []byte(`{"id":"msg_3","content":[{"type":"text","text":"done"}]}`)
	c.NormSet(msgsDay1, "claude", nil, payload)

	got, ok := c.NormGet(msgsDay2, "claude", nil)
	if !ok {
		t.Fatal("expected L1n hit: conversations differ only in injected date")
	}
	if string(got) != string(payload) {
		t.Errorf("unexpected payload: %s", got)
	}
}

// L1 exact must still miss when only the date changes.
func TestL1ExactMissesOnDateVariant(t *testing.T) {
	cfg := testCacheConfig()
	c := NewCache(cfg)

	msgsDay1 := []Message{
		{Role: "system", Content: jsonStr("Today is 2026-06-30. You are a coding assistant.")},
		{Role: "user", Content: jsonStr("fix the build")},
	}
	msgsDay2 := []Message{
		{Role: "system", Content: jsonStr("Today is 2026-07-01. You are a coding assistant.")},
		{Role: "user", Content: jsonStr("fix the build")},
	}

	payload := []byte(`{"id":"msg_4","content":[{"type":"text","text":"done"}]}`)
	c.TokenSet(msgsDay1, "claude", nil, payload)

	if _, ok := c.TokenGet(msgsDay2, "claude", nil); ok {
		t.Fatal("L1 exact must NOT hit for different dates — that would break date-specific queries")
	}
}

// Different user messages must not produce the same L1n key.
func TestL1NMissesOnDifferentIntent(t *testing.T) {
	cfg := testCacheConfig()
	c := NewCache(cfg)

	msgs1 := []Message{
		{Role: "system", Content: jsonStr("Today is 2026-06-30.")},
		{Role: "user", Content: jsonStr("fix the build")},
	}
	msgs2 := []Message{
		{Role: "system", Content: jsonStr("Today is 2026-06-30.")},
		{Role: "user", Content: jsonStr("write the tests")},
	}

	payload := []byte(`{"id":"msg_5","content":[{"type":"text","text":"done"}]}`)
	c.NormSet(msgs1, "claude", nil, payload)

	if _, ok := c.NormGet(msgs2, "claude", nil); ok {
		t.Fatal("L1n must NOT hit when user messages differ in meaning")
	}
}

// ── NormStats ─────────────────────────────────────────────────────────────

func TestNormStatsTracksHitsAndMisses(t *testing.T) {
	cfg := testCacheConfig()
	c := NewCache(cfg)

	msgs := []Message{{Role: "user", Content: jsonStr("hello")}}
	payload := []byte(`{}`)
	c.NormSet(msgs, "m", nil, payload)

	c.NormGet(msgs, "m", nil) // hit
	c.NormGet([]Message{{Role: "user", Content: jsonStr("other")}}, "m", nil) // miss

	st := c.NormStats()
	if st["hits"].(int64) != 1 {
		t.Errorf("expected 1 hit, got %v", st["hits"])
	}
	if st["misses"].(int64) != 1 {
		t.Errorf("expected 1 miss, got %v", st["misses"])
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func jsonStr(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

func testCacheConfig() *Config {
	cfg := &Config{}
	cfg.TokenCache.Enabled = true
	cfg.TokenCache.TTL = 3600
	cfg.TokenCache.MaxEntries = 100
	cfg.SemanticCache.Threshold = 0.82
	cfg.SemanticCache.MaxEntries = 100
	return cfg
}
