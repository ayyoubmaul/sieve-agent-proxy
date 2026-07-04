<div align="center">
  <img src="sieve-logo.png" alt="Sieve Logo" width="200">
  
  # Sieve
  
  **Token-saving LLM proxy** that compresses, caches, and retrieves without losing context
  
  [![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat-square&logo=go)](https://go.dev)
  [![License](https://img.shields.io/badge/license-MIT-green?style=flat-square)](LICENSE)
  [![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen?style=flat-square)](CONTRIBUTING.md)
  
  [Features](#-features) • [Quick Start](#-quick-start) • [How It Works](#-how-it-works) • [Docs](docs/USAGE.md) • [Contributing](CONTRIBUTING.md)
</div>

---

A **single-binary Go proxy** that sits between your LLM client (Claude Code, Cursor, etc.) and any AI API. Cuts token costs through intelligent **compression**, **two-level caching**, and **content-aware tool-result compaction**.

**Zero external dependencies** — pure Go stdlib. Builds to a static binary you can drop anywhere.

## 🎯 Features

| Feature | Impact | Details |
|---------|--------|---------|
| **🔨 Tool-result compaction** | 40-60% ↓ input tokens | Smart JSON/log/SQL shrinking—keep structure, elide bulky values. Deterministic, preserves cache prefix. |
| **📦 Retrieval / context rolling** | Full originals on demand | Store originals content-addressed; compacted form marks where to fetch via `sieve_fetch()` MCP tool. |
| **🗜️ Token compression** | 15-30% ↓ input tokens | Whitespace normalization, deduplication, old-turn summarization—code-safe. |
| **⚡ L1 exact-match cache** | O(1) hits | SHA-256 matching, TTL + LRU, skips API call entirely. |
| **🧠 L2 semantic cache** | 5-15% hit rate | TF-IDF similarity, catches reworded duplicates—fast, dependency-free. |
| **📡 Streaming** | Zero latency | Full SSE support (Anthropic + OpenAI), cached replay included. |
| **🔀 Multi-upstream routing** | Single proxy, many backends | Route requests per-header to different APIs without restart. |

> 📖 **[USAGE.md](docs/USAGE.md)** walks you through every feature with live examples.

---

## 🚀 Quick Start

### Install

```bash
# Clone and build
git clone https://github.com/ayyoubmaul/sieve-agent-proxy
cd sieve-agent-proxy
make build              # or: go build -o bin/sieve ./cmd/sieve
./bin/sieve --help      # verify installation
```

### Run

```bash
# Start the proxy (listens on :4141)
./sieve

# Point Claude Code at it
ANTHROPIC_BASE_URL=http://localhost:4141 \
ANTHROPIC_API_KEY=sk-ant-xxx \
claude
```

All compression and caching are **on by default** — zero config needed for the basic win. Optional features (semantic caching, output reduction, multi-upstream) are controlled by env vars in `.env`.

👉 **[Full setup guide →](docs/USAGE.md)** | **[Development →](CONTRIBUTING.md)**

---

## 💡 How It Works

```
  ┌─ Claude Code ──┐
  │ (or any client)│
         │
         ▼
  ┌──────────────┐
  │ Sieve Proxy  │
  │              │
  │ ├─ Compress  │
  │ ├─ Cache L1  │
  │ ├─ Cache L2  │
  │ └─ Compact   │
  └──────────────┘
         │
         ▼
  ┌──────────────┐
  │ Claude API   │
  │ (Anthropic)  │
  └──────────────┘

Sieve sits in the hot path:
• Input: Whitelisted compression before sending to API
• Output: Tool-result compaction (shrink bulky JSON/logs/SQL)
• Caching: L1 (exact) + L2 (semantic) — both in-memory, O(1) hits
• Retrieval: Full originals saved; agent pulls them back via sieve_fetch()
```

---

## 📊 Why Go (and not Node or Rust)?

This proxy sits in the hot path — language choice matters:

|                                                 | Node (prototype)               | **Go (this)**                    | Rust                       |
| ----------------------------------------------- | ------------------------------ | -------------------------------- | -------------------------- |
| Deployment                                      | needs runtime + `node_modules` | **single static binary**         | single static binary       |
| Concurrency model                               | event loop (fine for I/O)      | **goroutines per stream, cheap** | async, excellent           |
| Memory footprint                                | higher, GC pauses vary         | **low, predictable**             | lowest                     |
| CPU work (TF-IDF cosine over cache)             | slower                         | **fast (compiled)**              | fastest                    |
| Dev speed for a streaming proxy w/ shared cache | fast                           | **fast**                         | slower (async + lifetimes) |

Node isn't _wrong_ — for pure I/O its event loop handles concurrent connections
well. But the wins here are deployment (one binary, no runtime), predictable
memory, and the CPU-bound semantic-cache math.

**Go over Rust** for this specific job: the network round-trip to the LLM API
dominates latency, so Rust's raw-perf edge rarely shows up, while Go gets you a
correct streaming proxy with shared mutable cache state far faster and with less
ceremony. Pick Rust instead if you need to embed this in a Rust stack or have
hard memory limits.

---

## Quick Start

```bash
cp .env.example .env      # optional — sensible defaults work out of the box

make run                  # or: go run .
# Proxy    → http://localhost:4141
# Dashboard→ http://localhost:4141/dashboard
```

Build a binary:

```bash
make build                # → ./sieve
make release              # → dist/ cross-compiled for linux/mac/windows
```

---

## Wrap a coding agent

Point an agent at the proxy in one command — sieve starts the proxy (or reuses a
running one), sets the agent's base-URL env var, and launches it:

```bash
sieve wrap claude          # Claude Code   → ANTHROPIC_BASE_URL
sieve wrap codex           # Codex CLI     → OPENAI_BASE_URL
sieve wrap aider           # aider         → OPENAI / ANTHROPIC base URLs
sieve wrap cursor          # cursor-agent  (best-effort)
```

Anything after the agent name is passed straight through
(`sieve wrap claude -p "fix the build"`). The agent supplies its own credentials,
which sieve forwards — or set `AUTH_PROVIDER` to inject a stored one. An unknown
agent name is launched best-effort with both base URLs set. `copilot` can't be
wrapped: the GitHub Copilot CLI routes through GitHub's backend and isn't
repointable.

---

## Run with Claude Code

sieve is a transparent proxy: Claude Code talks to it exactly as it would to the
Anthropic API, and sieve compacts tool output on the way upstream. Compaction is
on by default, so there's nothing to configure for the basic win.

### One command (recommended)

```bash
sieve wrap claude
```

Starts the proxy (or reuses a running one), points `ANTHROPIC_BASE_URL` at it, and
launches Claude Code. Anything after `claude` is passed through, e.g.
`sieve wrap claude -p "summarize the test failures"`.

### Manual (explicit base URL)

Run the proxy, then point Claude Code at it yourself:

```bash
PORT=4141 sieve serve &                            # 1. start sieve (its own config lives here)
ANTHROPIC_BASE_URL=http://localhost:4141 claude    # 2. point Claude Code at sieve
```

> ⚠️ **The one variable that matters is `ANTHROPIC_BASE_URL`.** It goes on the
> `claude` command and is what makes Claude Code talk to sieve. `PORT`,
> `TARGET_URL`, `AUTH_PROVIDER`, etc. configure **sieve** and only take effect on
> the `sieve serve` process — passing them to `claude` does nothing, and Claude
> Code will silently go straight to Anthropic. If you started sieve on a custom
> port, the base URL must match it (`PORT=8787 sieve serve` →
> `ANTHROPIC_BASE_URL=http://localhost:8787`).

**Verify traffic is actually reaching sieve.** Open
`http://localhost:<port>/dashboard` (or `curl …/stats`) and watch `requests`
climb as you chat. If it stays at `0`, Claude Code is bypassing sieve — almost
always a missing or mismatched `ANTHROPIC_BASE_URL`.

Either form needs Claude Code to be **logged in** — `claude` sends its own
credentials and sieve forwards them. If it isn't, you'll see
`Not logged in · Please run /login`; that's the agent's auth, not sieve. An
Anthropic **API key** is the sanctioned path — a subscription login may be refused
through a third-party base URL (see [Login auth](#login-auth-skip-pasting-keys)).

### Through a non-Anthropic gateway (model override)

You can also send Claude Code to an OpenAI-compatible LLM gateway (e.g. a LiteLLM
deployment that serves the Anthropic `/v1/messages` endpoint and translates to
Qwen / GLM / DeepSeek / Kimi models). Two extra things are needed:

```bash
# sieve side (.env or env): forward to the gateway, inject the stored gateway key
TARGET_URL=https://your-gateway.example.com
AUTH_PROVIDER=gateway       # the credential you saved with `sieve login -p gateway`
AUTH_OVERRIDE=true          # Claude Code always sends *some* key; ignore it and inject the stored one

# Claude Code side: pick models the gateway key is allowed to use
ANTHROPIC_BASE_URL=http://localhost:4141 \
ANTHROPIC_API_KEY=unused \
ANTHROPIC_MODEL=glm-5.2 \
ANTHROPIC_SMALL_FAST_MODEL=qwen3.6-flash \
claude
```

Why each piece:

- **`AUTH_OVERRIDE=true`** — Claude Code insists on sending a key. By default a
  client-supplied credential wins (transparent passthrough), so that throwaway key
  would be forwarded to the gateway and rejected. `AUTH_OVERRIDE` flips that:
  whenever it's on and `AUTH_PROVIDER` is set, sieve **discards the client key and
  injects the stored one**. (It's global — while on, *every* client key is ignored.)
- **`ANTHROPIC_MODEL` / `ANTHROPIC_SMALL_FAST_MODEL`** — Claude Code defaults to
  `claude-*` model ids the gateway key usually can't access (you'll get a 403
  listing the allowed models). Set both to allowed ids — the main model and the
  background/fast one.

> Note: tool-calling reliability through a translation gateway depends on the
> **underlying model**, not sieve. Non-Claude models behind LiteLLM sometimes emit
> malformed or missing `tool_use` blocks, which Claude Code's strict
> tool_use/tool_result pairing rejects (`400 … tool use concurrency`). If you hit
> that, try a more tool-capable allowed model, and turn off `TOKEN_CACHE` /
> `SEMANTIC_CACHE` so a replayed cached turn can't desync the tool pairing.

### Enable intent + retrieval (optional)

```bash
# register the fetch tool once, so the model can pull back originals on demand
claude mcp add sieve -- sieve mcp

# run with intent-aware compaction + on-demand retrieval
TOOL_INTENT=true TOOL_RETRIEVAL=true sieve wrap claude
```

### Watch it work

The proxy prints a line per request when it compacts a tool result:

```
→ /v1/messages | model=… msgs=3 stream=true
  ✂️  tool results −27520 chars
```

Running totals — including the prompt-cache hit ratio sieve is built to preserve —
are at `http://localhost:4141/stats` (`toolCharsSaved`, `cacheHitRatio`) and the
live `http://localhost:4141/dashboard`.

> **Verified end-to-end** with the Claude Code CLI: when the agent ran `cat` on a
> repetitive 27,600-char log, sieve compacted that tool result to **76 chars**
> (`…[×400]`) on the wire — the model still sees one representative line plus the
> count, and can `sieve_fetch` the full original if it needs it.

---

## Connect OpenCode

Point OpenCode's provider `baseURL` at the proxy so requests land on the
standard `/v1/...` paths.

`~/.config/opencode/config.json`:

```json
{
  "providers": {
    "openai": {
      "apiKey": "YOUR_KEY",
      "baseURL": "http://localhost:4141/v1"
    }
  },
  "model": "gpt-4o"
}
```

Anthropic via OpenCode:

```json
{
  "providers": {
    "anthropic": {
      "apiKey": "YOUR_ANTHROPIC_KEY",
      "baseURL": "http://localhost:4141"
    }
  }
}
```

The proxy auto-detects the wire format from the path (`/v1/messages` →
Anthropic, `/v1/chat/completions` → OpenAI) and sets the correct upstream
auth headers.

---

## Login auth (skip pasting keys)

Like OpenCode, you can log in once instead of sending a key on every request.
Credentials are stored in `~/.sieve/auth.json` (file mode `0600`), and the proxy
injects them automatically when an incoming request has no auth header. **A
credential sent by the client always wins** — login is just the fallback.

### API-key providers (most common)

```bash
./llm-compress-proxy login -p qwen          # prompts for the key
# or non-interactively:
./llm-compress-proxy login -p qwen --key sk-XXXX --header authorization
```

Then point the proxy at it:

```env
AUTH_PROVIDER=qwen
```

Now your requests need no key at all:

```bash
curl http://localhost:4141/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3.5-plus","messages":[{"role":"user","content":"hi"}]}'
```

### OAuth providers (browser login + auto-refresh)

For providers registered in `oauthProviders` (see `login.go`), login runs a
standard **OAuth 2.0 PKCE** flow:

```bash
./llm-compress-proxy login -p chatgpt
# opens the browser -> sign in to ChatGPT -> tokens + account id stored -> auto-refreshed
```

This runs the Codex public-client PKCE flow: it opens `auth.openai.com`, catches
the redirect on `http://localhost:1455/auth/callback`, exchanges the code for
tokens, and extracts your `chatgpt_account_id` from the returned `id_token` —
stored so it's sent as the `ChatGPT-Account-Id` header (the backend 401s without it).

**Two things to understand about ChatGPT login specifically:**

1. **It does not turn ChatGPT into a normal OpenAI API.** A ChatGPT-subscription
   token authenticates against the ChatGPT backend (the Codex _Responses_ API at
   `https://chatgpt.com/backend-api/codex`), **not** `api.openai.com/v1/chat/completions`,
   and the backend expects Codex-shaped requests (including a specific system
   prompt). So pointing a generic OpenAI client at this won't work. The realistic
   setup is running **Codex CLI through this proxy**: configure a custom Codex
   provider with `requires_openai_auth = true`, point its base URL at the proxy,
   set `TARGET_URL=https://chatgpt.com/backend-api/codex`, and let traffic pass
   through (Codex supplies its own auth + account-id; the proxy compresses/forwards).

2. **ToS.** Using a ChatGPT subscription through a third-party tool is a gray area.
   As of mid-2026 OpenAI has _not_ restricted Codex OAuth in third-party apps
   (unlike Anthropic, which banned the equivalent in Feb 2026), so it currently
   works — but the flow depends on mimicking the Codex CLI request shape and can
   break whenever OpenAI changes its auth check. Use at your own risk; an API key
   is the stable, sanctioned path.

### Other OAuth providers

### Manage

```bash
./llm-compress-proxy auth list             # show stored credentials
./llm-compress-proxy logout -p openai      # remove one
```

> **Important caveats.** The OAuth entries in `oauthProviders` are _templates_
> using public client IDs. Whether you may use a provider's subscription OAuth
> through a third-party proxy depends on that provider's terms of service — for
> many providers an API key is the sanctioned path, so check first. Some
> providers also need extra per-request headers (e.g. an account id parsed from
> the returned `id_token`); add those via the credential's `Extra` map. Tokens
> and keys are long-lived secrets — the store is `0600`, but don't commit it.

---

## Request flow

```
agent ─POST→ Proxy :4141
   │
   ├─ L1 token cache?      SHA-256(messages+model)   → HIT: return instantly
   ├─ L2 semantic cache?   TF-IDF cosine ≥ threshold → HIT: return (non-stream)
   ├─ compact tool results JSON · logs · tables  (deterministic; store originals)
   ├─ compress messages    whitespace · dedup · summarise old turns
   ├─ forward to TARGET_URL (stream or batch)
   └─ store response in L1 + L2, return to client
```

### Tool-result compaction

Coding agents resend the entire conversation every turn, and the densest,
least-useful part of it is tool output. The generic text compressor leaves it
alone (it never touches `tool_result` blocks), so it's re-billed in full on
every subsequent turn. This stage compacts it, content-aware:

| Tool output             | What it does                                                                                                  |
| ----------------------- | ------------------------------------------------------------------------------------------------------------- |
| **Bloated JSON**        | Keeps every key and the structure; elides long string values (`…[+N chars]`) and caps big arrays (head + tail). Numbers preserved exactly; output stays valid JSON. |
| **Logs / repetitive**   | Normalises volatile tokens (timestamps, ids, counters) to a *shape*, then collapses runs of same-shape lines into one + `…[×N]`; global head/tail cap on top. |
| **SQL / table dumps**   | Detects delimiter-separated rows and keeps the header + first/last rows, eliding the middle (`…[N rows omitted]…`) — the boundary rows you usually want. |

Two properties make it safe to run on every request:

- **Deterministic.** Every compactor is a pure function of the input bytes — the
  same tool result always compacts to exactly the same output. That keeps each
  already-seen result **byte-identical across turns**, so the Anthropic
  **prompt-cache prefix** a growing agent conversation relies on for its discount
  is preserved. (Compaction that varied per turn would bust the very cache it's
  meant to help — which is why this is deterministic rather than intent-driven.)
- **Size-gated.** Results at or below `TOOL_COMPACT_MIN_BYTES` pass through
  byte-identical; only genuinely bloated output is reshaped.

The choice for JSON is to keep all keys and elide bulky *values* — the bloat
lives in values (embedded blobs, huge fields, long arrays), not key names, and
dropping keys blindly risks discarding the one field the model needs.

#### Intent-aware compaction (`TOOL_INTENT`)

By default compaction is purely structural. Turn on `TOOL_INTENT` to bias it by
the **user's latest message**: JSON values whose key matches a term from the
prompt are kept fuller, and log lines / table rows that match are protected from
elision. So when you ask about the `auth` flow, the `auth`-related fields and the
log lines mentioning it survive, while the rest is still trimmed.

This would normally fight the cache (intent changes every turn), so it's made
safe by **pinning**: the first time a given tool result is compacted, its bytes
are frozen in a process-lifetime cache keyed by the original's content hash, and
every later turn reuses them verbatim. Intent shapes the first impression — the
turn the result was actually produced for — and the pin keeps it byte-stable
forever after, so the prompt-cache prefix never moves.

### Retrieval / context rolling (`TOOL_RETRIEVAL`)

Compaction is lossy by design, so the full original of every compacted result is
saved to a **content-addressed store** (`~/.sieve/store`, keyed by
`sha256(original)[:12]`) and the compacted form is prefixed with a marker:

```
[sieve compacted 24576B→1180B · call sieve_fetch("ab12cd34ef00") for the full original]
```

The bulky original lives **out-of-band**; a compact stand-in with a ref lives in
the context; the model pulls the original back only if it actually needs it — so
the tokens are spent **on demand** instead of on every turn. Retrieval works two
ways, both reading the same on-disk store:

- **`sieve_fetch` MCP tool** — run `sieve mcp` and register it with your agent so
  the model can call it itself:

  ```bash
  claude mcp add sieve -- sieve mcp
  ```

  > **Marker name vs. tool name.** The tool is registered under the bare name
  > `fetch`; MCP hosts prefix the server name, so a server registered as `sieve`
  > exposes it as `sieve_fetch` — which is what the marker prints by default. If
  > your host surfaces a different name (e.g. a legacy `sieve_sieve_fetch`), set
  > `RETRIEVAL_TOOL_NAME` to match so the marker always names a callable tool.
  > The `sieve mcp` server accepts both `fetch` and `sieve_fetch`.

  The proxy (writer) and `sieve mcp` (reader) are separate processes that share
  state only through `~/.sieve/store`, so nothing extra needs to be running.
- **`GET /sieve/fetch?ref=<ref>`** — the same lookup over HTTP, for scripts or
  debugging.

Because the store is content-addressed, the ref embedded in the marker is itself
a pure function of the content, so the compacted output stays byte-stable and
cache-friendly. Retrieval is **off by default**: enable it only once the MCP
server is registered, otherwise the marker advertises a tool the agent can't call.

### Compression strategies

| Strategy                 | Typical savings | Notes                                                                                                                                               |
| ------------------------ | --------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| Whitespace normalisation | 2–8%            | Code-safe: CRLF→LF, trailing-space strip, collapse 3+ blank lines. Leaves indentation and inline spacing untouched so source code is never altered. |
| Deduplication            | 5–20%           | Drops only byte-for-byte identical messages (keeps all system prompts)                                                                              |
| Old-turn summarisation   | 30–60%          | Condenses turns older than `SUMMARIZE_AFTER`                                                                                                        |

> Note: summarisation is lossy by design — it trades old verbatim turns for a
> compact summary block. Tune `SUMMARIZE_AFTER` / `KEEP_RECENT_TURNS`, or set
> `SUMMARIZE_OLD_TURNS=false` for fully lossless behaviour (whitespace + dedup only).

### Output token reduction

The strategies above shrink what the model **reads** (input). The proxy can also
reduce what the model **writes** (output) — which matters because output tokens
bill several times higher than input (e.g. Opus: $5/1M in vs $25/1M out).

A proxy cannot un-generate tokens — they are billed the moment the model writes
them — so the only lever is to shape the **request** so the model generates less.
Three levers are available, each independently gated under the `OUTPUT_POLICY`
master switch:

| Lever | Variable | Default | Description |
| ----- | -------- | ------- | ----------- |
| _master_ | `OUTPUT_POLICY`     | `false` | Enables request-side output-token reduction |
| reasoning | `REASONING_PROFILE` | `off`   | `off` / `anthropic` (sets `output_config.effort`) / `openai` (sets `reasoning_effort`) / `auto` |
| reasoning | `ROUTINE_EFFORT`    | –       | Downgrade target: `low` / `medium` / `high`. Only ever *lowers* effort (thinking tokens bill as output) |
| clamp | `MAX_OUTPUT_TOKENS`  | `0`     | Cap the per-response ceiling. Guardrail against runaways — never raises or injects a cap the client omitted |
| trim  | `TRIM_OUTPUT`        | `false` | Inject a conciseness directive (drops ceremony / restated code) |
| trim  | `OUTPUT_STYLE`       | `concise` | `concise` / `terse` |
| trim  | `OUTPUT_DIRECTIVE`   | –       | Full custom directive text (overrides `OUTPUT_STYLE`) |
| trim-resp | `TRIM_RESPONSE`  | `false` | Trim the **reply** before returning it (see below) |

> These trade some response quality for fewer output tokens, so the whole stage
> is **off by default and opt-in**. The reasoning profile is explicit because
> sending an effort field to a target that doesn't accept it (a non-effort Claude
> model, a non-reasoning OpenAI model, or another provider) can be rejected —
> pick the profile that matches your `TARGET_URL`.
>
> **Caching is preserved.** The directive is *appended* (a trailing `system`
> block on Anthropic, a trailing system message on OpenAI) rather than inserted
> ahead of existing content, so your prompt-cache prefix and `cache_control`
> breakpoints stay byte-identical. The `max_tokens` clamp isn't part of any cache
> key at all.
>
> Either way, `/stats` and the dashboard always **measure** input/output tokens
> so you can see the effect.

`TRIM_RESPONSE` is the one response-side lever (`trim-resp`). A proxy can't
un-bill tokens the model already wrote, but in an agent loop the reply is
re-sent as input next turn — so trimming it shrinks *future* input. It reuses the
same code-safe whitespace cleanup as input compression (CRLF→LF, trailing-space
strip, collapse 3+ blank lines), touching **only plain assistant text** — tool
calls, structured output, and code indentation are passed through byte-for-byte.
It runs on the **non-streamed path only** (trimming a live stream would mean
buffering the whole reply and defeat streaming). Aggressive natural-language
ceremony-stripping is deliberately *not* done — it can't be made safe generically
without risking corrupted replies.

### Cache levels

| Level       | Match                     | Speed | Best for                            |
| ----------- | ------------------------- | ----- | ----------------------------------- |
| L1 token    | exact SHA-256             | O(1)  | identical repeated prompts          |
| L2 semantic | TF-IDF cosine ≥ threshold | O(n)  | reworded / near-duplicate questions |

> **Caching and coding agents.** L1 only hits on byte-identical prompts — but an
> agent appends a turn every request, so its prompts are never identical and L1
> effectively never hits. L2 (semantic) runs on **non-streaming requests only**;
> agents stream, so it doesn't run for them — intentionally, since serving a
> "similar" cached answer to a slightly different coding prompt would be wrong
> code. So for coding agents the savings come from **compression + output
> reduction**, not the cache. L2 is worthwhile for repetitive *non-streaming*
> workloads; tune `SEMANTIC_THRESHOLD` (`0.82` ≈ near-duplicates only; `0.5–0.7`
> catches looser matches at the cost of more false hits).

---

### PromptAlign (provider-side prompt cache)

The cache levels above save an upstream **call**. PromptAlign instead discounts
the **input tokens** of the calls sieve still makes, by shaping the outbound
request so the *provider's* own prompt cache hits more often. It's the
complement of L1/L1n/L2 — and the primary saver when you've turned the response
cache off (`TOKEN_CACHE=false SEMANTIC_CACHE=false`). Opt in with
`PROMPT_ALIGN=true`.

Three deterministic, independently-gated transforms run as the **last** request
mutation (on the final, byte-stable body):

| Transform     | What it does                                                                                       | Provider          |
| ------------- | -------------------------------------------------------------------------------------------------- | ----------------- |
| **Normalize** | Code-safe whitespace cleanup + deterministic JSON key order, so the prefix is byte-identical across turns/clients | Anthropic, OpenAI |
| **Reorder**   | Hoists a leading system run to the front — *conservatively*: a system message placed mid-conversation is left in place | OpenAI            |
| **Inject**    | Adds `cache_control` breakpoints at the stable/volatile boundary: end of `tools`, end of `system`, and the last stable history message | Anthropic         |

**Conservative by design:**

- **Client intent wins.** If the request already carries *any* `cache_control`,
  PromptAlign injects nothing — it only normalizes. No double-marking, no
  blowing Anthropic's 4-breakpoint budget.
- **Never marks the volatile tail.** The newest user turn changes every request,
  so it's never given a breakpoint — the cached prefix stops just before it.
- **Never marks a `tool_result`** block (Anthropic disallows caching them).
- **OpenAI** has no `cache_control`; its prompt cache is automatic and
  prefix-based, so there PromptAlign only stabilizes the prefix.

When injection happens for Anthropic, sieve also merges the
`anthropic-beta: prompt-caching-2024-07-31` flag (disable with
`PROMPT_ALIGN_SET_BETA=false`); GA endpoints ignore it. The effect shows up on
`/stats` as the provider's `cachedTokens` / `cacheHitRatio` rising, plus
`promptAlignApplied` and `breakpointsInjected` counters.

---

## Endpoints

| Endpoint                    | Purpose                                 |
| --------------------------- | --------------------------------------- |
| `POST /v1/messages`         | Anthropic format                        |
| `POST /v1/chat/completions` | OpenAI format                           |
| `GET /health`               | liveness + basic counters               |
| `GET /stats`                | full cache + compression metrics        |
| `POST /cache/clear`         | flush both caches                       |
| `GET /sieve/fetch?ref=`     | original of a compacted tool result     |
| `GET /dashboard`            | live browser dashboard                  |
| `*`                         | transparent passthrough to `TARGET_URL` |

---

## Configuration (`.env` or env vars)

| Variable              | Default                     | Description                                  |
| --------------------- | --------------------------- | -------------------------------------------- |
| `PORT`                | `4141`                      | Proxy port                                   |
| `TARGET_URL`          | `https://api.anthropic.com` | Upstream API                                 |
| `AUTH_PROVIDER`       | –                           | Stored credential (see `login`) to inject when a request has no auth header |
| `AUTH_OVERRIDE`       | `false`                     | Inject `AUTH_PROVIDER` even when the client sent a key (discards the client's) |
| `COMPRESSION`         | `true`                      | Enable compression pipeline                  |
| `SUMMARIZE_OLD_TURNS` | `true`                      | Enable old-turn summarisation                |
| `SUMMARIZE_AFTER`     | `10`                        | Summarise when convo exceeds this many turns |
| `KEEP_RECENT_TURNS`   | `4`                         | Turns always kept verbatim                   |
| `TOOL_COMPACTION`     | `true`                      | Content-aware tool-result compaction         |
| `TOOL_COMPACT_MIN_BYTES` | `1024`                   | Only compact tool results larger than this   |
| `TOOL_COMPACT_MAX_STRING` | `512`                   | Truncate JSON string values longer (runes)   |
| `TOOL_COMPACT_MAX_ARRAY` | `24`                     | Cap JSON arrays longer than this             |
| `TOOL_COMPACT_MAX_LINES` | `60`                     | Cap line-oriented output longer than this    |
| `TOOL_INTENT`         | `false`                     | Bias compaction by the user's latest message (pinned for cache safety) |
| `TOOL_RETRIEVAL`      | `false`                     | Save originals + advertise `sieve_fetch(ref)` |
| `RETRIEVAL_TOOL_NAME` | `sieve_fetch`               | Tool name written into the compaction marker (must match the host-exposed name) |
| `TOKEN_CACHE`         | `true`                      | Enable L1 exact cache                        |
| `TOKEN_CACHE_TTL`     | `3600`                      | L1 entry TTL (seconds)                       |
| `TOKEN_CACHE_MAX`     | `1000`                      | L1 max entries (LRU)                         |
| `SEMANTIC_CACHE`      | `true`                      | Enable L2 semantic cache                     |
| `SEMANTIC_THRESHOLD`  | `0.82`                      | Cosine threshold (0–1)                       |
| `SEMANTIC_CACHE_MAX`  | `500`                       | L2 max entries                               |
| `PROMPT_ALIGN`        | `false`                     | Enable PromptAlign (provider-side prompt-cache optimizer) |
| `PROMPT_ALIGN_INJECT` | `true`                      | Inject Anthropic `cache_control` only when the client set none |
| `PROMPT_ALIGN_REORDER`| `true`                      | Hoist a leading system run to the front (conservative; OpenAI) |
| `PROMPT_ALIGN_NORMALIZE` | `true`                   | Byte-normalize the prefix (whitespace + JSON key order) |
| `PROMPT_ALIGN_MAX_BREAKPOINTS` | `3`                | Max `cache_control` breakpoints to inject (Anthropic honors 4) |
| `PROMPT_ALIGN_SET_BETA` | `true`                    | Merge prompt-caching beta header when injecting |

---

## Requirements

- Go ≥ 1.21 to build. Nothing else — no Python, no GPU, no databases, no `node_modules`.

## Notes & limitations

- The semantic cache uses **TF-IDF**, not embeddings. It's fast and dependency-free
  but lexical: it catches reworded duplicates, not deep paraphrase. Swapping in an
  embedding model behind the same `SemanticGet`/`SemanticSet` interface is straightforward.
- L2 lookup is O(n) over cached entries; keep `SEMANTIC_CACHE_MAX` reasonable.
- Caches are in-memory and reset on restart. Add a persistence layer if you need
  durability across restarts.

## License

MIT
