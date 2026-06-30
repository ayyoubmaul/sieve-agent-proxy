package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTrimAnthropicResponse(t *testing.T) {
	// A text block with trailing spaces and an oversized blank run, plus a
	// tool_use block that must be passed through untouched, plus usage.
	in := `{"id":"msg_1","type":"message","content":[` +
		`{"type":"text","text":"Here is the result   \n\n\n\nDone.   "},` +
		`{"type":"tool_use","id":"t1","name":"run","input":{"cmd":"ls   "}}` +
		`],"usage":{"input_tokens":10,"output_tokens":20}}`

	out := trimAnthropicResponse([]byte(in))

	var obj struct {
		Content []map[string]json.RawMessage `json:"content"`
		Usage   struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}

	var text string
	_ = json.Unmarshal(obj.Content[0]["text"], &text)
	if strings.Contains(text, "result   ") || strings.Contains(text, "\n\n\n") {
		t.Errorf("text not trimmed: %q", text)
	}
	if text != "Here is the result\n\nDone." {
		t.Errorf("text = %q, want collapsed/trimmed", text)
	}

	// tool_use input must be byte-identical (its trailing spaces survive).
	var input struct {
		Cmd string `json:"cmd"`
	}
	_ = json.Unmarshal(obj.Content[1]["input"], &input)
	if input.Cmd != "ls   " {
		t.Errorf("tool_use input was altered: %q, want %q", input.Cmd, "ls   ")
	}

	// usage must survive the re-marshal.
	if obj.Usage.OutputTokens != 20 {
		t.Errorf("usage.output_tokens = %d, want 20", obj.Usage.OutputTokens)
	}
}

func TestTrimResponsePreservesCodeIndentation(t *testing.T) {
	// Leading indentation inside the reply must be preserved (code-safe).
	in := `{"content":[{"type":"text","text":"def f():\n    return 1   \n\n\n\nx"}]}`
	out := trimAnthropicResponse([]byte(in))

	var obj struct {
		Content []map[string]json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(out, &obj)
	var text string
	_ = json.Unmarshal(obj.Content[0]["text"], &text)

	if !strings.Contains(text, "    return 1") {
		t.Errorf("indentation lost: %q", text)
	}
	if strings.Contains(text, "return 1   ") {
		t.Error("trailing whitespace not stripped")
	}
	if strings.Contains(text, "\n\n\n") {
		t.Error("blank run not collapsed")
	}
}

func TestTrimOpenAIResponse(t *testing.T) {
	in := `{"choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"Sure thing   \n\n\n\nbye","tool_calls":[{"id":"c1"}]}}]}`
	out := trimOpenAIResponse([]byte(in))

	var obj struct {
		Choices []struct {
			Message struct {
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Choices[0].Message.Content != "Sure thing\n\nbye" {
		t.Errorf("content = %q, want trimmed", obj.Choices[0].Message.Content)
	}
	if len(obj.Choices[0].Message.ToolCalls) == 0 {
		t.Error("tool_calls dropped")
	}
}

func TestTrimResponseSkipsNonText(t *testing.T) {
	// OpenAI null content (tool-only reply): must be returned unchanged.
	in := `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]}}]}`
	if got := string(trimOpenAIResponse([]byte(in))); got != in {
		t.Errorf("null-content reply was modified:\n got %s\nwant %s", got, in)
	}

	// Unrecognised shape: returned unchanged.
	weird := `{"something":"else"}`
	if got := string(trimAnthropicResponse([]byte(weird))); got != weird {
		t.Errorf("unrecognised shape modified: %s", got)
	}
}

func TestTrimResponseGating(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"x   \n\n\n\ny"}]}`)

	srv := &Server{cfg: &Config{}}
	// Master off → unchanged.
	if got := string(srv.trimResponse(body, true)); got != string(body) {
		t.Error("trim ran with Output.Enabled=false")
	}
	// Master on, lever off → unchanged.
	srv.cfg.Output.Enabled = true
	if got := string(srv.trimResponse(body, true)); got != string(body) {
		t.Error("trim ran with TrimResponse=false")
	}
	// Both on → trimmed.
	srv.cfg.Output.TrimResponse = true
	if got := string(srv.trimResponse(body, true)); got == string(body) {
		t.Error("trim did not run with both flags on")
	}
}
