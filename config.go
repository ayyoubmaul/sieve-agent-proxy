package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime settings, populated from environment variables
// (optionally seeded from a .env file).
type Config struct {
	Port      string
	TargetURL string

	// AuthProvider names the stored credential (see `login`) to inject when an
	// incoming request carries no Authorization / x-api-key header. Empty = off.
	AuthProvider string

	// AuthOverride makes the stored AuthProvider credential win over whatever
	// the client sent, instead of only filling in when the client sent nothing.
	// Lets a client (e.g. Claude Code, which insists on some key) point at sieve
	// with a throwaway key while sieve injects the real upstream credential.
	AuthOverride bool

	Compression struct {
		Enabled             bool
		SummarizeOldTurns   bool
		SummarizeAfterTurns int
		KeepRecentTurns     int
	}

	// ToolCompaction shrinks bloated tool output (JSON, logs, result sets)
	// before the generic compressor runs. Deterministic and content-addressed,
	// so it preserves the upstream prompt-cache prefix (see toolresult.go).
	ToolCompaction struct {
		Enabled   bool
		Retrieval bool // store originals + advertise the fetch tool in the marker
		Intent    bool // bias compaction by the user's latest message (pinned for stability)
		// FetchToolName is the tool name written into the compaction marker —
		// i.e. the name the MODEL must call to retrieve an original. An MCP host
		// prefixes the server name onto the bare tool, so a server named "sieve"
		// exposing the "fetch" tool surfaces it as "sieve_fetch". Override with
		// RETRIEVAL_TOOL_NAME when the host uses a different prefix (e.g.
		// "sieve_sieve_fetch" if the registered tool is itself named sieve_fetch).
		FetchToolName string
		Opts          compactOpts
	}

	TokenCache struct {
		Enabled    bool
		TTL        int // seconds
		MaxEntries int
		// NormalizeL1 enables the L1n tier: a secondary exact cache keyed by
		// a volatile-normalized hash (dates, UUIDs, hex, numbers stripped).
		// A hit means the conversation is structurally identical to a cached
		// one but differs only in injected volatile tokens — e.g. a system
		// prompt that includes today's date. Off by default because it can
		// serve a cached response for a prompt that differs in date, which is
		// only safe when the caller knows the date does not affect the answer.
		NormalizeL1 bool
	}

	SemanticCache struct {
		Enabled    bool
		Threshold  float64
		MaxEntries int
	}

	// Output shapes the *outbound* request to reduce generated output tokens,
	// which bill higher than input (e.g. Opus: $5/1M in vs $25/1M out). Unlike
	// the message compression above, this trades some response quality for
	// fewer output tokens, so it is opt-in and off by default.
	Output struct {
		Enabled          bool
		ReasoningProfile string // off | anthropic | openai | auto
		Effort           string // low | medium | high — downgrade target
		MaxOutputTokens  int    // clamp the per-response output ceiling (0 = off)
		TrimOutput       bool   // inject a conciseness directive
		Style            string // concise | terse
		Directive        string // full custom directive (overrides Style)
		TrimResponse     bool   // code-safe whitespace trim of the reply (non-stream)
	}

	// PromptAlign shapes the *outbound* request to raise the provider's own
	// prompt-cache hit rate (Anthropic cache_control discounts; OpenAI automatic
	// prefix caching). It is the complement of the L1/L1n/L2 response cache —
	// where that saves a whole call, this discounts the input tokens sieve still
	// sends — and the primary saver when the response cache is disabled. Opt-in.
	PromptAlign struct {
		Enabled        bool // master switch
		Inject         bool // inject Anthropic cache_control only when client set none
		Reorder        bool // hoist leading system content to front (conservative)
		Normalize      bool // byte-normalize the prefix (whitespace, key order)
		MaxBreakpoints int  // Anthropic honors max 4; 3 covers tools+system+history
		SetBeta        bool // merge prompt-caching beta header when injecting
	}
}

// loadDotEnv reads KEY=VALUE pairs from a .env file into the process
// environment, without overwriting variables that are already set.
// Lines starting with # and inline " #" comments are ignored.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		val = strings.Trim(val, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	return v != "false" && v != "0" && v != "no"
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

// toolCompactionSummary renders the tool-compaction settings for the banner.
func (c *Config) toolCompactionSummary() string {
	if !c.ToolCompaction.Enabled {
		return "❌ off"
	}
	o := c.ToolCompaction.Opts
	extra := ""
	if c.ToolCompaction.Intent {
		extra += " · intent"
	}
	if c.ToolCompaction.Retrieval {
		extra += " · retrieval"
	}
	return fmt.Sprintf("✅ on  (>%dB · str≤%d · arr≤%d · lines≤%d%s)",
		o.MinBytes, o.MaxStringRunes, o.MaxArrayItems, o.MaxLines, extra)
}

// alignSummary renders the active PromptAlign levers for the startup banner.
func (c *Config) alignSummary() string {
	if !c.PromptAlign.Enabled {
		return "❌ off"
	}
	var parts []string
	if c.PromptAlign.Normalize {
		parts = append(parts, "normalize")
	}
	if c.PromptAlign.Reorder {
		parts = append(parts, "reorder")
	}
	if c.PromptAlign.Inject {
		parts = append(parts, fmt.Sprintf("inject≤%d", c.PromptAlign.MaxBreakpoints))
	}
	if len(parts) == 0 {
		return "✅ on (no-op)"
	}
	return "✅ " + strings.Join(parts, " · ")
}

// LoadConfig reads .env (if present) and resolves all settings.
func LoadConfig() *Config {
	loadDotEnv(".env")

	c := &Config{
		Port:      env("PORT", "4141"),
		TargetURL: strings.TrimRight(env("TARGET_URL", "https://api.anthropic.com"), "/"),
	}

	c.AuthProvider = env("AUTH_PROVIDER", "")
	c.AuthOverride = envBool("AUTH_OVERRIDE", false)

	c.Compression.Enabled = envBool("COMPRESSION", true)
	c.Compression.SummarizeOldTurns = envBool("SUMMARIZE_OLD_TURNS", true)
	c.Compression.SummarizeAfterTurns = envInt("SUMMARIZE_AFTER", 10)
	c.Compression.KeepRecentTurns = envInt("KEEP_RECENT_TURNS", 4)

	d := defaultCompactOpts()
	c.ToolCompaction.Enabled = envBool("TOOL_COMPACTION", true)
	c.ToolCompaction.Retrieval = envBool("TOOL_RETRIEVAL", false)
	c.ToolCompaction.Intent = envBool("TOOL_INTENT", false)
	c.ToolCompaction.FetchToolName = env("RETRIEVAL_TOOL_NAME", "sieve_fetch")
	c.ToolCompaction.Opts = compactOpts{
		MinBytes:       envInt("TOOL_COMPACT_MIN_BYTES", d.MinBytes),
		MaxStringRunes: envInt("TOOL_COMPACT_MAX_STRING", d.MaxStringRunes),
		MaxArrayItems:  envInt("TOOL_COMPACT_MAX_ARRAY", d.MaxArrayItems),
		HeadItems:      d.HeadItems,
		TailItems:      d.TailItems,
		MaxLines:       envInt("TOOL_COMPACT_MAX_LINES", d.MaxLines),
		HeadLines:      d.HeadLines,
		TailLines:      d.TailLines,
	}

	c.TokenCache.Enabled = envBool("TOKEN_CACHE", true)
	c.TokenCache.TTL = envInt("TOKEN_CACHE_TTL", 3600)
	c.TokenCache.MaxEntries = envInt("TOKEN_CACHE_MAX", 1000)
	c.TokenCache.NormalizeL1 = envBool("CACHE_L1_NORMALIZE", false)

	c.SemanticCache.Enabled = envBool("SEMANTIC_CACHE", true)
	c.SemanticCache.Threshold = envFloat("SEMANTIC_THRESHOLD", 0.82)
	c.SemanticCache.MaxEntries = envInt("SEMANTIC_CACHE_MAX", 500)

	c.Output.Enabled = envBool("OUTPUT_POLICY", false)
	c.Output.ReasoningProfile = env("REASONING_PROFILE", "off")
	c.Output.Effort = env("ROUTINE_EFFORT", "")
	c.Output.MaxOutputTokens = envInt("MAX_OUTPUT_TOKENS", 0)
	c.Output.TrimOutput = envBool("TRIM_OUTPUT", false)
	c.Output.Style = env("OUTPUT_STYLE", "concise")
	c.Output.Directive = env("OUTPUT_DIRECTIVE", "")
	c.Output.TrimResponse = envBool("TRIM_RESPONSE", false)

	c.PromptAlign.Enabled = envBool("PROMPT_ALIGN", false)
	c.PromptAlign.Inject = envBool("PROMPT_ALIGN_INJECT", true)
	c.PromptAlign.Reorder = envBool("PROMPT_ALIGN_REORDER", true)
	c.PromptAlign.Normalize = envBool("PROMPT_ALIGN_NORMALIZE", true)
	c.PromptAlign.MaxBreakpoints = envInt("PROMPT_ALIGN_MAX_BREAKPOINTS", 3)
	c.PromptAlign.SetBeta = envBool("PROMPT_ALIGN_SET_BETA", true)

	return c
}
