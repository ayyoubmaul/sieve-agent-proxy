package sieve

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Minimal MCP (Model Context Protocol) stdio server exposing a single tool,
// sieve_fetch, backed by the content-addressed Store. Run as `sieve mcp` and
// registered with a coding agent (e.g. `claude mcp add sieve -- sieve mcp`), it
// lets the model pull back the full original of any tool result the proxy
// compacted — using the ref shown in the compaction marker.
//
// The proxy and this server are separate processes; they share state only
// through ~/.sieve/store on disk, so this server needs nothing running. The
// transport is newline-delimited JSON-RPC 2.0 over stdin/stdout — pure stdlib.
// stdout carries protocol bytes only; logging (if any) goes to stderr.

const mcpProtocolVersion = "2024-11-05"
const sieveVersion = "0.1.0"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// cmdMCP runs the stdio server loop until stdin closes.
func CmdMCP(args []string) {
	store := NewStore()
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			handleRPCLine(line, store, out)
			_ = out.Flush()
		}
		if err != nil {
			return // EOF or read error → done
		}
	}
}

func handleRPCLine(line []byte, store *Store, out *bufio.Writer) {
	var req rpcRequest
	if json.Unmarshal(bytes.TrimSpace(line), &req) != nil {
		return // unparseable → ignore (can't form a valid error reply without an id)
	}
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		writeRPCResult(out, req.ID, map[string]interface{}{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "sieve", "version": sieveVersion},
		})
	case "tools/list":
		writeRPCResult(out, req.ID, map[string]interface{}{"tools": []interface{}{sieveFetchTool()}})
	case "tools/call":
		handleToolCall(req, store, out)
	case "ping":
		writeRPCResult(out, req.ID, map[string]interface{}{})
	default:
		if !isNotification {
			writeRPCError(out, req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func sieveFetchTool() map[string]interface{} {
	return map[string]interface{}{
		"name": "fetch",
		"description": "Retrieve the full, original content of a tool result that sieve compacted. " +
			"When a tool result shows a marker like [sieve compacted ... call sieve_fetch(\"REF\") ...], " +
			"pass that REF here to get the complete untruncated content. (The marker prints the " +
			"host-exposed name, typically the server prefix plus this tool — e.g. \"sieve_fetch\".)",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{
					"type":        "string",
					"description": "The 12-character ref id from a sieve compaction marker.",
				},
			},
			"required": []interface{}{"ref"},
		},
	}
}

func handleToolCall(req rpcRequest, store *Store, out *bufio.Writer) {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Ref string `json:"ref"`
		} `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &p)

	// Accept the bare "fetch" name and the legacy "sieve_fetch" alias (some
	// hosts call the tool by its previously-registered name).
	if p.Name != "fetch" && p.Name != "sieve_fetch" {
		writeRPCError(out, req.ID, -32602, "unknown tool: "+p.Name)
		return
	}
	if text, ok := store.Get(p.Arguments.Ref); ok {
		writeRPCResult(out, req.ID, toolTextResult(text, false))
		return
	}
	writeRPCResult(out, req.ID, toolTextResult(
		fmt.Sprintf("No stored content for ref %q — it may never have been compacted, or the store was cleared.", p.Arguments.Ref),
		true))
}

func toolTextResult(text string, isError bool) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"isError": isError,
	}
}

func writeRPCResult(out *bufio.Writer, id json.RawMessage, result interface{}) {
	writeRPC(out, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(out *bufio.Writer, id json.RawMessage, code int, msg string) {
	writeRPC(out, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func writeRPC(out *bufio.Writer, resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	out.Write(b)
	out.WriteByte('\n')
}
