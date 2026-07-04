package sieve

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed public
var publicFS embed.FS

// Stats tracks aggregate proxy metrics (guarded by mu).
type Stats struct {
	mu                   sync.Mutex
	Requests             int64
	TokenCacheHits       int64
	NormCacheHits        int64 // L1n: volatile-normalized key hits
	SemanticCacheHits    int64
	TotalCharsSaved      int64
	ToolResultCharsSaved int64
	TotalInputTokens     int64
	TotalOutputTokens    int64
	TotalCachedTokens    int64
	PromptAlignApplied   int64
	BreakpointsInjected  int64
	Forwarded            int64
	Errors               int64
	Sources              map[string]*srcStat // keyed by "Agent · Provider"
	StartedAt            time.Time
}

type Server struct {
	cfg    *Config
	comp   *Compressor
	cache  *Cache
	stats  *Stats
	auth   *Auth
	store  *Store    // non-nil only when tool-result retrieval is enabled
	pin    *pinCache // non-nil only when intent-aware compaction is enabled
	client *http.Client
}

func NewServer(cfg *Config) *Server {
	var store *Store
	if cfg.ToolCompaction.Retrieval {
		store = NewStore()
	}
	var pin *pinCache
	if cfg.ToolCompaction.Intent {
		pin = newPinCache()
	}
	return &Server{
		cfg:   cfg,
		comp:  NewCompressor(cfg),
		cache: NewCache(cfg),
		stats: &Stats{StartedAt: time.Now()},
		auth:  LoadAuth(),
		store: store,
		pin:   pin,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:              http.ProxyFromEnvironment,
				ForceAttemptHTTP2:  true,
				DisableCompression: true, // keep SSE streams raw
				MaxIdleConns:       100,
			},
		},
	}
}

// Handler wires up all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/cache/clear", s.handleClear)
	mux.HandleFunc("/sieve/fetch", s.handleFetch)

	mux.HandleFunc("/v1/messages", s.handleLLM)         // Anthropic
	mux.HandleFunc("/v1/chat/completions", s.handleLLM) // OpenAI / OpenCode
	mux.HandleFunc("/chat/completions", s.handleLLM)    // no-prefix variant (sieve as bare proxy)

	sub, _ := fs.Sub(publicFS, "public")
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.FS(sub))))
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
	})

	mux.HandleFunc("/", s.handlePassthrough) // transparent fallback

	return s.withCORS(mux)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Monitoring ───────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"uptime":   time.Since(s.stats.StartedAt).Milliseconds(),
		"requests": s.stats.Requests,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.stats.mu.Lock()
	// Prompt-cache hit ratio: the share of total prompt tokens the provider
	// served from its prompt cache. This is the number the goal targets —
	// deterministic tool compaction keeps the prompt prefix byte-stable so this
	// ratio stays high across an agent's growing conversation.
	//
	// NB: providers report cached tokens *separately* from input_tokens, not as
	// a subset — input_tokens counts only the fresh (uncached) tokens. So the
	// denominator must be input + cached, or the ratio blows past 100%.
	cacheHitRatio := 0.0
	if total := s.stats.TotalInputTokens + s.stats.TotalCachedTokens; total > 0 {
		cacheHitRatio = float64(s.stats.TotalCachedTokens) / float64(total) * 100
	}
	global := map[string]interface{}{
		"requests":            s.stats.Requests,
		"tokenCacheHits":      s.stats.TokenCacheHits,
		"normCacheHits":       s.stats.NormCacheHits,
		"semanticCacheHits":   s.stats.SemanticCacheHits,
		"totalCharsSaved":     s.stats.TotalCharsSaved,
		"toolCharsSaved":      s.stats.ToolResultCharsSaved,
		"inputTokens":         s.stats.TotalInputTokens,
		"outputTokens":        s.stats.TotalOutputTokens,
		"cachedTokens":        s.stats.TotalCachedTokens,
		"cacheHitRatio":       cacheHitRatio,
		"promptAlignApplied":  s.stats.PromptAlignApplied,
		"breakpointsInjected": s.stats.BreakpointsInjected,
		"forwarded":           s.stats.Forwarded,
		"errors":              s.stats.Errors,
		"uptime":              time.Since(s.stats.StartedAt).Milliseconds(),
	}
	// Snapshot request sources (descending by request count) while locked.
	sources := make([]*srcStat, 0, len(s.stats.Sources))
	for _, v := range s.stats.Sources {
		cp := *v
		sources = append(sources, &cp)
	}
	s.stats.mu.Unlock()

	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Requests > sources[j].Requests
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"global":        global,
		"sources":       sources,
		"tokenCache":    s.cache.TokenStats(),
		"normCache":     s.cache.NormStats(),
		"semanticCache": s.cache.SemanticStats(),
	})
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	s.cache.Clear()
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "message": "All caches cleared."})
}

// handleFetch returns the full original payload for a compaction ref. It backs
// both the sieve_fetch MCP tool and manual lookups (?ref=...). Retrieval must be
// enabled (TOOL_RETRIEVAL) for the store to exist and refs to have been saved.
func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "retrieval disabled (set TOOL_RETRIEVAL=true)"})
		return
	}
	ref := r.URL.Query().Get("ref")
	if orig, ok := s.store.Get(ref); ok {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(orig))
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown ref"})
}

// ── Core LLM handler ─────────────────────────────────────────────────────

func (s *Server) handleLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	t0 := time.Now()
	s.incr(func(st *Stats) { st.Requests++ })

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read body"})
		return
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	isAnthropic := r.URL.Path == "/v1/messages"

	var messages []Message
	if raw, ok := body["messages"]; ok {
		_ = json.Unmarshal(raw, &messages)
	}
	var model string
	if raw, ok := body["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	var stream bool
	if raw, ok := body["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}

	// Attribute the request to its calling agent + upstream provider for the
	// dashboard's "Request Sources" panel.
	s.recordSource(detectAgent(r), detectProvider(isAnthropic, model))

	// Extra request fields that must differentiate cache entries beyond
	// messages+model (different tools or system prompt => different response).
	// This is the json.RawMessage your cache.go's TokenGet/TokenSet expect.
	cacheKeyExtra, _ := json.Marshal(map[string]json.RawMessage{
		"system": body["system"],
		"tools":  body["tools"],
	})

	log.Printf("→ %s | model=%s msgs=%d stream=%v", r.URL.Path, model, len(messages), stream)

	// ── L1: token cache (exact) ──────────────────────────────────────────
	if s.cfg.TokenCache.Enabled {
		if cached, ok := s.cache.TokenGet(messages, model, cacheKeyExtra); ok {
			s.incr(func(st *Stats) { st.TokenCacheHits++ })
			log.Printf("  ✅ L1 hit (%dms)", time.Since(t0).Milliseconds())
			if stream {
				s.replayStream(w, cached, isAnthropic)
			} else {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(cached)
			}
			return
		}
	}

	// ── L1n: volatile-normalised token cache (opt-in) ─────────────────────
	// Hits when the conversation is structurally identical to a cached one but
	// differs only in injected volatile tokens (e.g. a system prompt that
	// includes today's date). Enable with CACHE_L1_NORMALIZE=true only when
	// you know those volatile tokens don't change the correct answer.
	if s.cfg.TokenCache.Enabled && s.cfg.TokenCache.NormalizeL1 {
		if cached, ok := s.cache.NormGet(messages, model, cacheKeyExtra); ok {
			s.incr(func(st *Stats) { st.NormCacheHits++ })
			log.Printf("  ✅ L1n hit (%dms)", time.Since(t0).Milliseconds())
			if stream {
				s.replayStream(w, cached, isAnthropic)
			} else {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(cached)
			}
			return
		}
	}

	// ── L2: semantic cache (non-stream) ──────────────────────────────────
	if s.cfg.SemanticCache.Enabled && !stream {
		if cached, sim, ok := s.cache.SemanticGet(messages); ok {
			s.incr(func(st *Stats) { st.SemanticCacheHits++ })
			log.Printf("  🧠 L2 hit similarity=%.1f%% (%dms)", sim*100, time.Since(t0).Milliseconds())
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cached)
			return
		}
	}

	// ── Compact tool results ──────────────────────────────────────────────
	// Content-aware shrinking of tool output (bloated JSON, repeated log lines,
	// long result sets) — the densest, least useful bytes a coding agent
	// resends every turn. Runs before the generic compressor. Deterministic, so
	// an already-seen tool result stays byte-identical across turns and the
	// upstream prompt-cache prefix is preserved.
	working := messages
	if s.cfg.ToolCompaction.Enabled && len(working) > 0 {
		opt := s.cfg.ToolCompaction.Opts
		var ctx *compactCtx
		if s.store != nil || s.pin != nil {
			ctx = &compactCtx{store: s.store, pin: s.pin, fetchTool: s.cfg.ToolCompaction.FetchToolName}
		}
		if s.pin != nil { // intent mode: bias by the user's latest message
			opt.Intent = extractIntent(working)
		}
		if compacted, saved := compactToolResults(working, opt, ctx); saved > 0 {
			working = compacted
			s.incr(func(st *Stats) {
				st.ToolResultCharsSaved += int64(saved)
				st.TotalCharsSaved += int64(saved)
			})
			log.Printf("  ✂️  tool results −%d chars", saved)
		}
	}

	// ── Compress ─────────────────────────────────────────────────────────
	finalMessages := working
	if s.cfg.Compression.Enabled && len(working) > 0 {
		res := s.comp.Compress(working)
		finalMessages = res.Messages
		if res.Saved > 0 {
			s.incr(func(st *Stats) { st.TotalCharsSaved += int64(res.Saved) })
			log.Printf("  📦 %d→%d chars (%s%% saved)", res.Original, res.Compressed, res.Ratio)
		}
	}
	if fm, err := json.Marshal(finalMessages); err == nil {
		body["messages"] = fm
	}

	// ── Output policy: shape the request to reduce generated output tokens ──
	if s.cfg.Output.Enabled {
		s.applyOutputPolicy(body, isAnthropic)
	}

	// ── PromptAlign: raise the provider's prompt-cache hit rate ────────────
	// Runs last, on the final byte-stable body, so any appended directive is
	// inside the cached prefix and breakpoints mark the true stable/volatile
	// boundary. Discounts the input tokens sieve still sends — the primary saver
	// when the L1/L1n/L2 response cache is off.
	var alignRes alignResult
	if s.cfg.PromptAlign.Enabled {
		alignRes = s.applyPromptAlign(body, isAnthropic)
		if alignRes.applied {
			s.incr(func(st *Stats) {
				st.PromptAlignApplied++
				st.BreakpointsInjected += int64(alignRes.breakpoints)
			})
			if alignRes.breakpoints > 0 {
				log.Printf("  🎯 PromptAlign +%d breakpoint(s)", alignRes.breakpoints)
			}
		}
	}
	outBody, _ := json.Marshal(body)

	// ── Forward ──────────────────────────────────────────────────────────
	up := s.resolveUpstream(r)
	targetURL := up.Target + r.URL.Path
	log.Printf("  📡 → %s", targetURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(outBody))
	if err != nil {
		s.incr(func(st *Stats) { st.Errors++ })
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.applyHeaders(req, r, isAnthropic, up)

	// Ensure the prompt-caching beta flag is present when PromptAlign injected
	// breakpoints, so older Anthropic endpoints honor them (GA endpoints ignore
	// the flag). Client-managed breakpoints are left to the client's own header.
	if isAnthropic && s.cfg.PromptAlign.SetBeta && alignRes.breakpoints > 0 {
		req.Header.Set("anthropic-beta", mergeBeta(req.Header.Get("anthropic-beta"), "prompt-caching-2024-07-31"))
	}

	if stream {
		s.proxyStream(w, req, messages, model, cacheKeyExtra, isAnthropic, t0)
		return
	}

	resp, err := s.client.Do(req)
	if err != nil {
		s.incr(func(st *Stats) { st.Errors++ })
		log.Printf("  ❌ %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("  ❌ upstream %d", resp.StatusCode)
		copyHeader(w, resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	// Measure usage on the original body (the model was billed for what it
	// generated), then trim the reply for the client + cache.
	if u := usageFromResponse(respBody, isAnthropic); u.any() {
		s.recordUsage(u)
	}
	respBody = s.trimResponse(respBody, isAnthropic)
	s.cache.TokenSet(messages, model, cacheKeyExtra, respBody)
	if s.cfg.TokenCache.NormalizeL1 {
		s.cache.NormSet(messages, model, cacheKeyExtra, respBody)
	}
	s.cache.SemanticSet(messages, respBody)
	s.incr(func(st *Stats) { st.Forwarded++ })
	log.Printf("  ✅ %dms", time.Since(t0).Milliseconds())

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBody)
}

// proxyStream pipes an upstream SSE stream to the client, accumulating it so
// the completed response can be cached once the stream finishes cleanly.
func (s *Server) proxyStream(w http.ResponseWriter, req *http.Request, origMessages []Message, model string, cacheKeyExtra json.RawMessage, isAnthropic bool, t0 time.Time) {
	resp, err := s.client.Do(req)
	if err != nil {
		s.incr(func(st *Stats) { st.Errors++ })
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		copyHeader(w, resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	var acc bytes.Buffer
	buf := make([]byte, 8192)

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = w.Write(chunk)
			acc.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	raw := acc.String()
	complete := (isAnthropic && strings.Contains(raw, "message_stop")) ||
		(!isAnthropic && strings.Contains(raw, "[DONE]"))

	if complete {
		if parsed := parseStreamedResponse(raw, isAnthropic); parsed != nil {
			s.cache.TokenSet(origMessages, model, cacheKeyExtra, parsed)
			if s.cfg.TokenCache.NormalizeL1 {
				s.cache.NormSet(origMessages, model, cacheKeyExtra, parsed)
			}
			s.cache.SemanticSet(origMessages, parsed)
			s.incr(func(st *Stats) { st.Forwarded++ })
		}
		if u := streamUsage(raw, isAnthropic); u.any() {
			s.recordUsage(u)
		}
	}
	log.Printf("  ✅ stream done (%dms)", time.Since(t0).Milliseconds())
}

// replayStream re-emits a cached full response as an SSE stream so streaming
// clients receive the format they expect even on a cache hit.
func (s *Server) replayStream(w http.ResponseWriter, cached []byte, isAnthropic bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	const chunkSize = 80

	if isAnthropic {
		var obj struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(cached, &obj)

		text := ""
		if len(obj.Content) > 0 {
			text = obj.Content[0].Text
		}
		id := obj.ID
		if id == "" {
			id = fmt.Sprintf("msg_cached_%d", time.Now().UnixNano())
		}

		emit(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id": id, "type": "message", "role": "assistant",
				"content": []interface{}{}, "model": obj.Model,
			},
		})
		emit(w, flusher, "content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		for _, piece := range chunkString(text, chunkSize) {
			emit(w, flusher, "content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]interface{}{"type": "text_delta", "text": piece},
			})
		}
		emit(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
		emit(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta", "delta": map[string]interface{}{"stop_reason": "end_turn"},
		})
		emit(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
		return
	}

	// OpenAI format
	var obj struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(cached, &obj)

	text := ""
	if len(obj.Choices) > 0 {
		text = obj.Choices[0].Message.Content
	}
	id := obj.ID
	if id == "" {
		id = fmt.Sprintf("chatcmpl-cached-%d", time.Now().UnixNano())
	}
	created := time.Now().Unix()

	frame := func(delta map[string]interface{}, finish interface{}) string {
		payload := map[string]interface{}{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": obj.Model,
			"choices": []interface{}{map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(payload)
		return string(b)
	}

	writeSSE(w, flusher, frame(map[string]interface{}{"role": "assistant", "content": ""}, nil))
	for _, piece := range chunkString(text, chunkSize) {
		writeSSE(w, flusher, frame(map[string]interface{}{"content": piece}, nil))
	}
	writeSSE(w, flusher, frame(map[string]interface{}{}, "stop"))
	writeSSE(w, flusher, "[DONE]")
}

// ── Transparent passthrough ──────────────────────────────────────────────

func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	s.incr(func(st *Stats) { st.Requests++ })
	targetURL := s.resolveUpstream(r).Target + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	var bodyReader io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		b, _ := io.ReadAll(r.Body)
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bodyReader)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	for k, vals := range r.Header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	copyHeader(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ── Header / utility helpers ─────────────────────────────────────────────

func (s *Server) applyHeaders(req *http.Request, orig *http.Request, isAnthropic bool, up Upstream) {
	req.Header.Set("Content-Type", "application/json")

	// Anthropic version header is always passed through / defaulted.
	if isAnthropic {
		ver := orig.Header.Get("anthropic-version")
		if ver == "" {
			ver = "2023-06-01"
		}
		req.Header.Set("anthropic-version", ver)
		if beta := orig.Header.Get("anthropic-beta"); beta != "" {
			req.Header.Set("anthropic-beta", beta)
		}
		// Forward the identity headers Anthropic's OAuth enforcement inspects.
		// A fresh request is built upstream, so anything not copied here is lost
		// — and a missing x-app / browser-access / User-Agent makes a genuine
		// Claude Code OAuth token look like third-party traffic and get rejected.
		for _, h := range []string{"x-app", "anthropic-dangerous-direct-browser-access", "User-Agent"} {
			if v := orig.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
	}

	// 0. Override mode: the stored AUTH_PROVIDER credential wins over whatever
	// the client sent. Lets a client point at sieve with a throwaway key while
	// sieve injects the real upstream credential.
	if up.AuthOverride && s.auth != nil && up.AuthProvider != "" {
		req.Header.Del("x-api-key")
		req.Header.Del("Authorization")
		if err := s.auth.Inject(req, up.AuthProvider); err != nil {
			log.Printf("  ⚠️  auth: %v (set AUTH_PROVIDER + run `login`)", err)
		}
		return
	}

	// 1. A credential supplied by the client always wins (transparent passthrough).
	apiKey := orig.Header.Get("x-api-key")
	if apiKey == "" {
		auth := orig.Header.Get("Authorization")
		if len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
			apiKey = strings.TrimSpace(auth[7:])
		} else {
			apiKey = strings.TrimSpace(auth)
		}
	}
	if apiKey != "" {
		// OAuth subscription tokens (sk-ant-oat…) must travel as a Bearer token
		// with the oauth beta flag — Anthropic rejects them as x-api-key. Only
		// real API keys (sk-ant-api…) go in x-api-key.
		switch {
		case isAnthropic && strings.HasPrefix(apiKey, "sk-ant-oat"):
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("anthropic-beta", mergeBeta(req.Header.Get("anthropic-beta"), "oauth-2025-04-20"))
		case isAnthropic:
			req.Header.Set("x-api-key", apiKey)
		default:
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		return
	}

	// 2. No client credential — inject the stored one for the upstream (if any).
	if s.auth != nil && up.AuthProvider != "" {
		if err := s.auth.Inject(req, up.AuthProvider); err != nil {
			log.Printf("  ⚠️  auth: %v (set AUTH_PROVIDER + run `login`)", err)
		}
	}
}

// resolveUpstream picks the routing profile for a request: the X-Sieve-Upstream
// header if it names a configured profile, otherwise the default profile.
func (s *Server) resolveUpstream(r *http.Request) Upstream {
	if name := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Sieve-Upstream"))); name != "" {
		if up, ok := s.cfg.Upstreams[name]; ok {
			return up
		}
		log.Printf("  ⚠️  unknown upstream %q — using default", name)
	}
	if up, ok := s.cfg.Upstreams[s.cfg.DefaultUpstream]; ok {
		return up
	}
	// Fallback for a Config built without loadUpstreams (e.g. tests): route to
	// the top-level target/auth directly.
	return Upstream{Target: s.cfg.TargetURL, AuthProvider: s.cfg.AuthProvider, AuthOverride: s.cfg.AuthOverride}
}

// mergeBeta returns the anthropic-beta header value with flag guaranteed
// present, preserving any flags the client already sent (order-insensitive).
func mergeBeta(existing, flag string) string {
	if existing == "" {
		return flag
	}
	for _, f := range strings.Split(existing, ",") {
		if strings.TrimSpace(f) == flag {
			return existing
		}
	}
	return existing + "," + flag
}

func (s *Server) incr(f func(*Stats)) {
	s.stats.mu.Lock()
	f(s.stats)
	s.stats.mu.Unlock()
}

// recordSource attributes one request to an (agent, provider) pair.
func (s *Server) recordSource(agent, provider string) {
	key := agent + " · " + provider
	s.stats.mu.Lock()
	if s.stats.Sources == nil {
		s.stats.Sources = map[string]*srcStat{}
	}
	e := s.stats.Sources[key]
	if e == nil {
		e = &srcStat{Agent: agent, Provider: provider}
		s.stats.Sources[key] = e
	}
	e.Requests++
	s.stats.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func copyHeader(dst http.ResponseWriter, src http.Header) {
	for k, vals := range src {
		if k == "Content-Length" || k == "Transfer-Encoding" || k == "Connection" {
			continue
		}
		for _, v := range vals {
			dst.Header().Add(k, v)
		}
	}
}

func emit(w http.ResponseWriter, f http.Flusher, event string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if f != nil {
		f.Flush()
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f != nil {
		f.Flush()
	}
}

func chunkString(s string, size int) []string {
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

// parseStreamedResponse reconstructs a complete response object from a raw SSE
// transcript. Returns nil if nothing usable was parsed.
func parseStreamedResponse(raw string, isAnthropic bool) []byte {
	lines := strings.Split(raw, "\n")

	if isAnthropic {
		var text strings.Builder
		var model, id string
		for _, line := range lines {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var d map[string]interface{}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) != nil {
				continue
			}
			switch d["type"] {
			case "message_start":
				if msg, ok := d["message"].(map[string]interface{}); ok {
					if m, ok := msg["model"].(string); ok {
						model = m
					}
					if i, ok := msg["id"].(string); ok {
						id = i
					}
				}
			case "content_block_delta":
				if delta, ok := d["delta"].(map[string]interface{}); ok {
					if delta["type"] == "text_delta" {
						if t, ok := delta["text"].(string); ok {
							text.WriteString(t)
						}
					}
				}
			}
		}
		if text.Len() == 0 && id == "" {
			return nil
		}
		obj := map[string]interface{}{
			"id": id, "type": "message", "role": "assistant",
			"content":     []interface{}{map[string]interface{}{"type": "text", "text": text.String()}},
			"model":       model,
			"stop_reason": "end_turn",
		}
		b, _ := json.Marshal(obj)
		return b
	}

	// OpenAI format
	var text strings.Builder
	var model, id string
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var d map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) != nil {
			continue
		}
		if id == "" {
			if i, ok := d["id"].(string); ok {
				id = i
			}
		}
		if model == "" {
			if m, ok := d["model"].(string); ok {
				model = m
			}
		}
		if choices, ok := d["choices"].([]interface{}); ok && len(choices) > 0 {
			if ch, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := ch["delta"].(map[string]interface{}); ok {
					if c, ok := delta["content"].(string); ok {
						text.WriteString(c)
					}
				}
			}
		}
	}
	if text.Len() == 0 && id == "" {
		return nil
	}
	obj := map[string]interface{}{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []interface{}{map[string]interface{}{
			"index": 0, "message": map[string]interface{}{"role": "assistant", "content": text.String()},
			"finish_reason": "stop",
		}},
	}
	b, _ := json.Marshal(obj)
	return b
}
