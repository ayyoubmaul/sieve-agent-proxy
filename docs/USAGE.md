# Sieve — How-To Guide

A practical, step-by-step walkthrough of every feature: input compression,
two-level caching, output-token measurement + reduction, agent wrapping, and
login auth. Every command and config key here maps directly to the code.

> Sieve is a single-binary proxy that sits between your LLM client (or coding
> agent) and any AI API. It cuts token cost through compression, caching, and
> request/response shaping — all controlled by environment variables.

---

## Table of contents

1. [Build](#1-build)
2. [Run the proxy](#2-run-the-proxy)
3. [Input compression, tool-result compaction + caching (on by default)](#3-input-compression--caching-on-by-default)
4. [Output-token measurement (always on)](#4-output-token-measurement-always-on)
5. [Output-token reduction (opt-in)](#5-output-token-reduction-opt-in)
6. [PromptAlign — provider-side prompt cache (opt-in)](#6-promptalign--provider-side-prompt-cache-opt-in)
7. [Wrap a coding agent](#7-wrap-a-coding-agent)
8. [Login once (skip pasting keys)](#8-login-once-skip-pasting-keys)
9. [Monitor & manage](#9-monitor--manage)
10. [Cheat sheet](#10-cheat-sheet)
11. [Full config reference](#11-full-config-reference)

---

## 1. Build

```bash
cd sieve
go build -o sieve .          # produces ./sieve
# or: make build            # produces ./llm-compress-proxy
```

Create your config from the template:

```bash
cp .env.example .env
```

Sieve reads `.env` from the working directory on startup. All examples below use
`./sieve`. Requires Go ≥ 1.21 to build — nothing else.

---

## 2. Run the proxy

```bash
./sieve                      # equivalently: ./sieve serve
```

The startup banner shows what's active:

```
Listening  → http://localhost:4141
Target     → https://api.anthropic.com
Compress   → ✅ on
Output     → ❌ off
Token $    → ✅ on  (TTL 3600s, max 1000)
Semantic $ → ✅ on  (threshold 0.82)
Dashboard  → http://localhost:4141/dashboard
```

Send traffic through it one of two ways:

- **Point a client at it** — set the client's base URL:
  - Anthropic clients → `http://localhost:4141` (hits `/v1/messages`)
  - OpenAI clients → `http://localhost:4141/v1` (hits `/v1/chat/completions`)
- **Wrap an agent** — see [step 6](#6-wrap-a-coding-agent); it starts the proxy
  *and* points the agent at it.

The proxy auto-detects the wire format from the path and forwards to `TARGET_URL`.
Clients send their own API key, which sieve forwards (or use [login](#7-login-once-skip-pasting-keys)).

**Quick functional check:**

```bash
curl -s http://localhost:4141/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -d '{"model":"claude-opus-4-8","max_tokens":1024,
       "messages":[{"role":"user","content":"hi"}]}'
```

The proxy log shows the pipeline: `📦 compression`, `✅ L1 hit`, `📡 → upstream`, `✅ done`.

---

## 3. Input compression + caching (on by default)

These need no action — they're on out of the box and shrink what the model
**reads**:

| Feature | Keys (defaults) | What it does |
|---|---|---|
| Tool-result compaction | `TOOL_COMPACTION=true`, `TOOL_COMPACT_MIN_BYTES=1024` | Shrinks bloated JSON / logs / SQL dumps in tool output |
| Whitespace + dedup + old-turn summary | `COMPRESSION=true`, `SUMMARIZE_AFTER=10`, `KEEP_RECENT_TURNS=4` | Code-safe input shrink |
| L1 exact cache | `TOKEN_CACHE=true`, `TOKEN_CACHE_TTL=3600`, `TOKEN_CACHE_MAX=1000` | Identical prompts return instantly |
| L2 semantic cache | `SEMANTIC_CACHE=true`, `SEMANTIC_THRESHOLD=0.82`, `SEMANTIC_CACHE_MAX=500` | Reworded duplicates hit too |

For fully lossless input handling, set `SUMMARIZE_OLD_TURNS=false` (whitespace +
dedup only — never summarizes old turns).

### Tool-result compaction

The single biggest source of wasted tokens for a coding agent is **tool output**
re-sent every turn — huge JSON, repeated log lines, hundred-row SQL dumps. sieve
compacts it content-aware and **deterministically** (so an already-seen result
stays byte-identical across turns and your Anthropic prompt-cache prefix keeps
hitting):

- **JSON** keeps all keys + structure, elides long string values and caps big arrays — stays valid JSON.
- **Logs** dedup repeated line-shapes (timestamps/ids normalized) into `…[×N]`.
- **SQL / tables** keep the header + first/last rows, eliding the middle.

Only results larger than `TOOL_COMPACT_MIN_BYTES` are touched; everything smaller
passes through untouched. Watch the effect on `/stats` (`toolCharsSaved`) and the
dashboard's "Tokens Saved" card.

Set `TOOL_INTENT=true` to bias what's kept by your latest message — fields, log
lines, and rows matching your prompt are kept fuller, the rest trimmed harder. It
stays cache-safe by pinning each result's compacted bytes on first sight, so
they don't change across turns.

### Retrieval / context rolling (opt-in)

Compaction is lossy, so with `TOOL_RETRIEVAL=true` the full original of every
compacted result is saved to `~/.sieve/store` and the compacted form carries a
`call sieve_fetch("<ref>")` marker. Register the fetch tool so the agent can pull
an original back **on demand** — paying those tokens only when it actually needs
them, not every turn:

```bash
claude mcp add sieve -- sieve mcp     # exposes the fetch tool as "sieve_fetch"
# then run with retrieval on:
TOOL_RETRIEVAL=true sieve wrap claude
```

The proxy (writer) and `sieve mcp` (reader) share only the on-disk store, so
nothing extra needs to run. You can also fetch manually:
`curl "http://localhost:4141/sieve/fetch?ref=<ref>"`.

> **Make the marker name match the tool the agent sees.** The tool is registered
> as the bare name `fetch`; the MCP host prefixes the server name, so a server
> added as `sieve` surfaces it as `sieve_fetch` — exactly what the marker prints
> by default. If your host shows a different name (e.g. a legacy
> `sieve_sieve_fetch`), set `RETRIEVAL_TOOL_NAME` to that so the model is told to
> call a tool that actually exists. This applies identically to OpenAI- and
> Anthropic-format agents (the marker is the same text in both). The `sieve mcp`
> server accepts both `fetch` and `sieve_fetch`.

---

## 4. Output-token measurement (always on)

Every response's token `usage` is parsed automatically — no config. Output tokens
matter because they bill several times higher than input (e.g. Opus: $5/1M in vs
$25/1M out). Check the numbers:

```bash
curl -s http://localhost:4141/stats | python3 -m json.tool
```

```json
{
  "global": {
    "requests": 12,
    "inputTokens": 48210,
    "outputTokens": 9134,
    "totalCharsSaved": 31022,
    "tokenCacheHits": 3
  }
}
```

Or open `http://localhost:4141/dashboard` — there are **Output Tokens** /
**Input Tokens** tiles. Use this as your baseline *before* turning on the
reducers in step 5, then compare after.

---

## 5. Output-token reduction (opt-in)

A proxy can't un-bill tokens the model already generated, so the levers either
shape the **request** (so the model writes less) or trim the **reply** (to shrink
next turn's input). All four are **off by default** and gated behind one master
switch. These trade some response quality for fewer tokens — that's why they're
opt-in.

Turn on the master switch in `.env`:

```bash
OUTPUT_POLICY=true           # required for any lever below
```

### Lever 1 — Reasoning-effort downgrade (the biggest real saver)

Lowers thinking/reasoning effort; thinking tokens bill as output. **Pick the
profile that matches your `TARGET_URL`** — sending the wrong field to a model
that doesn't accept it can be rejected.

```bash
# Claude target (Opus 4.5+/Sonnet 4.6/Fable 5):
REASONING_PROFILE=anthropic   # sets output_config.effort
ROUTINE_EFFORT=low            # low | medium | high — only ever LOWERS, never raises

# OpenAI reasoning models instead:
# REASONING_PROFILE=openai    # sets reasoning_effort
# ROUTINE_EFFORT=low

# auto: derive from wire format (only if every target accepts the field)
# REASONING_PROFILE=auto
```

### Lever 2 — `max_tokens` clamp (safety rail)

Caps runaway generations. It **truncates** rather than making the model concise,
so set it **generously**:

```bash
MAX_OUTPUT_TOKENS=8000        # 0 = off. Never raises, and never injects a cap the client omitted.
```

### Lever 3 — Conciseness directive (drops ceremony / restated code)

Injected cache-safely — appended as a trailing `system` block (Anthropic) or
trailing system message (OpenAI), never ahead of your cached prefix:

```bash
TRIM_OUTPUT=true
OUTPUT_STYLE=concise          # concise | terse
# OUTPUT_DIRECTIVE="Only output the diff."   # optional full custom text, overrides OUTPUT_STYLE
```

### Lever 4 — Response trimming (shrinks *next* turn's input)

Code-safe whitespace cleanup of the reply (CRLF→LF, trailing-space strip,
collapse 3+ blank lines). Touches **plain text only** — tool calls, structured
output, and code indentation are untouched. Non-stream path only.

```bash
TRIM_RESPONSE=true
```

After restarting, the banner confirms what's live:

```
Output     → ✅ effort=low(anthropic) · max_tokens≤8000 · trim=concise · trim-resp
```

> ### Recommended starting point (Claude target)
>
> ```bash
> OUTPUT_POLICY=true
> REASONING_PROFILE=anthropic
> ROUTINE_EFFORT=low
> TRIM_OUTPUT=true
> OUTPUT_STYLE=concise
> TRIM_RESPONSE=true
> ```
>
> Start here, watch the dashboard, and dial `ROUTINE_EFFORT` back up to `medium`
> or `high` if quality drops on hard tasks.

**Caching is preserved** throughout: the directive is appended (not inserted
ahead of cached content), and `max_tokens` isn't part of any cache key.

---

## 6. PromptAlign — provider-side prompt cache (opt-in)

The L1/L1n/L2 caches save a whole upstream **call**. PromptAlign saves on the
calls you still make: it shapes the outbound request so the **provider's** own
prompt cache hits more often, discounting the input tokens. It's the main lever
when you've turned the response cache off.

```bash
# response cache off, PromptAlign on — discount input on every forwarded call
TOKEN_CACHE=false
SEMANTIC_CACHE=false
PROMPT_ALIGN=true
```

What it does, as the last request mutation (deterministic, idempotent):

- **Normalize** the prefix — code-safe whitespace + deterministic JSON key
  order, so the prefix is byte-identical turn to turn (a jittery prefix is what
  silently busts a provider prompt cache).
- **Reorder** (OpenAI) — hoist a leading system run to the front, *conservatively*:
  a system message placed mid-conversation is left where it is.
- **Inject** (Anthropic) — add `cache_control` breakpoints at the stable/volatile
  boundary (end of `tools`, end of `system`, last stable history message), but
  **only when the client set none**. Client breakpoints always win; the newest
  user turn and `tool_result` blocks are never marked.

```bash
PROMPT_ALIGN_INJECT=true           # inject only when client set no cache_control
PROMPT_ALIGN_REORDER=true          # conservative leading-system hoist (OpenAI)
PROMPT_ALIGN_NORMALIZE=true        # byte-stable prefix
PROMPT_ALIGN_MAX_BREAKPOINTS=3     # Anthropic honors max 4; 3 = tools+system+history
PROMPT_ALIGN_SET_BETA=true         # merge prompt-caching beta header when injecting
```

The banner confirms what's live:

```
Align      → ✅ normalize · reorder · inject≤3
```

**Reading the effect on `/stats`:** the provider returns `cached_tokens` in
usage, which sieve already records — so `cachedTokens` and `cacheHitRatio` climb
as the prompt cache starts hitting. Two PromptAlign-specific counters are added:

```bash
curl -s localhost:4141/stats | jq '.global | {cacheHitRatio, cachedTokens, promptAlignApplied, breakpointsInjected}'
```

> **Header note.** When PromptAlign injects Anthropic breakpoints it merges
> `anthropic-beta: prompt-caching-2024-07-31` so older endpoints honor them; GA
> endpoints ignore the flag. Set `PROMPT_ALIGN_SET_BETA=false` to suppress it.

---

## 7. Wrap a coding agent

One command starts the proxy (or reuses a running one), sets the agent's
base-URL env var, and launches it — so the agent's traffic flows through
everything configured above.

```bash
./sieve wrap claude          # Claude Code   → sets ANTHROPIC_BASE_URL
./sieve wrap codex           # Codex CLI     → sets OPENAI_BASE_URL
./sieve wrap aider           # aider         → sets both vendor base URLs
./sieve wrap cursor          # cursor-agent  (best-effort)
```

Anything after the agent name is passed straight through:

```bash
./sieve wrap claude -p "fix the failing build"
```

- The agent supplies its own credentials, which sieve forwards (or set
  `AUTH_PROVIDER`, [step 7](#7-login-once-skip-pasting-keys)).
- `./sieve wrap copilot` is refused — the GitHub Copilot CLI routes through
  GitHub's backend and can't be pointed at a custom proxy.
- An unknown agent name is launched best-effort with both base URLs set.

---

## 8. Login once (skip pasting keys)

Store a credential and let sieve inject it when a request has no auth header. A
key sent by the client always wins — login is just the fallback.

```bash
# API-key provider:
./sieve login -p qwen --key sk-XXXX --header authorization
# OAuth provider (browser flow):
./sieve login -p chatgpt

./sieve auth list            # show stored credentials
./sieve logout -p qwen       # remove one
```

Then point the proxy at the stored credential in `.env`:

```bash
AUTH_PROVIDER=qwen
```

Credentials are stored in `~/.sieve/auth.json` (mode `0600`). Don't commit it.

---

## 9. Monitor & manage

| Action | Command |
|---|---|
| Live dashboard | open `http://localhost:4141/dashboard` |
| Full metrics JSON | `curl localhost:4141/stats` |
| Liveness + request count | `curl localhost:4141/health` |
| Flush both caches | `curl -X POST localhost:4141/cache/clear` |
| All subcommands | `./sieve --help` |

---

## 10. Cheat sheet

```bash
# build
go build -o sieve .

# .env — point at your provider + opt into reduction
TARGET_URL=https://api.anthropic.com
OUTPUT_POLICY=true
REASONING_PROFILE=anthropic
ROUTINE_EFFORT=low
TRIM_OUTPUT=true
TRIM_RESPONSE=true

# run the easy way: proxy + agent in one command
./sieve wrap claude

# watch the savings
open http://localhost:4141/dashboard
```

Measurement (step 4) and input compression/caching (step 3) work with zero
config; the output reducers (step 5) and wrap (step 6) are opt-in.

---

## 10.5. Multi-upstream routing (opt-in)

Route a single proxy instance to different backends without restart:

```bash
# .env: define named profiles
UPSTREAMS='
anthropic {
  target = "https://api.anthropic.com"
}
gateway {
  target = "https://gateway.example.com/v1"
  auth_provider = "custom"
  auth_override = true
}
'
DEFAULT_UPSTREAM=anthropic

# Client selects upstream per-request
curl -H "X-Sieve-Upstream: gateway" http://localhost:4141/v1/messages
```

**How it works:**
- `DEFAULT_UPSTREAM` is used when no `X-Sieve-Upstream` header is set (or header value is unknown).
- Header lookup is case-insensitive (e.g. `GATEWAY` → gateway profile).
- Requests with no header and no default fall back to a synthesized "default" profile built from legacy `TARGET_URL`/`AUTH_PROVIDER` (backward compatibility).
- Each profile bundles a target URL with auth settings (`auth_provider` and `auth_override`).
- Full test coverage: see `upstream_test.go`.

---

## 11. Full config reference

All settings are env vars (read from `.env` or the environment).

### Proxy

| Variable | Default | Description |
|---|---|---|
| `PORT` | `4141` | Proxy port |
| `TARGET_URL` | `https://api.anthropic.com` | Upstream API (ignored if `UPSTREAMS` is set) |
| `AUTH_PROVIDER` | – | Stored credential to inject when a request has no auth header |
| `UPSTREAMS` | – | Named routing profiles (YAML/TOML-like format; see section 10.5) |
| `DEFAULT_UPSTREAM` | `default` | Fallback profile when X-Sieve-Upstream header is missing/unknown |

### Input compression

| Variable | Default | Description |
|---|---|---|
| `COMPRESSION` | `true` | Enable the compression pipeline |
| `SUMMARIZE_OLD_TURNS` | `true` | Summarize turns older than the threshold |
| `SUMMARIZE_AFTER` | `10` | Summarize when a conversation exceeds this many turns |
| `KEEP_RECENT_TURNS` | `4` | Turns always kept verbatim |

### Tool-result compaction

| Variable | Default | Description |
|---|---|---|
| `TOOL_COMPACTION` | `true` | Content-aware compaction of tool output (JSON / logs / tables) |
| `TOOL_COMPACT_MIN_BYTES` | `1024` | Only compact tool results larger than this |
| `TOOL_COMPACT_MAX_STRING` | `512` | Truncate JSON string values longer than this (runes) |
| `TOOL_COMPACT_MAX_ARRAY` | `24` | Cap JSON arrays longer than this (keep head + tail) |
| `TOOL_COMPACT_MAX_LINES` | `60` | Cap line-oriented output longer than this |
| `TOOL_INTENT` | `false` | Bias compaction by the user's latest message (pinned, so cache-safe) |
| `TOOL_RETRIEVAL` | `false` | Save originals + advertise `sieve_fetch(ref)` (needs the `sieve mcp` server) |
| `RETRIEVAL_TOOL_NAME` | `sieve_fetch` | Tool name written into the compaction marker; match the host-exposed name |

### Caching

| Variable | Default | Description |
|---|---|---|
| `TOKEN_CACHE` | `true` | L1 exact (SHA-256) cache |
| `TOKEN_CACHE_TTL` | `3600` | L1 entry TTL (seconds) |
| `TOKEN_CACHE_MAX` | `1000` | L1 max entries (LRU) |
| `SEMANTIC_CACHE` | `true` | L2 TF-IDF cosine cache |
| `SEMANTIC_THRESHOLD` | `0.82` | Cosine match threshold (0–1) |
| `SEMANTIC_CACHE_MAX` | `500` | L2 max entries |

### Output-token reduction (opt-in)

| Variable | Default | Description |
|---|---|---|
| `OUTPUT_POLICY` | `false` | Master switch for all levers below |
| `REASONING_PROFILE` | `off` | `off` / `anthropic` / `openai` / `auto` — which effort field to set |
| `ROUTINE_EFFORT` | – | `low` / `medium` / `high` — downgrade target (only lowers) |
| `MAX_OUTPUT_TOKENS` | `0` | Clamp the output ceiling (`0` = off; never raises or injects) |
| `TRIM_OUTPUT` | `false` | Inject a conciseness directive |
| `OUTPUT_STYLE` | `concise` | `concise` / `terse` |
| `OUTPUT_DIRECTIVE` | – | Full custom directive text (overrides `OUTPUT_STYLE`) |
| `TRIM_RESPONSE` | `false` | Code-safe whitespace trim of the reply (non-stream) |

> Output-token measurement (input/output token counts on `/stats` and the
> dashboard) is always on and needs no configuration.

### PromptAlign (opt-in)

| Variable | Default | Description |
|---|---|---|
| `PROMPT_ALIGN` | `false` | Master switch for the provider-side prompt-cache optimizer |
| `PROMPT_ALIGN_INJECT` | `true` | Inject Anthropic `cache_control` only when the client set none |
| `PROMPT_ALIGN_REORDER` | `true` | Hoist a leading system run to the front (conservative; OpenAI) |
| `PROMPT_ALIGN_NORMALIZE` | `true` | Byte-normalize the prefix (code-safe whitespace + JSON key order) |
| `PROMPT_ALIGN_MAX_BREAKPOINTS` | `3` | Max `cache_control` breakpoints to inject (Anthropic honors 4) |
| `PROMPT_ALIGN_SET_BETA` | `true` | Merge the prompt-caching beta header when injecting |

> Complements the response cache — discounts input tokens on calls sieve still
> makes. The primary saver when `TOKEN_CACHE`/`SEMANTIC_CACHE` are off.

### Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /v1/messages` | Anthropic format |
| `POST /v1/chat/completions` | OpenAI format |
| `GET /health` | Liveness + request count |
| `GET /stats` | Full metrics (caches, compression, token usage, cache-hit ratio) |
| `POST /cache/clear` | Flush both caches |
| `GET /sieve/fetch?ref=` | Original of a compacted tool result (needs `TOOL_RETRIEVAL`) |
| `GET /dashboard` | Live browser dashboard |
| `*` | Transparent passthrough to `TARGET_URL` |

### Commands

| Command | Purpose |
|---|---|
| `sieve` / `sieve serve` | Start the proxy |
| `sieve wrap <agent> [args...]` | Launch `claude` / `codex` / `aider` / `cursor` through the proxy |
| `sieve mcp` | Run the `sieve_fetch` MCP server (stdio) for tool-result retrieval |
| `sieve login -p <provider> [--key <k>] [--header <style>]` | Store a credential |
| `sieve logout -p <provider>` | Remove a credential |
| `sieve auth list` | List stored credentials |
| `sieve --help` | Usage |
