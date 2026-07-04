package sieve

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// End-to-end: a request through the proxy should have all three output-policy
// levers composed on the wire — (1) reasoning effort downgraded, (2) max_tokens
// clamped, (3) conciseness directive appended — and (4) the upstream's reported
// output tokens land on /stats.
func TestProxyAppliesPolicyAndMeasuresUsage(t *testing.T) {
	var gotUpstreamBody map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotUpstreamBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"ok"}],` +
			`"usage":{"input_tokens":31,"output_tokens":17}}`))
	}))
	defer upstream.Close()

	cfg := &Config{TargetURL: upstream.URL}
	cfg.Output.Enabled = true
	cfg.Output.ReasoningProfile = "anthropic"
	cfg.Output.Effort = "low"
	cfg.Output.MaxOutputTokens = 512
	cfg.Output.TrimOutput = true
	cfg.Output.Style = "concise"
	// Skip body rewriting so we can assert on the forwarded body cleanly.
	cfg.Compression.Enabled = false
	// Realistic cache sizing (matches LoadConfig defaults); the write path runs
	// regardless of the Enabled flags.
	cfg.TokenCache.MaxEntries = 1000
	cfg.TokenCache.TTL = 3600
	cfg.SemanticCache.MaxEntries = 500
	cfg.SemanticCache.Threshold = 0.82

	srv := NewServer(cfg)
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","max_tokens":1024,`+
			`"output_config":{"effort":"high"},`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// (1) effort was downgraded high -> low before forwarding.
	var oc struct {
		Effort string `json:"effort"`
	}
	if raw, ok := gotUpstreamBody["output_config"]; ok {
		_ = json.Unmarshal(raw, &oc)
	}
	if oc.Effort != "low" {
		t.Errorf("upstream output_config.effort = %q, want low", oc.Effort)
	}

	// (2) max_tokens was clamped 1024 -> 512.
	var maxTok int
	_ = json.Unmarshal(gotUpstreamBody["max_tokens"], &maxTok)
	if maxTok != 512 {
		t.Errorf("upstream max_tokens = %d, want 512", maxTok)
	}

	// (3) the conciseness directive was appended to the (absent) system field.
	if !strings.Contains(string(gotUpstreamBody["system"]), "concise") {
		t.Errorf("upstream system = %s, want appended directive", gotUpstreamBody["system"])
	}

	// (4) output tokens were measured onto /stats.
	statsReq := httptest.NewRequest(http.MethodGet, "/stats", nil)
	statsRec := httptest.NewRecorder()
	handler.ServeHTTP(statsRec, statsReq)

	var stats struct {
		Global struct {
			InputTokens  int64 `json:"inputTokens"`
			OutputTokens int64 `json:"outputTokens"`
		} `json:"global"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Global.OutputTokens != 17 || stats.Global.InputTokens != 31 {
		t.Errorf("stats tokens = (in %d, out %d), want (31, 17)",
			stats.Global.InputTokens, stats.Global.OutputTokens)
	}
}
