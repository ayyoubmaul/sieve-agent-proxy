# LLM Internals: Prompts, Tool Calls, Context Windows & Token Saving

> Reference guide for understanding how LLM providers work under the hood,
> and the design rationale for building a smart token-saving proxy/tool.

---

## 1. What Happens When You Send a Prompt

Every API call is a **stateless HTTP POST**. The server holds no memory between
calls — you send the entire conversation history every time.

```
Client → HTTPS POST → Provider API Gateway → GPU Cluster → Autoregressive decoding → Response
```

Internally the model:
1. Tokenizes your input text into integer IDs
2. Runs a forward pass through transformer layers (attention over all tokens in context)
3. Generates output tokens **one-by-one** (autoregressive) until it hits a stop
   condition or `max_tokens`
4. Returns the assembled response

---

## 2. Input / Output Data Structure

The wire format is JSON both ways. The client library unwraps the JSON and hands
the `.text` string to the UI. The user never sees the raw JSON.

### Request (OpenAI / Claude-compatible)

```json
{
  "model": "claude-sonnet-4-5",
  "system": "You are a helpful assistant.",
  "messages": [
    {"role": "user",      "content": "What is the capital of France?"},
    {"role": "assistant", "content": "Paris."},
    {"role": "user",      "content": "And Germany?"}
  ],
  "tools": [...],
  "max_tokens": 1024
}
```

### Response

```json
{
  "id": "msg_01abc",
  "type": "message",
  "role": "assistant",
  "content": [{"type": "text", "text": "Berlin."}],
  "usage": {
    "input_tokens": 312,
    "output_tokens": 5,
    "cache_read_input_tokens": 280,
    "cache_creation_input_tokens": 0
  }
}
```

---

## 3. Prompt Caching

| Provider       | Mechanism                                                          | Duration                 | Cost                     |
|----------------|--------------------------------------------------------------------|--------------------------|--------------------------|
| **Claude**     | Explicit: mark with `cache_control: {"type": "ephemeral"}`         | 5 min default, up to 1h  | ~10% of normal for reads |
| **OpenAI**     | Automatic: identical prefixes cached with no extra config          | ~1 hour                  | 50% discount on cached   |
| **Copilot**    | Depends on underlying model (GPT-4o, Claude, etc.)                 | Inherited from model     | Inherited                |

### Key Rules for Cache Effectiveness

- Caching only works on **prefix matches**. Reordering messages or changing the
  system prompt busts the cache entirely.
- Keep stable content (system prompt, tool definitions) **at the top**.
- Append user messages below — never prepend.
- Report `cache_read_input_tokens` from the usage field to measure hit rate.

---

## 4. Tool Call Flow — Full Round Trip

```
┌─────────────────────────────────────────────────────────────────┐
│  Turn 1: User asks something requiring a tool                   │
│                                                                 │
│  Client → API:                                                  │
│    messages: [user: "search for X"]                             │
│    tools: [{name: "search", description: "...", parameters: …}] │
│                                                                 │
│  API → Client:                                                  │
│    role: "assistant"                                            │
│    content: null                                                │
│    tool_calls: [{id: "tc_001", name: "search", args: {q:"X"}}]  │
│    finish_reason: "tool_calls"                                  │
└────────────────────────┬────────────────────────────────────────┘
                         │ Client executes search("X") locally
                         ▼
┌─────────────────────────────────────────────────────────────────┐
│  Turn 2: Client sends result back                               │
│                                                                 │
│  Client → API (full history):                                   │
│    messages: [                                                  │
│      {role: "user",      content: "search for X"},             │
│      {role: "assistant", tool_calls: [{id:"tc_001",...}]},      │ ← must include
│      {role: "tool",      tool_call_id: "tc_001",               │ ← paired with ↑
│                          content: "<search results>"}           │
│    ]                                                            │
│                                                                 │
│  API → Client:                                                  │
│    role: "assistant"                                            │
│    content: "Here are the results: ..."                         │
│    finish_reason: "stop"                                        │
└─────────────────────────────────────────────────────────────────┘
```

> **Important:** The tool executes **on your machine/server**, not inside the
> LLM. The LLM only decides *which* tool to call and with *what arguments*.

---

## 5. Context Window Overflow is Catastrophic With Tools

LLMs are trained to expect a valid, internally-consistent conversation format.
When you overflow the context window and naively truncate old messages, you get
**silent hallucination** — no error, just wrong data.

### Case A — Orphaned Tool Call (call without response)

```
messages: [
  ...
  {role: "assistant", tool_calls: [{id: "tc_001", fn: "read_file"}]},
  ← TRUNCATED: the tool result message was removed here
  {role: "user", content: "What did the file say?"}
]
```

The model sees it called `read_file` but never received a result.
It will **invent** a plausible file content.

### Case B — Orphaned Tool Response (response without call)

```
messages: [
  ← TRUNCATED: the assistant tool_call message was removed
  {role: "tool", tool_call_id: "tc_001", content: "...file contents..."},
  {role: "user", content: "Summarize that"}
]
```

The model sees a `tool` role message with no preceding `assistant` message that
requested it. This violates the format it was trained on. Behaviour is
undefined — usually hallucination or refusal.

### The Rule

> A `tool_calls` message and its corresponding `tool` result message(s) are an
> **atomic unit**. You must **never** split them when truncating.

---

## 6. Token-Saving Strategy

### a) Smart Context Truncation (highest risk, highest gain)

```
Safe truncation algorithm:
1. Count tokens in current messages array
2. If approaching limit, find the OLDEST message that is:
   - Not part of an active tool call/response pair
   - Not the system prompt
3. Remove it (or replace with a summary placeholder)
4. Repeat until within budget
5. Never split tool_call ↔ tool_result pairs — treat as atomic
```

### b) Tool Result Compression (easy wins)

Tool results are often the biggest token consumers (file contents, search
results, API responses). Before appending to context:

- Summarize / truncate verbose JSON
- Extract only the fields the LLM actually needs
- Use structured extraction: run a cheap/fast LLM call to compress the result
  before adding it to the main context

### c) Prompt Cache Optimization

- Keep system prompt and tool definitions **identical** across turns → maximize cache hits
- Append-only message strategy: never rewrite older messages
- Measure effectiveness via `cache_read_input_tokens` in the usage response

### d) Tool Definition Pruning

Tool schemas are sent with every request and can be large. If you have 20 tools
but only 3 are relevant to the current task, dynamically inject only those 3.

Strategies:
- Intent classification before each turn to select relevant tools
- Tool tagging by domain/capability and match to user query embedding

### e) Message Summarization

When the middle of the conversation is no longer needed:

1. Identify a contiguous block of messages that contain no open tool calls
2. Run a summarization call: send the block to a cheap model, get a 1–2 sentence summary
3. Replace the block with a single synthetic message:
   ```json
   {"role": "assistant", "content": "[Summary: user asked about X, you fetched Y, concluded Z]"}
   ```
4. Verify no orphaned tool calls remain in the surrounding context

---

## 7. Quick Reference: Message Roles

| Role          | Who sends it               | When                                        |
|---------------|----------------------------|---------------------------------------------|
| `system`      | You (developer)            | Once, at the top — instructions & persona   |
| `user`        | The end user               | Every human turn                            |
| `assistant`   | The LLM                    | Every model turn (text or tool_calls)       |
| `tool`        | Your code                  | After executing a tool, to return the result|

---

## 8. Usage Field — What to Track

```json
"usage": {
  "input_tokens":                312,   // tokens you paid to process
  "output_tokens":               48,    // tokens the model generated
  "cache_read_input_tokens":     280,   // tokens served from cache (cheaper)
  "cache_creation_input_tokens": 32     // tokens written to cache (slightly more expensive)
}
```

For a token-saving tool, log these per-request. The ratio
`cache_read / input_tokens` is your cache efficiency score.

---

---

## 9. Codebase Map — What Is Already Built

This section maps every strategy from §6 against the actual Go source.

### Legend

| Symbol | Meaning |
|--------|---------|
| ✅ | Fully implemented |
| ⚠️ | Partially implemented / has known limitation |
| ❌ | Not yet implemented (gap) |

---

### a) Smart Context Truncation

| Sub-feature | Status | Location |
|---|---|---|
| Whitespace normalization | ✅ | `compress.go:39` `normalizeWhitespace()` — CRLF→LF, trailing-space strip, collapse 3+ blank lines. Code-safe (indentation untouched). |
| Exact-duplicate removal | ✅ | `compress.go:148` `deduplicate()` — drops byte-for-byte identical non-system, non-tool messages. Key = `role + "\x00" + fullText`. |
| Old-turn summarization | ✅ | `compress.go:171` `summarizeOld()` — condenses turns older than `SUMMARIZE_AFTER` into a single context-summary user message; keeps `KEEP_RECENT_TURNS` verbatim. |
| **Tool-pair atomic protection** | ✅ | `compress.go:193` — `summarizeOld()` walks backwards from the split boundary and pulls any tool-related turns into the "recent" window before cutting. **A `tool_calls` message and its `tool` response are never split.** |
| Token counting (real) | ⚠️ | Uses character count (`sizeOf` in `compress.go:86`) as a proxy for token count. No tiktoken/cl100k integration — character savings ≠ token savings exactly. |
| Context window limit guard | ❌ | No explicit check against model context limits (e.g. 200K for Claude). Relies on upstream to reject; no pre-flight truncation based on token budget. |

---

### b) Tool Result Compression

| Sub-feature | Status | Location |
|---|---|---|
| Size gate | ✅ | `toolresult.go:83` — results at or below `MinBytes` (default 1024) pass through byte-identical. |
| JSON compaction | ✅ | `toolresult.go:139` `compactJSON()` — parses, recursively elides long string values (`truncateStringValue`) and caps large arrays (`compactArray`) with head+tail kept; re-encodes deterministically (sorted map keys, `json.Number` exact). |
| Log/repetitive-line dedup | ✅ | `toolresult.go:427` `collapseLines()` — 3-pass: cap runaway lines → collapse same-shape runs into `…[×N]` → global head/tail cap. Shape via `lineShape()` normalizes timestamps, UUIDs, hex, numbers. |
| Table/SQL result trimming | ✅ | `toolresult.go:350` `compactTable()` — detects `\|` or `\t` tabular output, keeps header + first/last rows, elides middle with `…[N rows omitted]…`. |
| Determinism guarantee | ✅ | All compactors are pure functions. Go's `encoding/json` marshals map keys sorted; `json.Number` round-trips exact. Same input → same output every turn → upstream prompt-cache prefix preserved. |
| Both wire formats | ✅ | `toolresult.go:239` `compactToolResults()` handles both Anthropic (`tool_result` content blocks) and OpenAI (`role: "tool"` messages). |

---

### c) Prompt Cache Optimization

| Sub-feature | Status | Location |
|---|---|---|
| Deterministic compaction (prefix stability) | ✅ | Core design principle documented in `toolresult.go:1–41`. Every compactor is pure and idempotent so already-seen tool results stay byte-identical across turns. |
| Intent-aware compaction with pinning | ✅ | `intent.go` — `extractIntent()` pulls significant terms from the latest user message. First compaction is intent-biased; result is frozen in `pinCache` (keyed by `sha256(original)[:12]`). All later turns reuse the frozen bytes → byte-stable across turns. |
| `cache_read_input_tokens` tracking | ✅ | `usage.go:35` `usageFromResponse()` and `streamUsage()` extract both Anthropic and OpenAI usage fields including cached tokens. Aggregated in `Stats.TotalCachedTokens`. |
| Cache hit ratio metric | ✅ | `server.go:130` — `cacheHitRatio = TotalCachedTokens / TotalInputTokens × 100`. Exposed at `/stats` and the dashboard. |
| Directive injection cache-safe | ✅ | `output.go:170` `injectDirective()` — appends (never prepends) so the client's prompt-cache prefix and `cache_control` breakpoints stay byte-identical. |

---

### d) Tool Definition Pruning

| Sub-feature | Status | Location |
|---|---|---|
| Dynamic tool selection per request | ❌ | Not implemented. All tool definitions in the request are forwarded as-is. Tool schemas can be large (hundreds of tokens each); pruning to only the relevant subset for each turn is a future optimization. |

**Suggested approach:** classify user intent (already extracted in `intent.go:76`) and tag tools by domain keywords; inject only matching tools per turn.

---

### e) Message Summarization

| Sub-feature | Status | Location |
|---|---|---|
| Structural summarization (current) | ✅ | `compress.go:171` `summarizeOld()` — condenses old turns into a formatted text block: `[U] <450 chars>` / `[A] <450 chars>` / `[tool] <200 chars>` lines joined under a summary header. Fast and local — no LLM call. |
| LLM-based semantic summarization | ❌ | Not implemented. Current summarization is structural (truncated verbatim text), not semantic (a model-generated paragraph capturing meaning). A real summarization call to a cheap model (e.g. Haiku, GPT-4o-mini) would produce much denser, more useful summaries at the cost of latency + tokens. |
| Tool-pair protection in summarization | ✅ | `compress.go:193` — tool messages in the archive block are kept as opaque JSON, never half-dropped. |

---

### f) L1 Token Cache (Exact)

| Sub-feature | Status | Location |
|---|---|---|
| SHA-256 exact match | ✅ | `cache.go:61` `hashKey()` / `hashKeyWithTools()` — key = `sha256(messages + model + system + tools)`. |
| TTL eviction | ✅ | `cache.go:88` — expired entries removed on read. Default TTL: 3600s. |
| LRU eviction | ✅ | `cache.go:105` — when at capacity, evicts the entry with the oldest `ts` (last-access time). |
| Streaming replay | ✅ | `server.go:411` `replayStream()` — cached full response re-emitted as SSE so streaming clients get the expected format on a cache hit. |
| Practical hit rate for agents | ⚠️ | As noted in README: agents append a turn every request, so their prompts are never byte-identical. L1 effectively never hits for agent workloads. Primarily useful for non-agent (repeated batch) use cases. |

---

### g) L2 Semantic Cache (Near-Duplicate)

| Sub-feature | Status | Location |
|---|---|---|
| TF-IDF cosine similarity | ✅ | `cache.go:268` `tfidf()` + `cosine()` — smoothed IDF (`log((N+1)/(df+1)) + 1`) prevents zero-vector collapse in small corpora. |
| Configurable threshold | ✅ | `SEMANTIC_THRESHOLD` (default 0.82). |
| Streaming bypass | ✅ | `server.go:242` — L2 is intentionally skipped for streaming requests. Serving a "similar" cached answer to a slightly different coding prompt would produce wrong code. |
| In-memory only | ⚠️ | `cache.go:33` — both caches are in-process memory maps, reset on restart. No persistence. |
| O(n) lookup | ⚠️ | `cache.go:161` — linear scan over all cached entries per request. Fine up to `SEMANTIC_CACHE_MAX` (default 500) but does not scale to thousands. Embeddings + ANN index would be needed at scale. |

---

### h) Output Token Reduction

| Sub-feature | Status | Location |
|---|---|---|
| `max_tokens` clamp (guardrail) | ✅ | `output.go:56` `clampMaxTokens()` — lowers existing ceiling, never injects one. Handles both `max_tokens` (Anthropic/older OpenAI) and `max_completion_tokens` (newer OpenAI reasoning models). |
| Reasoning effort downgrade | ✅ | `output.go:82` `downgradeEffort()` — sets `output_config.effort` (Anthropic) or `reasoning_effort` (OpenAI); only ever lowers, never raises. Profile: `anthropic` / `openai` / `auto`. |
| Conciseness directive injection | ✅ | `output.go:170` `injectDirective()` — `concise` or `terse` preset, or custom `OUTPUT_DIRECTIVE`. |
| Response-side whitespace trim | ✅ | `response.go:23` `trimResponse()` — applies `normalizeWhitespace` to plain assistant text only; tool calls, thinking blocks, structured output passed through byte-for-byte. Non-streamed path only. |

---

### i) Retrieval / Context Rolling

| Sub-feature | Status | Location |
|---|---|---|
| Content-addressed store | ✅ | `store.go` — filesystem store at `~/.sieve/store`, keyed by `sha256(original)[:12]`. Idempotent writes. `0600` file permissions. |
| Compaction marker | ✅ | `toolresult.go:107` — prefix: `[sieve compacted NB→MB · call sieve_fetch("ref") for the full original]`. |
| MCP server (`sieve_fetch` tool) | ✅ | `mcp.go:90` — JSON-RPC 2.0 over stdio; single `sieve_fetch` tool that looks up `~/.sieve/store`. Separate process from the proxy; shares state through the filesystem only. |
| HTTP endpoint | ✅ | `server.go:165` `handleFetch()` — `GET /sieve/fetch?ref=<ref>`. |
| Ref security | ✅ | `store.go:39` `refPattern` — strict `^[0-9a-f]{12}$` validation; caller-supplied ref can never path-traverse outside the store directory. |

---

## 10. Gap Summary & Suggested Next Steps

| Priority | Gap | Effort | Impact |
|----------|-----|--------|--------|
| Medium | **Real token counting** — replace `sizeOf` (chars) with a tiktoken-compatible counter to make summarization and truncation decisions in actual token space | Medium | Accuracy of all budget decisions |
| Medium | **Context window limit guard** — pre-flight check: if `len(tokens) > model_limit × 0.9`, trigger summarization proactively before the upstream rejects | Low | Prevents hard upstream 413/400 errors |
| High | **Tool definition pruning** — use already-computed intent terms (`extractIntent`) to select which tool schemas to send per turn | Medium | Direct token savings on every request with many tools |
| Low | **LLM-based summarization** — replace structural `summarizeOld` with a cheap-model call for denser, semantically meaningful summaries | High | Summary quality; adds latency + cost per summarization |
| Low | **Cache persistence** — write L1/L2 to disk on shutdown, reload on start | Low | Warm cache across restarts |

*Last updated: 2026-06-30*
