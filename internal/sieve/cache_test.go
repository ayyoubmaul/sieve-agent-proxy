package sieve

import (
	"encoding/json"
	"testing"
)

// A cache configured with a zero max must not panic on write. Production's
// LoadConfig never produces a 0, but an explicit SEMANTIC_CACHE_MAX=0 /
// TOKEN_CACHE_MAX=0 in the environment would, and the write path runs
// regardless of the Enabled flags.
func TestCacheZeroMaxNoPanic(t *testing.T) {
	cfg := &Config{}
	cfg.TokenCache.MaxEntries = 0
	cfg.SemanticCache.MaxEntries = 0
	cache := NewCache(cfg)

	cb, _ := json.Marshal("hello")
	msgs := []Message{{Role: "user", Content: cb, Extra: map[string]json.RawMessage{}}}
	resp := []byte(`{"content":"ok"}`)

	// Multiple writes exercise both the empty-buffer and at-capacity branches.
	for i := 0; i < 3; i++ {
		cache.TokenSet(msgs, "m", nil, resp)
		cache.SemanticSet(msgs, resp)
	}
}
