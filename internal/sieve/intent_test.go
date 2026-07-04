package sieve

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestExtractIntentSkipsToolResults(t *testing.T) {
	body := `[
		{"role":"user","content":"Find the auth token bug in the handler"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"...big tool output..."}]}
	]`
	var msgs []Message
	if err := json.Unmarshal([]byte(body), &msgs); err != nil {
		t.Fatal(err)
	}
	terms := extractIntent(msgs)
	set := map[string]bool{}
	for _, x := range terms {
		set[x] = true
	}
	for _, want := range []string{"auth", "token", "bug", "handler"} {
		if !set[want] {
			t.Errorf("intent missing %q (got %v)", want, terms)
		}
	}
	if set["the"] {
		t.Error("stopword 'the' leaked into intent terms")
	}
}

func TestIntentKeepsMatchingJSONKeyFuller(t *testing.T) {
	opt := defaultCompactOpts()
	opt.Intent = []string{"summary"}
	long := strings.Repeat("x", 4000)
	input := fmt.Sprintf(`{"summary":%q,"noise":%q}`, long, long)

	out, ok := compactJSON(input, opt)
	if !ok {
		t.Fatal("compactJSON ok=false")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output not a string map: %v\n%s", err, out)
	}
	if len([]rune(m["summary"])) < 2000 {
		t.Errorf("intent-matched key was over-truncated: %d runes", len([]rune(m["summary"])))
	}
	if len([]rune(m["noise"])) > 800 {
		t.Errorf("non-matching key not truncated: %d runes", len([]rune(m["noise"])))
	}
}

func TestIntentProtectsMatchingLogLine(t *testing.T) {
	opt := defaultCompactOpts()
	mkLines := func() []string {
		lines := make([]string, 0, 200)
		for i := 0; i < 200; i++ {
			if i == 120 {
				lines = append(lines, "FATAL panic in worker goroutine 42")
				continue
			}
			tag := string(rune('A'+i%26)) + string(rune('A'+(i/26)%26))
			lines = append(lines, "event-"+tag+" routine processing ok")
		}
		return lines
	}

	// Without intent, the middle panic line is elided.
	if out := collapseLines(mkLines(), opt); strings.Contains(out, "panic in worker") {
		t.Fatal("control: panic line should have been elided without intent")
	}
	// With intent "panic", it survives the head/tail cap.
	opt.Intent = []string{"panic"}
	out := collapseLines(mkLines(), opt)
	if !strings.Contains(out, "panic in worker") {
		t.Errorf("intent-matched middle line was elided:\n%s", out)
	}
	if !strings.Contains(out, "lines omitted") {
		t.Error("expected the rest of the middle to still be elided")
	}
}

// The core cache-safety guarantee: once an original is compacted, the pin cache
// freezes its bytes, so a later turn with *different* intent reuses them verbatim
// — the forwarded bytes never change, so the prompt-cache prefix is preserved.
func TestIntentPinningKeepsBytesStable(t *testing.T) {
	opt := defaultCompactOpts()
	big := strings.Repeat("y", 4000)
	input := fmt.Sprintf(`{"alpha":%q,"beta":%q}`, big, big)

	// Sanity: intent actually changes the shape when not pinned.
	a := opt
	a.Intent = []string{"alpha"}
	b := opt
	b.Intent = []string{"beta"}
	o1, _ := compactToolText(input, a, nil)
	o2, _ := compactToolText(input, b, nil)
	if o1 == o2 {
		t.Fatal("intent had no effect: alpha- vs beta-biased output should differ")
	}

	// With a shared pin cache, the first compaction is frozen: flipping intent on
	// the next turn returns identical bytes.
	ctx := &compactCtx{pin: newPinCache()}
	turn1, _ := compactToolText(input, a, ctx)
	turn2, _ := compactToolText(input, b, ctx) // different intent, same original
	if turn1 != turn2 {
		t.Errorf("pinned output changed across turns (cache would bust):\n%s\n---\n%s", turn1, turn2)
	}
}
