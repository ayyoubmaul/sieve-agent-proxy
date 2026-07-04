package sieve

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// `sieve wrap <agent> [args...]` launches a coding agent pointed at the proxy in
// one command — the equivalent of Headroom's `headroom wrap claude`. It starts
// the proxy in-process (or reuses one already on the port), sets the agent's
// base-URL env var so its traffic flows through sieve, and execs the agent with
// any remaining args passed through.

// agentSpec describes how to launch and repoint one agent.
type agentSpec struct {
	bin       string // executable to look up on PATH
	anthropic bool   // set ANTHROPIC_BASE_URL (Claude Code)
	openai    bool   // set OPENAI_BASE_URL (OpenAI-compatible clients)
	litellm   bool   // also set *_API_BASE aliases (aider routes via litellm)
	note      string // surfaced caveat, if any
}

var wrapAgents = map[string]agentSpec{
	"claude": {bin: "claude", anthropic: true},
	"codex":  {bin: "codex", openai: true},
	"aider":  {bin: "aider", anthropic: true, openai: true, litellm: true},
	"cursor": {bin: "cursor-agent", openai: true,
		note: "cursor-agent may ignore a custom base URL; routing is best-effort."},
}

// wrapSpec resolves an agent name to its spec. copilot is rejected with an
// explanation (it can't be repointed); unknown names get a best-effort spec.
func wrapSpec(name string) (agentSpec, error) {
	if name == "copilot" {
		return agentSpec{}, fmt.Errorf("the GitHub Copilot CLI routes through GitHub's backend and cannot be pointed at a custom proxy")
	}
	if s, ok := wrapAgents[name]; ok {
		return s, nil
	}
	return agentSpec{bin: name, anthropic: true, openai: true,
		note: "unknown agent; setting both base URLs (best-effort)."}, nil
}

// buildWrapEnv returns the base-URL env entries (KEY=VALUE) for a spec and the
// names that were set, given the proxy origin (e.g. http://localhost:4141).
func buildWrapEnv(spec agentSpec, origin string) (env, names []string) {
	if spec.anthropic {
		env = append(env, "ANTHROPIC_BASE_URL="+origin)
		names = append(names, "ANTHROPIC_BASE_URL")
	}
	if spec.openai {
		env = append(env, "OPENAI_BASE_URL="+origin+"/v1")
		names = append(names, "OPENAI_BASE_URL")
	}
	if spec.litellm {
		env = append(env, "OPENAI_API_BASE="+origin+"/v1", "ANTHROPIC_API_BASE="+origin)
		names = append(names, "OPENAI_API_BASE", "ANTHROPIC_API_BASE")
	}
	return env, names
}

func CmdWrap(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: sieve wrap <agent> [args...]")
		fmt.Println("agents: claude · codex · aider · cursor")
		os.Exit(1)
	}
	name := args[0]
	passthrough := args[1:]

	spec, err := wrapSpec(name)
	if err != nil {
		fmt.Printf("✗ %s: %v\n", name, err)
		os.Exit(1)
	}

	binPath, err := exec.LookPath(spec.bin)
	if err != nil {
		fmt.Printf("✗ %q not found on PATH (looked for %q)\n", name, spec.bin)
		os.Exit(1)
	}

	cfg := LoadConfig()
	origin := fmt.Sprintf("http://localhost:%s", cfg.Port)

	// Start the proxy in-process unless one is already listening on the port.
	if ln, err := net.Listen("tcp", ":"+cfg.Port); err == nil {
		srv := NewServer(cfg)
		go func() { _ = http.Serve(ln, srv.Handler()) }()
		fmt.Printf("⚡ sieve proxy on %s → %s\n", origin, cfg.TargetURL)
	} else {
		fmt.Printf("⚡ reusing proxy already on %s\n", origin)
	}

	env := os.Environ()
	extra, names := buildWrapEnv(spec, origin)
	env = append(env, extra...)

	fmt.Printf("→ launching %s (%s)\n", name, strings.Join(names, ", "))
	if spec.note != "" {
		fmt.Printf("  note: %s\n", spec.note)
	}

	// Don't let Ctrl-C kill the parent before the child cleans up — the child
	// shares the terminal and receives the signal directly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range sigCh {
		}
	}()

	cmd := exec.Command(binPath, passthrough...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Printf("✗ failed to run %s: %v\n", name, err)
		os.Exit(1)
	}
}
