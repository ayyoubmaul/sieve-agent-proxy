package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// End-to-end: a /v1/messages request whose history carries a bloated tool_result
// must reach the upstream with that result compacted, and /stats must report the
// characters saved. Other messages and fields are left intact.
func TestProxyCompactsToolResults(t *testing.T) {
	var forwarded map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &forwarded)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"ok"}],` +
			`"usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	cfg := &Config{TargetURL: upstream.URL}
	cfg.ToolCompaction.Enabled = true
	cfg.ToolCompaction.Opts = defaultCompactOpts()
	cfg.Compression.Enabled = false // isolate the compaction stage
	cfg.TokenCache.MaxEntries = 1000
	cfg.TokenCache.TTL = 3600
	cfg.SemanticCache.MaxEntries = 500
	cfg.SemanticCache.Threshold = 0.82

	handler := NewServer(cfg).Handler()

	var log strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&log, "2024-01-01T10:00:%02d WARN connection retry to upstream node-7\n", i%60)
	}
	logJSON, _ := json.Marshal(log.String())
	reqBody := fmt.Sprintf(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[
		{"role":"user","content":[{"type":"text","text":"check the logs"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"cmd":"cat log"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":%s}]}
	]}`, logJSON)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var msgs []Message
	if err := json.Unmarshal(forwarded["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("forwarded %d messages, want 3", len(msgs))
	}
	// The assistant tool_use message in the middle must be untouched.
	if !strings.Contains(string(msgs[1].Content), "toolu_1") {
		t.Error("assistant tool_use message was altered")
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(msgs[2].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	var got string
	_ = json.Unmarshal(blocks[0]["content"], &got)
	if len(got) >= len(log.String()) {
		t.Errorf("forwarded tool_result not compacted: %d >= %d", len(got), log.Len())
	}
	if !strings.Contains(got, "×") {
		t.Errorf("expected a run-collapse marker, got:\n%s", got)
	}

	statsRec := httptest.NewRecorder()
	handler.ServeHTTP(statsRec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	var stats struct {
		Global struct {
			ToolCharsSaved int64 `json:"toolCharsSaved"`
		} `json:"global"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Global.ToolCharsSaved <= 0 {
		t.Errorf("toolCharsSaved = %d, want > 0", stats.Global.ToolCharsSaved)
	}
}

// End-to-end retrieval: with TOOL_RETRIEVAL on, the forwarded tool_result carries
// a sieve_fetch(ref) marker; GET /sieve/fetch?ref= returns the original; and a
// separate Store opened on the same dir (as the `sieve mcp` process would) reads
// the very same bytes — proving the proxy and MCP coordinate through disk.
func TestProxyRetrievalRoundTrip(t *testing.T) {
	t.Setenv("SIEVE_STORE_DIR", t.TempDir())

	var forwarded map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &forwarded)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	cfg := &Config{TargetURL: upstream.URL}
	cfg.ToolCompaction.Enabled = true
	cfg.ToolCompaction.Retrieval = true
	cfg.ToolCompaction.Opts = defaultCompactOpts()
	cfg.TokenCache.MaxEntries = 1000
	cfg.TokenCache.TTL = 3600
	cfg.SemanticCache.MaxEntries = 500
	cfg.SemanticCache.Threshold = 0.82
	handler := NewServer(cfg).Handler()

	original := strings.Repeat("2024-01-01T10:00:00Z INFO long original tool output line\n", 300)
	origJSON, _ := json.Marshal(original)
	reqBody := fmt.Sprintf(`{"model":"m","max_tokens":16,"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":%s}]}
	]}`, origJSON)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// Decode the forwarded tool_result text, then pull the ref out of its marker.
	var fmsgs []Message
	if err := json.Unmarshal(forwarded["messages"], &fmsgs); err != nil {
		t.Fatal(err)
	}
	var fblocks []map[string]json.RawMessage
	_ = json.Unmarshal(fmsgs[0].Content, &fblocks)
	var compacted string
	_ = json.Unmarshal(fblocks[0]["content"], &compacted)

	ref := regexp.MustCompile(`sieve_fetch\("([0-9a-f]{12})"\)`).FindStringSubmatch(compacted)
	if ref == nil {
		t.Fatalf("no sieve_fetch marker in forwarded tool_result: %s", compacted)
	}

	// (1) HTTP fetch endpoint returns the original.
	fetchRec := httptest.NewRecorder()
	handler.ServeHTTP(fetchRec, httptest.NewRequest(http.MethodGet, "/sieve/fetch?ref="+ref[1], nil))
	if fetchRec.Code != http.StatusOK || fetchRec.Body.String() != original {
		t.Errorf("/sieve/fetch returned code=%d, %d bytes (want %d)", fetchRec.Code, fetchRec.Body.Len(), len(original))
	}

	// (2) A separate Store on the same dir (the MCP process) reads the same bytes.
	if got, ok := NewStore().Get(ref[1]); !ok || got != original {
		t.Errorf("independent store could not read ref %q (ok=%v)", ref[1], ok)
	}
}
