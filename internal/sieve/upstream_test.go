package sieve

import (
	"net/http"
	"testing"
)

func upstreamServer() *Server {
	return &Server{cfg: &Config{
		DefaultUpstream: "anthropic",
		Upstreams: map[string]Upstream{
			"anthropic": {Target: "https://api.anthropic.com", AuthProvider: "", AuthOverride: false},
			"gateway":   {Target: "https://gateway.example.com/v1", AuthProvider: "custom", AuthOverride: true},
			"default":   {Target: "https://api.anthropic.com"},
		},
	}}
}

func reqWithUpstream(header string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	if header != "" {
		r.Header.Set("X-Sieve-Upstream", header)
	}
	return r
}

func TestResolveUpstream(t *testing.T) {
	s := upstreamServer()
	cases := []struct {
		header     string
		wantTarget string
		wantAuth   string
	}{
		{"", "https://api.anthropic.com", ""},                    // no header → default
		{"gateway", "https://gateway.example.com/v1", "custom"},  // explicit gateway
		{"GATEWAY", "https://gateway.example.com/v1", "custom"},  // case-insensitive
		{"anthropic", "https://api.anthropic.com", ""},           // explicit anthropic
		{"bogus", "https://api.anthropic.com", ""},               // unknown → default
	}
	for _, c := range cases {
		up := s.resolveUpstream(reqWithUpstream(c.header))
		if up.Target != c.wantTarget || up.AuthProvider != c.wantAuth {
			t.Errorf("header=%q → target=%q auth=%q; want target=%q auth=%q",
				c.header, up.Target, up.AuthProvider, c.wantTarget, c.wantAuth)
		}
	}
}

func TestLoadUpstreamsDefaultFallback(t *testing.T) {
	// With no UPSTREAMS env, a "default" profile is synthesised from TargetURL.
	c := &Config{TargetURL: "https://example.com", AuthProvider: "x", AuthOverride: true}
	ups, def := loadUpstreams(c)
	if def != "default" {
		t.Fatalf("default upstream = %q, want %q", def, "default")
	}
	if got := ups["default"]; got.Target != "https://example.com" || got.AuthProvider != "x" || !got.AuthOverride {
		t.Fatalf("default profile not built from TargetURL: %+v", got)
	}
}
