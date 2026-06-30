package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
)

func main() {
	// Subcommands: login / logout / auth list. Anything else starts the server.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			cmdLogin(os.Args[2:])
			return
		case "logout":
			cmdLogout(os.Args[2:])
			return
		case "auth":
			cmdAuth(os.Args[2:])
			return
		case "wrap":
			cmdWrap(os.Args[2:])
			return
		case "mcp":
			cmdMCP(os.Args[2:])
			return
		case "serve":
			// fall through to server
		case "-h", "--help", "help":
			printUsage()
			return
		}
	}
	runServer()
}

func printUsage() {
	fmt.Println(`Usage:
  (no args) | serve        Start the proxy server
  wrap <agent> [args...]   Launch a coding agent (claude|codex|aider|cursor)
                           through the proxy in one command
  mcp                      Run the sieve_fetch MCP server (stdio) so an agent can
                           retrieve originals of compacted tool results
  login -p <provider>      Log in to a provider (browser OAuth if known, else API key)
       [--key <key>]       Provide the API key non-interactively
       [--header <style>]  Header style for API keys: authorization | x-api-key
  logout -p <provider>     Remove a stored credential
  auth list                List stored credentials`)
}

func cmdLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	provider := fs.String("p", "", "provider id")
	key := fs.String("key", "", "API key (for API-key providers; omit to be prompted)")
	header := fs.String("header", "", "header style: authorization | x-api-key")
	_ = fs.Parse(args)

	if *provider == "" {
		fmt.Println("error: -p <provider> is required")
		os.Exit(1)
	}
	store := LoadAuth()
	if err := RunLogin(store, *provider, *key, *header); err != nil {
		log.Fatalf("login failed: %v", err)
	}
}

func cmdLogout(args []string) {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	provider := fs.String("p", "", "provider id")
	_ = fs.Parse(args)

	if *provider == "" {
		fmt.Println("error: -p <provider> is required")
		os.Exit(1)
	}
	store := LoadAuth()
	if !store.Has(*provider) {
		fmt.Printf("no stored credential for %q\n", *provider)
		return
	}
	if err := store.Delete(*provider); err != nil {
		log.Fatalf("logout failed: %v", err)
	}
	fmt.Printf("✅ Removed credential for %q\n", *provider)
}

func cmdAuth(args []string) {
	if len(args) == 0 || args[0] != "list" {
		fmt.Println("usage: auth list")
		return
	}
	store := LoadAuth()
	creds := store.List()
	if len(creds) == 0 {
		fmt.Println("No stored credentials. Use `login -p <provider>` to add one.")
		return
	}
	ids := make([]string, 0, len(creds))
	for id := range creds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Printf("Stored credentials (%s):\n", authPath())
	for _, id := range ids {
		fmt.Printf("  • %-20s %s\n", id, creds[id])
	}
}

func runServer() {
	cfg := LoadConfig()
	srv := NewServer(cfg)

	line := strings.Repeat("─", 56)
	fmt.Printf("\n%s\n", line)
	fmt.Println("  ⚡ LLM Compress Proxy (Go)")
	fmt.Println(line)
	fmt.Printf("  Listening  → http://localhost:%s\n", cfg.Port)
	fmt.Printf("  Target     → %s\n", cfg.TargetURL)
	fmt.Printf("  Compress   → %s\n", onOff(cfg.Compression.Enabled))
	fmt.Printf("  Tool ✂️    → %s\n", cfg.toolCompactionSummary())
	fmt.Printf("  Output     → %s\n", cfg.outputSummary())
	fmt.Printf("  Align      → %s\n", cfg.alignSummary())
	fmt.Printf("  Token $    → %s\n", cacheLine(cfg.TokenCache.Enabled,
		fmt.Sprintf("TTL %ds, max %d", cfg.TokenCache.TTL, cfg.TokenCache.MaxEntries)))
	fmt.Printf("  Semantic $ → %s\n", cacheLine(cfg.SemanticCache.Enabled,
		fmt.Sprintf("threshold %.2f", cfg.SemanticCache.Threshold)))
	if cfg.AuthProvider != "" {
		fmt.Printf("  Auth       → injecting stored credential %q\n", cfg.AuthProvider)
	}
	fmt.Printf("  Dashboard  → http://localhost:%s/dashboard\n", cfg.Port)
	fmt.Printf("%s\n\n", line)

	if err := http.ListenAndServe(":"+cfg.Port, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func onOff(b bool) string {
	if b {
		return "✅ on"
	}
	return "❌ off"
}

func cacheLine(enabled bool, detail string) string {
	if enabled {
		return "✅ on  (" + detail + ")"
	}
	return "❌ off"
}
