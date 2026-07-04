package sieve

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

func loadConfig() *Config {
	os.Setenv("PORT", "4141")
	os.Setenv("TARGET_URL", "https://api.anthropic.com")
	os.Setenv("COMPRESSION", "true")
	os.Setenv("TOKEN_CACHE", "true")
	os.Setenv("TOKEN_CACHE_TTL", "3600")
	os.Setenv("TOKEN_CACHE_MAX", "1000")
	os.Setenv("SEMANTIC_CACHE", "true")
	os.Setenv("SEMANTIC_THRESHOLD", "0.82")
	os.Setenv("SEMANTIC_CACHE_MAX", "500")
	return LoadConfig()
}

func makeMessages(count int, length int) []Message {
	msgs := make([]Message, count)
	for i := 0; i < count; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		text := strings.Repeat(fmt.Sprintf("word%d ", i%100), length)
		cb, _ := json.Marshal(text)
		msgs[i] = Message{
			Role:    role,
			Content: cb,
			Extra:   make(map[string]json.RawMessage),
		}
	}
	return msgs
}

func makeSimilarMessages(baseText string, variations int) [][]Message {
	msgsList := make([][]Message, variations)
	for i := 0; i < variations; i++ {
		text := baseText
		if i > 0 {
			text = strings.ReplaceAll(baseText, "quick", "fast")
			text = strings.ReplaceAll(text, "brown", "red")
			text = strings.ReplaceAll(text, "fox", "dog")
		}
		cb, _ := json.Marshal(text)
		msgsList[i] = []Message{
			{Role: "user", Content: cb, Extra: make(map[string]json.RawMessage)},
		}
	}
	return msgsList
}

func BenchmarkWhitespaceNormalization(b *testing.B) {
	cfg := loadConfig()
	compressor := NewCompressor(cfg)
	messages := makeMessages(20, 50)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compressor.normalizeWS(messages)
	}
}

func BenchmarkDeduplication(b *testing.B) {
	cfg := loadConfig()
	compressor := NewCompressor(cfg)
	messages := makeMessages(20, 50)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compressor.deduplicate(messages)
	}
}

func BenchmarkCompressSmall(b *testing.B) {
	cfg := loadConfig()
	compressor := NewCompressor(cfg)
	messages := makeMessages(10, 20)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compressor.Compress(messages)
	}
}

func BenchmarkCompressMedium(b *testing.B) {
	cfg := loadConfig()
	compressor := NewCompressor(cfg)
	messages := makeMessages(50, 100)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compressor.Compress(messages)
	}
}

func BenchmarkCompressLarge(b *testing.B) {
	cfg := loadConfig()
	compressor := NewCompressor(cfg)
	messages := makeMessages(100, 200)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compressor.Compress(messages)
	}
}

func BenchmarkTokenCacheSet(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	messages := makeMessages(20, 50)
	response := []byte(`{"content": "test response"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.TokenSet(messages, "test-model", nil, response)
	}
}

func BenchmarkTokenCacheGet(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	messages := makeMessages(20, 50)
	response := []byte(`{"content": "test response"}`)

	cache.TokenSet(messages, "test-model", nil, response)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.TokenGet(messages, "test-model", nil)
	}
}

func BenchmarkTokenCacheGetMiss(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	missMessages := makeMessages(20, 51)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.TokenGet(missMessages, "test-model", nil)
	}
}

func BenchmarkSemanticCacheSet(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	messages := makeMessages(10, 30)
	response := []byte(`{"content": "test response"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.SemanticSet(messages, response)
	}
}

func BenchmarkSemanticCacheGetHit(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	messages := makeMessages(10, 30)
	response := []byte(`{"content": "test response"}`)

	cache.SemanticSet(messages, response)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.SemanticGet(messages)
	}
}

func BenchmarkSemanticCacheGetMiss(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	missMessages := makeMessages(10, 35)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.SemanticGet(missMessages)
	}
}

func BenchmarkSemanticCacheNearDuplicate(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)

	baseText := "The quick brown fox jumps over the lazy dog"
	msgsList := makeSimilarMessages(baseText, 10)

	for _, msgs := range msgsList {
		cache.SemanticSet(msgs, []byte(`{"content": "response"}`))
	}

	queryText := "The fast red dog jumps over the lazy cat"
	cb, _ := json.Marshal(queryText)
	queryMsgs := []Message{
		{Role: "user", Content: cb, Extra: make(map[string]json.RawMessage)},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.SemanticGet(queryMsgs)
	}
}

func BenchmarkTFIDF(b *testing.B) {
	corpus := make([]string, 100)
	for i := 0; i < 100; i++ {
		corpus[i] = strings.Repeat(fmt.Sprintf("word%d ", i), 10)
	}
	query := strings.Repeat("word50 word51 word52", 5)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tfidf(query, corpus)
	}
}

func BenchmarkCosineSimilarity(b *testing.B) {
	a := map[string]float64{"word1": 0.5, "word2": 0.3, "word3": 0.2}
	b_map := map[string]float64{"word1": 0.4, "word2": 0.4, "word4": 0.2}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cosine(a, b_map)
	}
}

func BenchmarkFullPipelineCacheHit(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	compressor := NewCompressor(cfg)

	messages := makeMessages(30, 50)
	response := []byte(`{"content": "test response"}`)

	cache.TokenSet(messages, "test-model", nil, response)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found := cache.TokenGet(messages, "test-model", nil); found {
			continue
		}
		compressor.Compress(messages)
	}
}

func BenchmarkFullPipelineCacheMiss(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	compressor := NewCompressor(cfg)

	messages := makeMessages(30, 50)
	response := []byte(`{"content": "test response"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, found := cache.TokenGet(messages, "test-model", nil); !found {
			result := compressor.Compress(messages)
			cache.TokenSet(result.Messages, "test-model", nil, response)
		} else {
			_ = response
		}
	}
}

func BenchmarkHashKey(b *testing.B) {
	messages := makeMessages(20, 50)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hashKeyWithTools(messages, "test-model", nil)
	}
}

func BenchmarkQueryText(b *testing.B) {
	messages := makeMessages(30, 50)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		queryText(messages)
	}
}

func TestCompressionRatio(t *testing.T) {
	cfg := loadConfig()
	compressor := NewCompressor(cfg)

	tests := []struct {
		name     string
		messages []Message
	}{
		{"Small", makeMessages(10, 20)},
		{"Medium", makeMessages(50, 50)},
		{"Large", makeMessages(100, 100)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t2 *testing.T) {
			result := compressor.Compress(tt.messages)
			t2.Logf("Original: %d bytes, Compressed: %d bytes, Saved: %d bytes (%s)",
				result.Original, result.Compressed, result.Saved, result.Ratio)
		})
	}
}

func TestCacheHitRate(t *testing.T) {
	cfg := loadConfig()
	cache := NewCache(cfg)

	messages := makeMessages(20, 50)
	response := []byte(`{"content": "test response"}`)

	cache.TokenSet(messages, "test-model", nil, response)

	_, hit := cache.TokenGet(messages, "test-model", nil)
	if !hit {
		t.Error("Expected cache hit, got miss")
	}

	missMessages := makeMessages(20, 51)
	_, hit = cache.TokenGet(missMessages, "test-model", nil)
	if hit {
		t.Error("Expected cache miss, got hit")
	}

	stats := cache.TokenStats()
	t.Logf("Token cache stats: %+v", stats)
}

func TestSemanticCacheSimilarity(t *testing.T) {
	cfg := loadConfig()
	cache := NewCache(cfg)

	baseText := "How do I implement a binary search tree in Go?"
	cb, _ := json.Marshal(baseText)
	msgs1 := []Message{
		{Role: "user", Content: cb, Extra: make(map[string]json.RawMessage)},
	}
	cache.SemanticSet(msgs1, []byte(`{"content": "BST implementation"}`))

	similarText := "How can I create a binary search tree using Go?"
	cb2, _ := json.Marshal(similarText)
	msgs2 := []Message{
		{Role: "user", Content: cb2, Extra: make(map[string]json.RawMessage)},
	}

	_, score, found := cache.SemanticGet(msgs2)
	t.Logf("Semantic similarity score: %.4f, Found: %v", score, found)
}

func BenchmarkCacheConcurrency(b *testing.B) {
	cfg := loadConfig()
	cache := NewCache(cfg)
	messages := makeMessages(20, 50)
	response := []byte(`{"content": "test response"}`)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			if r.Intn(2) == 0 {
				cache.TokenSet(messages, "test-model", nil, response)
			} else {
				cache.TokenGet(messages, "test-model", nil)
			}
		}
	})
}
