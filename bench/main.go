// bench measures sieve efficiency vs direct gateway calls.
// Usage:
//
//	go run ./bench -key sk-xxx -direct https://gateway.example.com/v1 -sieve http://localhost:4142 -model claude-opus-4-1 -test all
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"
)

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ── Round-trip conversation seed ─────────────────────────────────────────────
// Each user turn is sent; the model replies; we append both to history and
// continue — simulating a real opencode session growing over time.

var roundTripTurns = []string{
	"I'm building a data pipeline in Python that reads from Kafka, transforms records, and writes to BigQuery. The pipeline needs to handle late-arriving events and deduplication. Where should I start?",
	"Good. Now let's talk about the transformation layer. We need to enrich each event with user profile data from Postgres. How do we avoid hammering Postgres on every record?",
	"What about schema evolution? Kafka topics will have multiple schema versions over time.",
	"The pipeline needs to run on Kubernetes. What's the right deployment model — Deployment or StatefulSet?",
	"How should we handle dead-letter queues for records that fail transformation?",
	"We want observability. What metrics should we expose?",
	"What about testing strategy? The pipeline has a lot of external dependencies.",
	"The team is debating between Avro and Protobuf for serialization. What's your take?",
	"We're seeing consumer lag spikes every 2 hours. What should we check?",
	"How do we do blue-green deployments for this pipeline without losing Kafka offsets?",
	"What's the best way to replay historical Kafka events into BigQuery for backfills?",
	"We need to add a real-time anomaly detection step. Should it be in-pipeline or a separate service?",
}

// ── Static conversation for compression-only test ────────────────────────────
// Pre-built so we can send the exact same payload to both direct and sieve
// and compare prompt tokens received by the upstream.

var staticConversation = []message{
	{Role: "user", Content: "I'm building a data pipeline in Python that reads from Kafka, transforms records, and writes to BigQuery. The pipeline needs to handle late-arriving events and deduplication. Where should I start?"},
	{Role: "assistant", Content: "Start with the ingestion layer: use the `confluent-kafka-python` client with consumer groups so multiple workers can scale horizontally. For late-arriving events, attach an event-time watermark to each record at ingest and buffer a configurable window (e.g. 5 minutes) before emitting. For deduplication, use a Redis SET keyed by a composite of (source, event_id, event_time_bucket) with a TTL matching your late-arrival window. BigQuery's streaming insert supports `insertId` for best-effort dedup on their side as a second line of defense."},
	{Role: "user", Content: "Good. Now let's talk about the transformation layer. We need to enrich each event with user profile data from Postgres. How do we avoid hammering Postgres on every record?"},
	{Role: "assistant", Content: "Use an in-process LRU cache (e.g. `cachetools.TTLCache`) sized to your active user set — typically 10k–100k entries at ~1KB each fits comfortably in memory. Set the TTL to match your acceptable staleness (5–15 minutes is common). On a cache miss, fetch from Postgres using a connection pool (`psycopg2` + `pgbouncer` in transaction mode). For cold-start or high-miss scenarios, batch pending misses into a single `WHERE id = ANY($1)` query rather than one query per miss. This reduces Postgres load by 90%+ in most workloads."},
	{Role: "user", Content: "What about schema evolution? Kafka topics will have multiple schema versions over time."},
	{Role: "assistant", Content: "Use a Schema Registry (Confluent's or AWS Glue). Producers register schemas; consumers resolve the schema from the `magic byte + schema_id` prefix in each message. In Python, `confluent-kafka` integrates with `fastavro` for Avro or `protobuf` for proto schemas. For BigQuery, use schema auto-detection off and manage schema migrations explicitly — add nullable columns only, never remove or reorder. Keep a `_schema_version` field in every record so you can backfill or filter by version when needed."},
	{Role: "user", Content: "The pipeline needs to run on Kubernetes. What's the right deployment model — Deployment or StatefulSet?"},
	{Role: "assistant", Content: "Use a Deployment for stateless workers (transformation, enrichment) and a StatefulSet only if you need stable network identity or persistent local storage — rare for Kafka consumers since offset state lives in Kafka itself. Set `terminationGracePeriodSeconds` high enough (60–120s) to finish in-flight batches on pod shutdown. Use PodDisruptionBudgets to prevent simultaneous evictions. Mount secrets (Kafka creds, Postgres DSN) via Kubernetes Secrets or an external vault (HashiCorp Vault / AWS Secrets Manager) — never bake them into the image."},
	{Role: "user", Content: "How should we handle dead-letter queues for records that fail transformation?"},
	{Role: "assistant", Content: "Publish failed records to a dedicated DLQ topic (e.g. `pipeline.dlq`) with an envelope containing the original payload, error message, stack trace, attempt count, and timestamp. Use a separate consumer group to process the DLQ: implement exponential backoff with jitter (start at 1s, cap at 30min), and after N retries (e.g. 5) move records to a permanent failure store (GCS or BigQuery `pipeline_failures` table) for manual inspection. Alert on DLQ lag exceeding a threshold — that's your canary for upstream schema breaks or Postgres outages."},
	{Role: "user", Content: "We want observability. What metrics should we expose?"},
	{Role: "assistant", Content: "Expose these via Prometheus (use `prometheus_client` in Python): consumer lag per topic+partition, records processed/sec, enrichment cache hit rate, transformation error rate, end-to-end latency (event_time to BigQuery write_time), DLQ publish rate, and Postgres query duration (p50/p95/p99). Add structured logging with `structlog` — include trace_id, event_id, schema_version, and partition+offset on every log line. Ship logs to your SIEM or Loki. Use Grafana dashboards with alert rules on lag > 10k records or error rate > 1%."},
	{Role: "user", Content: "What about testing strategy? The pipeline has a lot of external dependencies."},
	{Role: "assistant", Content: "Three layers: unit tests mock all I/O (use `unittest.mock` for Kafka consumer, Postgres, Redis, BigQuery client) and test transformation logic in isolation. Integration tests spin up real dependencies via `docker-compose` (Kafka + Schema Registry + Postgres + Redis) and run against a test topic — use `pytest-docker` or Testcontainers for Python. End-to-end tests run the full pipeline against a staging environment with synthetic events injected into Kafka and assertions on BigQuery output. Focus coverage on the enrichment cache logic, schema evolution paths, and DLQ routing — those are where bugs hide."},
	{Role: "user", Content: "The team is debating between Avro and Protobuf for serialization. What's your take?"},
	{Role: "assistant", Content: "Protobuf if you have polyglot consumers (Go, Java, Python all need to read the same topic) or if you prioritize smaller payload size and faster (de)serialization. Avro if your stack is primarily JVM/Python and you want schema evolution with full backward/forward compatibility enforced by the registry — Avro's schema resolution at read time is more flexible. Both work well with Confluent Schema Registry. Avoid JSON on the wire for high-throughput topics — it's 3–5× larger than Avro/Proto and significantly slower to parse."},
	{Role: "user", Content: "We're seeing consumer lag spikes every 2 hours. What should we check?"},
	{Role: "assistant", Content: "Check these in order: (1) Kafka topic partition count vs consumer instance count — if partitions < instances, some workers are idle; if partitions >> instances, rebalances are expensive. (2) GC pauses in the Python worker — use `gc.callbacks` to log pause duration; if >500ms, tune object lifetimes or switch to PyPy. (3) Postgres connection pool exhaustion — log wait time on pool.getconn(); add pgbouncer metrics. (4) BigQuery write latency — streaming insert quotas (10MB/s per project by default) can throttle; check quota dashboards. (5) Cronjob contention — a 2-hour interval screams scheduled job competing for resources; check Kubernetes node CPU/memory at that time."},
	{Role: "user", Content: "How do we do blue-green deployments for this pipeline without losing Kafka offsets?"},
	{Role: "assistant", Content: "Use two consumer groups — blue and green — both consuming the same topic but only one active at a time. On deploy: start the green group, let it warm up (cache fill, connection pools), then atomically switch traffic by updating your service mesh routing or load balancer. Keep the blue group running but paused (committed offsets preserved) for a rollback window. For Kafka offsets specifically: green starts consuming from the last committed offset of blue — no loss. Use `kafka-consumer-groups.sh --reset-offsets` only if you need to replay. Coordinate with your CI/CD system so both groups share the same Schema Registry and avoid schema version conflicts during the cutover window."},
	{Role: "user", Content: "What's the best way to replay historical Kafka events into BigQuery for backfills?"},
	{Role: "assistant", Content: "Two approaches depending on scale. For small backfills (<100M records): use a dedicated consumer group (`backfill-YYYYMMDD`) with offsets reset to the target start time via `kafka-consumer-groups.sh --reset-offsets --to-datetime`. Run it alongside production — they share the topic, not the offsets. Write backfill output to a staging BigQuery table, validate row counts and checksums, then MERGE into production. For large backfills: export the Kafka topic to GCS using Kafka Connect's GCS Sink (or MirrorMaker2 to a temp cluster), then use BigQuery's `LOAD` command from GCS — far cheaper than streaming inserts for bulk. Always test idempotency: your deduplication logic must handle re-ingesting the same event_id."},
	{Role: "user", Content: "We need to add a real-time anomaly detection step. Should it be in-pipeline or a separate service?"},
	{Role: "assistant", Content: "Separate service, connected via a dedicated Kafka topic. In-pipeline anomaly detection couples ML model lifecycle to data pipeline deploys — model updates require pipeline restarts, which is operationally painful. Instead: pipeline publishes enriched events to `events.enriched`; your anomaly service consumes from there, runs inference (Isolation Forest, LSTM, or a fine-tuned model served via TorchServe/Triton), and publishes anomaly signals to `events.anomalies`. This lets you retrain and redeploy the model independently, scale inference separately (GPU nodes), and A/B test detection algorithms without touching the pipeline. BigQuery can consume both topics and join them for offline analysis."},
	{Role: "user", Content: "Can you summarize all the architectural decisions we've made in this conversation?"},
}

// ── Single-turn prompts for cache test ───────────────────────────────────────

var singleTurnPrompts = []string{
	"What is the capital of France?",
	"Explain what a binary search tree is in one sentence.",
	"What is 2 + 2?",
	"What is the capital of France?",
	"Explain what a binary search tree is in one sentence.",
	"Write a hello world in Python.",
	"Write a hello world in Python.",
	"What is the difference between TCP and UDP?",
	"What is the capital of France?",
}

// ── HTTP ──────────────────────────────────────────────────────────────────────

type turnResult struct {
	turn         int
	latencyMs    int64
	promptTokens int
	totalTokens  int
	replyPreview string
	err          error
}

func post(endpoint, apiKey, model string, msgs []message) (latencyMs int64, promptTok, totalTok int, reply string, err error) {
	body, _ := json.Marshal(map[string]interface{}{
		"model":    model,
		"stream":   false,
		"messages": msgs,
	})

	start := time.Now()
	req, _ := http.NewRequest("POST", endpoint+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err2 := http.DefaultClient.Do(req)
	latencyMs = time.Since(start).Milliseconds()
	if err2 != nil {
		err = err2
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(raw, &out)
	promptTok = out.Usage.PromptTokens
	totalTok = out.Usage.TotalTokens
	if len(out.Choices) > 0 {
		c := out.Choices[0].Message.Content
		if len(c) > 80 {
			c = c[:79] + "…"
		}
		reply = c
	}
	return
}

func getStats(sieve string) map[string]interface{} {
	resp, err := http.Get(sieve + "/stats")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func clearStats(sieve string) {
	http.Post(sieve+"/cache/clear", "application/json", nil)
}

func pct(before, after int64) float64 {
	if before == 0 {
		return 0
	}
	return float64(before-after) / float64(before) * 100
}

func toFloat(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return fmt.Sprintf("%-*s", n, s)
	}
	return s[:n-1] + "…"
}

// ── Test 1: Cache ─────────────────────────────────────────────────────────────

func runCacheTest(direct, sieve, apiKey, model string) {
	fmt.Println("\n╔══════════════════════════════════════════╗")
	fmt.Println("║        Test 1: Cache Efficiency          ║")
	fmt.Println("╚══════════════════════════════════════════╝")

	clearStats(sieve)
	repeatIdx := map[int]bool{3: true, 4: true, 6: true, 8: true}

	var dMs, sTok, dTok int64
	var sMs int64

	fmt.Printf("\n  %-3s  %-38s  %8s  %8s  %8s  %8s\n", "#", "Prompt", "D-ms", "S-ms", "D-tok", "S-tok")
	fmt.Printf("  %s\n", fmt.Sprintf("%s", "─────────────────────────────────────────────────────────────────────────────"))

	for i, p := range singleTurnPrompts {
		dr := struct{ ms, tok int64 }{}
		sr := struct{ ms, tok int64 }{}

		ms, ptok, _, _, _ := post(direct, apiKey, model, []message{{Role: "user", Content: p}})
		dr.ms, dr.tok = ms, int64(ptok)

		ms, ptok, _, _, _ = post(sieve, apiKey, model, []message{{Role: "user", Content: p}})
		sr.ms, sr.tok = ms, int64(ptok)

		flag := ""
		if repeatIdx[i] {
			flag = "⚡"
		}
		fmt.Printf("  %-3d  %s  %7dms  %7dms  %6d  %6d  %s\n",
			i+1, truncate(p, 38), dr.ms, sr.ms, dr.tok, sr.tok, flag)

		dMs += dr.ms
		sMs += sr.ms
		dTok += dr.tok
		sTok += sr.tok
	}

	n := int64(len(singleTurnPrompts))
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "  \t\tDirect\tSieve\tSavings")
	fmt.Fprintf(w, "  Total latency\t\t%dms\t%dms\t%.1f%%\n", dMs, sMs, pct(dMs, sMs))
	fmt.Fprintf(w, "  Avg latency/req\t\t%dms\t%dms\t%.1f%%\n", dMs/n, sMs/n, pct(dMs/n, sMs/n))
	fmt.Fprintf(w, "  Total prompt tokens\t\t%d\t%d\t%.1f%%\n", dTok, sTok, pct(dTok, sTok))
	w.Flush()

	if st := getStats(sieve); st != nil {
		if g, ok := st["global"].(map[string]interface{}); ok {
			fmt.Printf("\n  Token cache hits: %.0f / %d\n", toFloat(g["tokenCacheHits"]), n)
		}
	}
}

// ── Test 2: Compression ───────────────────────────────────────────────────────

func runCompressionTest(direct, sieve, apiKey, model string) {
	fmt.Println("\n╔══════════════════════════════════════════╗")
	fmt.Println("║      Test 2: Compression Efficiency      ║")
	fmt.Printf("║  %d-turn static conversation               ║\n", len(staticConversation))
	fmt.Println("╚══════════════════════════════════════════╝")

	clearStats(sieve)

	dMs, dPtok, dTtok, _, _ := post(direct, apiKey, model, staticConversation)
	sMs, sPtok, sTtok, _, _ := post(sieve, apiKey, model, staticConversation)

	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "  \t\tDirect\tSieve\tSavings")
	fmt.Fprintf(w, "  Latency\t\t%dms\t%dms\t%.1f%%\n", dMs, sMs, pct(dMs, sMs))
	fmt.Fprintf(w, "  Prompt tokens\t\t%d\t%d\t%.1f%%\n", dPtok, sPtok, pct(int64(dPtok), int64(sPtok)))
	fmt.Fprintf(w, "  Total tokens\t\t%d\t%d\t%.1f%%\n", dTtok, sTtok, pct(int64(dTtok), int64(sTtok)))
	w.Flush()

	if st := getStats(sieve); st != nil {
		if g, ok := st["global"].(map[string]interface{}); ok {
			saved := toFloat(g["totalCharsSaved"])
			fmt.Printf("\n  Chars compressed: %.0f  (~%.0f tokens)\n", saved, saved/4)
		}
	}
}

// ── Test 3: Round-trip ────────────────────────────────────────────────────────
// Simulates a live conversation: each turn sends growing history and appends
// the real model reply. Tracks cumulative tokens and latency per turn.

func runRoundTrip(label, endpoint, apiKey, model string) []turnResult {
	fmt.Printf("\n  ── %s ──\n", label)
	fmt.Printf("  %-5s  %-42s  %8s  %10s  %10s\n", "Turn", "User message", "Latency", "Prompt tok", "Cumul tok")
	fmt.Printf("  %s\n", "───────────────────────────────────────────────────────────────────────────────")

	history := []message{}
	results := make([]turnResult, 0, len(roundTripTurns))
	var cumulativeTok int

	for i, userMsg := range roundTripTurns {
		history = append(history, message{Role: "user", Content: userMsg})

		ms, ptok, ttok, reply, err := post(endpoint, apiKey, model, history)
		cumulativeTok += ttok

		tr := turnResult{
			turn:         i + 1,
			latencyMs:    ms,
			promptTokens: ptok,
			totalTokens:  ttok,
			replyPreview: reply,
			err:          err,
		}
		results = append(results, tr)

		if err != nil {
			fmt.Printf("  %-5d  %s  ERROR: %v\n", i+1, truncate(userMsg, 42), err)
			history = history[:len(history)-1]
			continue
		}

		fmt.Printf("  %-5d  %s  %7dms  %10d  %10d\n",
			i+1, truncate(userMsg, 42), ms, ptok, cumulativeTok)

		history = append(history, message{Role: "assistant", Content: reply})
	}

	return results
}

func runRoundTripTest(direct, sieve, apiKey, model string) {
	fmt.Println("\n╔══════════════════════════════════════════╗")
	fmt.Println("║      Test 3: Round-Trip Conversation     ║")
	fmt.Printf("║  %d turns, growing history, real replies  ║\n", len(roundTripTurns))
	fmt.Println("╚══════════════════════════════════════════╝")

	clearStats(sieve)

	dResults := runRoundTrip("Direct", direct, apiKey, model)
	sResults := runRoundTrip("Sieve ", sieve, apiKey, model)

	// Aggregate
	var dTotalMs, sTotalMs, dTotalPtok, sTotalPtok int64
	for i := range dResults {
		if dResults[i].err == nil {
			dTotalMs += dResults[i].latencyMs
			dTotalPtok += int64(dResults[i].promptTokens)
		}
		if i < len(sResults) && sResults[i].err == nil {
			sTotalMs += sResults[i].latencyMs
			sTotalPtok += int64(sResults[i].promptTokens)
		}
	}

	n := int64(len(roundTripTurns))
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "  \t\tDirect\tSieve\tSavings")
	fmt.Fprintf(w, "  Total latency\t\t%dms\t%dms\t%.1f%%\n", dTotalMs, sTotalMs, pct(dTotalMs, sTotalMs))
	fmt.Fprintf(w, "  Avg latency/turn\t\t%dms\t%dms\t%.1f%%\n", dTotalMs/n, sTotalMs/n, pct(dTotalMs/n, sTotalMs/n))
	fmt.Fprintf(w, "  Total prompt tokens sent\t\t%d\t%d\t%.1f%%\n", dTotalPtok, sTotalPtok, pct(dTotalPtok, sTotalPtok))
	fmt.Fprintf(w, "  Avg prompt tokens/turn\t\t%d\t%d\t%.1f%%\n", dTotalPtok/n, sTotalPtok/n, pct(dTotalPtok/n, sTotalPtok/n))
	w.Flush()

	if st := getStats(sieve); st != nil {
		if g, ok := st["global"].(map[string]interface{}); ok {
			saved := toFloat(g["totalCharsSaved"])
			fmt.Printf("\n  Chars compressed across all turns: %.0f (~%.0f tokens)\n", saved, saved/4)
			fmt.Printf("  Token cache hits: %.0f\n", toFloat(g["tokenCacheHits"]))
		}
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	key := flag.String("key", os.Getenv("API_KEY"), "API key")
	direct := flag.String("direct", "https://gateway.example.com/v1", "Direct gateway URL (with path prefix, no trailing slash)")
	sieve := flag.String("sieve", "http://localhost:4142", "Sieve URL")
	model := flag.String("model", "claude-opus-4-1", "Model name")
	test := flag.String("test", "all", "Which test: cache | compression | roundtrip | all")
	flag.Parse()

	if *key == "" {
		fmt.Fprintln(os.Stderr, "error: -key required (or set DANA_API_KEY)")
		os.Exit(1)
	}

	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║       Sieve Efficiency Benchmark         ║")
	fmt.Printf("║  model: %-32s║\n", *model)
	fmt.Println("╚══════════════════════════════════════════╝")

	switch *test {
	case "cache":
		runCacheTest(*direct, *sieve, *key, *model)
	case "compression":
		runCompressionTest(*direct, *sieve, *key, *model)
	case "roundtrip":
		runRoundTripTest(*direct, *sieve, *key, *model)
	default:
		runCacheTest(*direct, *sieve, *key, *model)
		runCompressionTest(*direct, *sieve, *key, *model)
		runRoundTripTest(*direct, *sieve, *key, *model)
	}
}
