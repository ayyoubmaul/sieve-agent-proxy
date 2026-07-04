package sieve

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func runRPC(store *Store, line string) string {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	handleRPCLine([]byte(line), store, w)
	_ = w.Flush()
	return buf.String()
}

func TestMCPInitialize(t *testing.T) {
	out := runRPC(newTestStore(t), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("bad response %q: %v", out, err)
	}
	if resp.Result.ProtocolVersion != mcpProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", resp.Result.ProtocolVersion, mcpProtocolVersion)
	}
	if resp.Result.ServerInfo.Name != "sieve" {
		t.Errorf("serverInfo.name = %q, want sieve", resp.Result.ServerInfo.Name)
	}
}

func TestMCPToolsList(t *testing.T) {
	out := runRPC(newTestStore(t), `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	// The tool is registered under the bare name "fetch"; the MCP host prefixes
	// the server name ("sieve") to expose it as "sieve_fetch".
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("bad response %q: %v", out, err)
	}
	if len(resp.Result.Tools) == 0 || resp.Result.Tools[0].Name != "fetch" {
		t.Errorf("tools/list should register bare name \"fetch\", got: %s", out)
	}
}

// Both the bare "fetch" name and the legacy "sieve_fetch" alias must resolve.
func TestMCPToolsCallAcceptsBothNames(t *testing.T) {
	for _, name := range []string{"fetch", "sieve_fetch"} {
		store := newTestStore(t)
		original := "original for " + name
		ref, _ := store.Put(original)
		out := runRPC(store, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":%q,"arguments":{"ref":%q}}}`, name, ref))
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatalf("%s: bad response %q: %v", name, out, err)
		}
		if resp.Result.IsError || len(resp.Result.Content) == 0 || resp.Result.Content[0].Text != original {
			t.Errorf("calling %q failed: %s", name, out)
		}
	}
}

func TestMCPToolsCallFetchesOriginal(t *testing.T) {
	store := newTestStore(t)
	original := "the FULL original content that was compacted away"
	ref, _ := store.Put(original)

	out := runRPC(store, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sieve_fetch","arguments":{"ref":%q}}}`, ref))

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("bad response %q: %v", out, err)
	}
	if resp.Result.IsError {
		t.Fatal("fetch reported isError for a valid ref")
	}
	if len(resp.Result.Content) == 0 || resp.Result.Content[0].Text != original {
		t.Errorf("fetched content = %+v, want %q", resp.Result.Content, original)
	}
}

func TestMCPToolsCallUnknownRef(t *testing.T) {
	out := runRPC(newTestStore(t),
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"sieve_fetch","arguments":{"ref":"000000000000"}}}`)
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("bad response %q: %v", out, err)
	}
	if !resp.Result.IsError {
		t.Error("unknown ref should set isError=true")
	}
}

func TestMCPNotificationNoReply(t *testing.T) {
	out := runRPC(newTestStore(t), `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if out != "" {
		t.Errorf("notification produced a reply: %q", out)
	}
}
