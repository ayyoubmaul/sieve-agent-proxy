package sieve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
)

type tokenEntry struct {
	value []byte
	exp   time.Time
	ts    time.Time
}

type semEntry struct {
	query string
	value []byte
	ts    time.Time
}

// Cache provides three response layers:
//
//	L1  (token)      — exact SHA-256 match, O(1)
//	L1n (norm token) — SHA-256 of volatile-normalized messages, O(1); opt-in
//	L2  (semantic)   — TF-IDF cosine similarity, O(n)
//
// L1n absorbs conversations that are structurally identical but differ only in
// injected volatile tokens — the most common example being a system prompt that
// includes today's date ("Today is 2026-06-30 …"). Without L1n such a prompt
// busts L1 every day even though the conversation and response are identical.
// L1n is disabled by default (CACHE_L1_NORMALIZE=false) because serving a
// cached response for a prompt that differs in dates is only safe when the
// caller knows the date does not affect the answer.
//
// All three levels are safe for concurrent use.
type Cache struct {
	tokMu   sync.Mutex
	tok     map[string]*tokenEntry
	tokTTL  time.Duration
	tokMax  int
	tokHits int64
	tokMiss int64

	normMu   sync.Mutex
	norm     map[string]*tokenEntry // L1n: volatile-normalized key, shares tok TTL/max
	normHits int64
	normMiss int64

	semMu   sync.Mutex
	sem     []*semEntry
	semThr  float64
	semMax  int
	semHits int64
	semMiss int64
}

func NewCache(cfg *Config) *Cache {
	return &Cache{
		tok:    make(map[string]*tokenEntry),
		tokTTL: time.Duration(cfg.TokenCache.TTL) * time.Second,
		tokMax: cfg.TokenCache.MaxEntries,
		norm:   make(map[string]*tokenEntry),
		semThr: cfg.SemanticCache.Threshold,
		semMax: cfg.SemanticCache.MaxEntries,
	}
}

// ── Volatile-token normalisation ─────────────────────────────────────────

// normalizeVolatile strips volatile tokens (timestamps, UUIDs, hex strings,
// plain numbers) from text and replaces them with stable placeholders. This is
// the same transformation lineShape() applies to tool output lines before
// deduplication — reused here to make cache keys insensitive to dynamic content
// that does not carry semantic meaning about what the user wants.
//
// The regexes (reTimestamp, reUUID, reHex, reClock, reNumber) live in
// toolresult.go (same package) and are shared to avoid duplication.
func normalizeVolatile(s string) string {
	s = reTimestamp.ReplaceAllString(s, "<ts>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reClock.ReplaceAllString(s, "<ts>")
	s = reNumber.ReplaceAllString(s, "<n>")
	return s
}

// normalizeContentForKey strips volatile tokens from a raw JSON content field
// for use in cache-key computation. Handles both the plain-string form and the
// block-array form (same two shapes normalizeContent in compress.go handles).
func normalizeContentForKey(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		b, _ := json.Marshal(normalizeVolatile(s))
		return b
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) == nil {
		for i := range blocks {
			if tx, ok := blocks[i]["text"]; ok {
				var txt string
				if json.Unmarshal(tx, &txt) == nil {
					nb, _ := json.Marshal(normalizeVolatile(txt))
					blocks[i]["text"] = nb
				}
			}
		}
		b, _ := json.Marshal(blocks)
		return b
	}
	return raw
}

// hashKeyVolatileNorm returns a cache key identical to hashKeyWithTools except
// that volatile tokens are stripped from every message's content first. Two
// conversations that differ only in injected dates, UUIDs, or counters produce
// the same key — allowing L1n to serve cached responses across those variants.
func hashKeyVolatileNorm(messages []Message, model string, tools json.RawMessage) string {
	norm := make([]Message, len(messages))
	for i, m := range messages {
		nm := m
		nm.Content = normalizeContentForKey(m.Content)
		norm[i] = nm
	}
	payload := struct {
		Messages []Message       `json:"messages"`
		Model    string          `json:"model"`
		Tools    json.RawMessage `json:"tools,omitempty"`
	}{norm, model, tools}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ── L1n: normalised token cache ───────────────────────────────────────────

func (c *Cache) NormGet(messages []Message, model string, tools json.RawMessage) ([]byte, bool) {
	key := hashKeyVolatileNorm(messages, model, tools)
	c.normMu.Lock()
	defer c.normMu.Unlock()

	e, ok := c.norm[key]
	if !ok || time.Now().After(e.exp) {
		if ok {
			delete(c.norm, key)
		}
		c.normMiss++
		return nil, false
	}
	c.normHits++
	e.ts = time.Now()
	return e.value, true
}

func (c *Cache) NormSet(messages []Message, model string, tools json.RawMessage, value []byte) {
	key := hashKeyVolatileNorm(messages, model, tools)
	c.normMu.Lock()
	defer c.normMu.Unlock()

	if len(c.norm) >= c.tokMax {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, v := range c.norm {
			if first || v.ts.Before(oldest) {
				oldest, oldestKey, first = v.ts, k, false
			}
		}
		if oldestKey != "" {
			delete(c.norm, oldestKey)
		}
	}

	cp := make([]byte, len(value))
	copy(cp, value)
	c.norm[key] = &tokenEntry{value: cp, exp: time.Now().Add(c.tokTTL), ts: time.Now()}
}

func (c *Cache) NormStats() map[string]interface{} {
	c.normMu.Lock()
	defer c.normMu.Unlock()
	total := c.normHits + c.normMiss
	rate := "0%"
	if total > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(c.normHits)/float64(total)*100)
	}
	return map[string]interface{}{
		"size":    len(c.norm),
		"hits":    c.normHits,
		"misses":  c.normMiss,
		"hitRate": rate,
	}
}

// ── L1: token cache ──────────────────────────────────────────────────────

func hashKey(messages []Message, model string) string {
	payload := struct {
		Messages []Message `json:"messages"`
		Model    string    `json:"model"`
	}{messages, model}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashKeyWithTools(messages []Message, model string, tools json.RawMessage) string {
	payload := struct {
		Messages []Message       `json:"messages"`
		Model    string          `json:"model"`
		Tools    json.RawMessage `json:"tools,omitempty"`
	}{messages, model, tools}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c *Cache) TokenGet(messages []Message, model string, tools json.RawMessage) ([]byte, bool) {
	key := hashKeyWithTools(messages, model, tools)
	c.tokMu.Lock()
	defer c.tokMu.Unlock()

	e, ok := c.tok[key]
	if !ok || time.Now().After(e.exp) {
		if ok {
			delete(c.tok, key)
		}
		c.tokMiss++
		return nil, false
	}
	c.tokHits++
	e.ts = time.Now()
	return e.value, true
}

func (c *Cache) TokenSet(messages []Message, model string, tools json.RawMessage, value []byte) {
	key := hashKeyWithTools(messages, model, tools)
	c.tokMu.Lock()
	defer c.tokMu.Unlock()

	if len(c.tok) >= c.tokMax {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, v := range c.tok {
			if first || v.ts.Before(oldest) {
				oldest, oldestKey, first = v.ts, k, false
			}
		}
		if oldestKey != "" {
			delete(c.tok, oldestKey)
		}
	}

	cp := make([]byte, len(value))
	copy(cp, value)
	c.tok[key] = &tokenEntry{value: cp, exp: time.Now().Add(c.tokTTL), ts: time.Now()}
}

func (c *Cache) TokenStats() map[string]interface{} {
	c.tokMu.Lock()
	defer c.tokMu.Unlock()
	total := c.tokHits + c.tokMiss
	rate := "0%"
	if total > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(c.tokHits)/float64(total)*100)
	}
	return map[string]interface{}{
		"size":    len(c.tok),
		"hits":    c.tokHits,
		"misses":  c.tokMiss,
		"hitRate": rate,
	}
}

// ── L2: semantic cache ───────────────────────────────────────────────────

func (c *Cache) SemanticGet(messages []Message) ([]byte, float64, bool) {
	query := queryText(messages)

	c.semMu.Lock()
	defer c.semMu.Unlock()

	if len(c.sem) == 0 {
		c.semMiss++
		return nil, 0, false
	}

	corpus := make([]string, len(c.sem))
	for i, e := range c.sem {
		corpus[i] = e.query
	}

	qVec := tfidf(query, corpus)
	var best *semEntry
	bestScore := 0.0
	for _, e := range c.sem {
		score := cosine(qVec, tfidf(e.query, corpus))
		if score > bestScore {
			bestScore, best = score, e
		}
	}

	if best != nil && bestScore >= c.semThr {
		c.semHits++
		best.ts = time.Now()
		return best.value, bestScore, true
	}
	c.semMiss++
	return nil, 0, false
}

func (c *Cache) SemanticSet(messages []Message, value []byte) {
	query := queryText(messages)

	c.semMu.Lock()
	defer c.semMu.Unlock()

	// len(c.sem) > 0 guards the slice eviction below: with semMax <= 0 the
	// condition would otherwise fire on an empty buffer and slice c.sem[1:]
	// out of range. This matches TokenSet's (map-based) behaviour for max 0.
	if len(c.sem) > 0 && len(c.sem) >= c.semMax {
		oldestIdx := 0
		for i, e := range c.sem {
			if e.ts.Before(c.sem[oldestIdx].ts) {
				oldestIdx = i
			}
		}
		c.sem = append(c.sem[:oldestIdx], c.sem[oldestIdx+1:]...)
	}

	cp := make([]byte, len(value))
	copy(cp, value)
	c.sem = append(c.sem, &semEntry{query: query, value: cp, ts: time.Now()})
}

func (c *Cache) SemanticStats() map[string]interface{} {
	c.semMu.Lock()
	defer c.semMu.Unlock()
	total := c.semHits + c.semMiss
	rate := "0%"
	if total > 0 {
		rate = fmt.Sprintf("%.1f%%", float64(c.semHits)/float64(total)*100)
	}
	return map[string]interface{}{
		"size":    len(c.sem),
		"hits":    c.semHits,
		"misses":  c.semMiss,
		"hitRate": rate,
	}
}

// ── Shared ───────────────────────────────────────────────────────────────

func (c *Cache) Clear() {
	c.tokMu.Lock()
	c.tok = make(map[string]*tokenEntry)
	c.tokHits, c.tokMiss = 0, 0
	c.tokMu.Unlock()

	c.normMu.Lock()
	c.norm = make(map[string]*tokenEntry)
	c.normHits, c.normMiss = 0, 0
	c.normMu.Unlock()

	c.semMu.Lock()
	c.sem = nil
	c.semHits, c.semMiss = 0, 0
	c.semMu.Unlock()
}

// ── TF-IDF cosine similarity ─────────────────────────────────────────────

var (
	reNonAlnum = regexp.MustCompile(`[^a-z0-9\s]`)
	reSpaces   = regexp.MustCompile(`\s+`)
)

// queryText extracts the user-facing query (system messages excluded) and
// normalises volatile tokens (dates, UUIDs, hex, numbers) before returning it.
// Normalisation is unconditional for L2: two queries that differ only in a
// dynamic date or request-ID should compare as semantically equivalent, and
// stripping those tokens before TF-IDF makes that happen without any threshold
// tuning. (L1 is unaffected — it uses the raw hash.)
func queryText(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		t := m.Text()
		if t == "" && len(m.Content) > 0 {
			t = string(m.Content)
		}
		parts = append(parts, normalizeVolatile(t))
	}
	return truncateRunes(strings.Join(parts, " "), 2000)
}

func tokenize(text string) []string {
	text = strings.ToLower(text)
	text = reNonAlnum.ReplaceAllString(text, " ")
	raw := reSpaces.Split(text, -1)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if len(t) > 1 {
			out = append(out, t)
		}
	}
	return out
}

// tfidf computes a smoothed TF-IDF vector for text against a corpus.
func tfidf(text string, corpus []string) map[string]float64 {
	tokens := tokenize(text)
	N := float64(len(corpus) + 1)

	tf := map[string]float64{}
	for _, t := range tokens {
		tf[t]++
	}
	denom := float64(len(tokens))
	if denom == 0 {
		denom = 1
	}
	for t := range tf {
		tf[t] /= denom
	}

	corpusSets := make([]map[string]bool, len(corpus))
	for i, doc := range corpus {
		set := map[string]bool{}
		for _, t := range tokenize(doc) {
			set[t] = true
		}
		corpusSets[i] = set
	}

	vec := map[string]float64{}
	for t, v := range tf {
		df := 1.0
		for _, set := range corpusSets {
			if set[t] {
				df++
			}
		}
		// Smoothed IDF (log((N+1)/(df+1)) + 1), always ≥ 1. The plain log(N/df)
		// collapses to 0 once a term appears in every corpus doc — which, with a
		// small cache, is every term of a stored entry, zeroing its whole vector
		// so cosine similarity is always 0 and the cache never hits.
		vec[t] = v * (math.Log((N+1)/(df+1)) + 1)
	}
	return vec
}

func cosine(a, b map[string]float64) float64 {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var dot, ma, mb float64
	for k := range keys {
		va, vb := a[k], b[k]
		dot += va * vb
		ma += va * va
		mb += vb * vb
	}
	d := math.Sqrt(ma) * math.Sqrt(mb)
	if d == 0 {
		return 0
	}
	return dot / d
}
