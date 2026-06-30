package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// validJSON fails the test if s is not parseable JSON.
func validJSON(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, s)
	}
	return v
}

func TestCompactToolTextGating(t *testing.T) {
	opt := defaultCompactOpts()

	// Small input is returned byte-identical.
	small := `{"ok":true,"rows":3}`
	if out, changed := compactToolText(small, opt, nil); changed || out != small {
		t.Errorf("small input was altered: changed=%v out=%q", changed, out)
	}

	// A large repetitive log is compacted and shrinks.
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "2024-01-01T10:00:%02d INFO retrying upstream connection attempt\n", i%60)
	}
	out, changed := compactToolText(b.String(), opt, nil)
	if !changed {
		t.Fatal("large repetitive log was not compacted")
	}
	if len(out) >= b.Len() {
		t.Errorf("compacted output not smaller: %d >= %d", len(out), b.Len())
	}
}

func TestCompactJSONObjectPreservesKeys(t *testing.T) {
	opt := defaultCompactOpts()
	input := `{"id":123,"name":"alice","nested":{"k":1.50},"blob":"` + strings.Repeat("x", 1000) + `"}`

	out, ok := compactJSON(input, opt)
	if !ok {
		t.Fatal("compactJSON returned ok=false on valid JSON")
	}
	v := validJSON(t, out)
	m, isMap := v.(map[string]interface{})
	if !isMap {
		t.Fatalf("output is not an object: %s", out)
	}
	for _, k := range []string{"id", "name", "nested", "blob"} {
		if _, present := m[k]; !present {
			t.Errorf("key %q was dropped", k)
		}
	}
	if m["name"] != "alice" {
		t.Errorf("name = %v, want alice", m["name"])
	}
	// json.Number keeps numeric tokens exact (no float mangling of 1.50 → 1.5).
	if !strings.Contains(out, "1.50") {
		t.Errorf("number 1.50 was reformatted: %s", out)
	}
	blob, _ := m["blob"].(string)
	if !strings.Contains(blob, "chars]") || len([]rune(blob)) > opt.MaxStringRunes+32 {
		t.Errorf("blob not truncated with marker: %q", blob)
	}
}

func TestCompactJSONArrayHeadTail(t *testing.T) {
	opt := defaultCompactOpts()
	parts := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		parts = append(parts, fmt.Sprintf(`{"row":%d,"v":"r%02d"}`, i, i))
	}
	input := "[" + strings.Join(parts, ",") + "]"

	out, ok := compactJSON(input, opt)
	if !ok {
		t.Fatal("compactJSON ok=false")
	}
	arr, isArr := validJSON(t, out).([]interface{})
	if !isArr {
		t.Fatalf("output is not an array: %s", out)
	}
	want := opt.HeadItems + opt.TailItems + 1
	if len(arr) != want {
		t.Fatalf("array len = %d, want %d", len(arr), want)
	}
	if !strings.Contains(out, "more items") {
		t.Errorf("missing elision marker: %s", out)
	}
	// First head element and last tail element must be the true boundaries.
	first, _ := arr[0].(map[string]interface{})
	last, _ := arr[len(arr)-1].(map[string]interface{})
	if fmt.Sprint(first["row"]) != "0" {
		t.Errorf("first kept row = %v, want 0", first["row"])
	}
	if fmt.Sprint(last["row"]) != "99" {
		t.Errorf("last kept row = %v, want 99", last["row"])
	}
}

func TestCompactJSONDeterministicAndIdempotent(t *testing.T) {
	opt := defaultCompactOpts()
	// Many keys → exercises map-key ordering stability.
	input := `{"zeta":"` + strings.Repeat("z", 800) + `","alpha":1,"mid":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30],"beta":{"x":"y"}}`

	first, ok := compactJSON(input, opt)
	if !ok {
		t.Fatal("ok=false")
	}
	for i := 0; i < 5; i++ {
		again, _ := compactJSON(input, opt)
		if again != first {
			t.Fatalf("non-deterministic output on run %d:\n%s\n---\n%s", i, first, again)
		}
	}
	// Idempotent: compacting the compacted form is a no-op.
	twice, _ := compactJSON(first, opt)
	if twice != first {
		t.Errorf("not idempotent:\n%s\n---\n%s", first, twice)
	}
}

func TestCollapseLinesDedupesRepeats(t *testing.T) {
	opt := defaultCompactOpts()
	lines := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		lines = append(lines, fmt.Sprintf("2024-01-01T10:00:%02d WARN disk usage high on node-7", i))
	}
	out := collapseLines(lines, opt)
	if strings.Count(out, "\n") != 0 {
		t.Errorf("12 same-shape lines should collapse to one line, got:\n%s", out)
	}
	if !strings.Contains(out, "×12") {
		t.Errorf("missing ×12 run marker: %s", out)
	}
}

func TestCollapseLinesHeadTailCap(t *testing.T) {
	opt := defaultCompactOpts()
	lines := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		// Distinct, non-numeric shapes so runs never collapse.
		tag := string(rune('A'+i%26)) + string(rune('A'+(i/26)%26))
		lines = append(lines, "event-"+tag+" handled by worker pool")
	}
	out := collapseLines(lines, opt)
	got := strings.Count(out, "\n") + 1
	want := opt.HeadLines + opt.TailLines + 1
	if got != want {
		t.Errorf("line count = %d, want %d", got, want)
	}
	if !strings.Contains(out, "lines omitted") {
		t.Errorf("missing omitted-lines marker: %s", out)
	}
	if !strings.HasPrefix(out, "event-AA") {
		t.Errorf("head not preserved, got prefix: %.20q", out)
	}
}

func TestCompactTable(t *testing.T) {
	opt := defaultCompactOpts()
	var b strings.Builder
	b.WriteString("| id | name  | value |\n")
	b.WriteString("|----|-------|-------|\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "| %d | name%d | %d |\n", i, i, i*10)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")

	out, ok := compactTable(lines, opt)
	if !ok {
		t.Fatal("compactTable did not detect a pipe table")
	}
	if !strings.HasPrefix(out, "| id | name  | value |") {
		t.Errorf("header row not preserved:\n%s", out)
	}
	if !strings.Contains(out, "rows omitted") {
		t.Errorf("missing rows-omitted marker:\n%s", out)
	}
	got := strings.Count(out, "\n") + 1
	want := opt.HeadLines + opt.TailLines + 1
	if got != want {
		t.Errorf("table line count = %d, want %d", got, want)
	}
}

func TestCompactTableRejectsProse(t *testing.T) {
	opt := defaultCompactOpts()
	lines := []string{
		"This is a paragraph of normal prose output from a tool.",
		"It has several lines but no column structure at all.",
		"Line three continues the thought without any pipes.",
		"Line four as well, still prose.",
		"Line five, six, seven, eight, nine to clear the row count.",
		"Six.", "Seven.", "Eight.", "Nine.",
	}
	if _, ok := compactTable(lines, opt); ok {
		t.Error("prose was misdetected as a table")
	}
}

func TestLineShapeNormalizesVolatileTokens(t *testing.T) {
	got := lineShape("2024-01-01T10:00:00Z req 550e8400-e29b-41d4-a716-446655440000 took 123ms")
	want := "<ts> req <uuid> took <n>ms"
	if got != want {
		t.Errorf("lineShape = %q, want %q", got, want)
	}
	// Two log lines differing only in volatile tokens share a shape.
	a := lineShape("2024-06-01T09:15:42 GET /users/42 200 in 13ms")
	b := lineShape("2024-06-01T09:15:43 GET /users/99 200 in 8ms")
	if a != b {
		t.Errorf("shapes differ but should match:\n%q\n%q", a, b)
	}
}

func TestCompactToolResultsAnthropic(t *testing.T) {
	opt := defaultCompactOpts()
	bigLog := strings.Repeat("2024-01-01T10:00:00Z WARN repeated line here\n", 300)
	bigLogJSON, _ := json.Marshal(bigLog)

	body := fmt.Sprintf(`[
		{"role":"user","content":[{"type":"text","text":"run the thing"}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":%s},
			{"type":"tool_result","tool_use_id":"toolu_2","content":"short ok"}
		]}
	]`, bigLogJSON)

	var msgs []Message
	if err := json.Unmarshal([]byte(body), &msgs); err != nil {
		t.Fatal(err)
	}
	out, saved := compactToolResults(msgs, opt, nil)
	if saved <= 0 {
		t.Fatal("expected savings from the big tool_result")
	}

	// The first message (plain user text) must be untouched.
	if string(out[0].Content) != string(msgs[0].Content) {
		t.Error("non-tool message was modified")
	}
	// The big tool_result shrank; the short one is byte-identical.
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(out[1].Content, &blocks); err != nil {
		t.Fatal(err)
	}
	var big, short string
	_ = json.Unmarshal(blocks[0]["content"], &big)
	_ = json.Unmarshal(blocks[1]["content"], &short)
	if len(big) >= len(bigLog) {
		t.Errorf("big tool_result not compacted: %d", len(big))
	}
	if short != "short ok" {
		t.Errorf("short tool_result altered: %q", short)
	}
	// tool_use_id and other fields preserved.
	if _, ok := blocks[0]["tool_use_id"]; !ok {
		t.Error("tool_use_id dropped")
	}
}

func TestCompactToolResultsOpenAI(t *testing.T) {
	opt := defaultCompactOpts()
	rows := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":%d,"name":"n%d"}`, i, i))
	}
	payload := "[" + strings.Join(rows, ",") + "]"
	payloadJSON, _ := json.Marshal(payload)

	body := fmt.Sprintf(`[
		{"role":"user","content":"query the db"},
		{"role":"tool","tool_call_id":"call_1","content":%s}
	]`, payloadJSON)

	var msgs []Message
	if err := json.Unmarshal([]byte(body), &msgs); err != nil {
		t.Fatal(err)
	}
	out, saved := compactToolResults(msgs, opt, nil)
	if saved <= 0 {
		t.Fatal("expected savings from the OpenAI tool message")
	}
	var compacted string
	_ = json.Unmarshal(out[1].Content, &compacted)
	if len(compacted) >= len(payload) {
		t.Errorf("tool message not compacted: %d", len(compacted))
	}
	validJSON(t, compacted) // still valid JSON after compaction
	// tool_call_id preserved in Extra.
	if _, ok := out[1].Extra["tool_call_id"]; !ok {
		t.Error("tool_call_id dropped")
	}
}

func TestTruncateLongSingleLine(t *testing.T) {
	opt := defaultCompactOpts()
	blob := strings.Repeat("abcd", 5000) // 20k single line, no newlines
	out := collapseLines([]string{blob}, opt)
	if len([]rune(out)) > opt.MaxStringRunes*4+32 {
		t.Errorf("runaway single line not capped: %d runes", len([]rune(out)))
	}
	if !strings.Contains(out, "chars]") {
		t.Errorf("missing char-elision marker")
	}
}
