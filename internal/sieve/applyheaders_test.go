package sieve

import (
	"net/http"
	"testing"
)

// newReqWith builds a bare incoming request carrying the given headers.
func newReqWith(headers map[string]string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestApplyHeadersOAuthTokenStaysBearer(t *testing.T) {
	s := &Server{cfg: &Config{}}
	orig := newReqWith(map[string]string{
		"Authorization": "Bearer sk-ant-oat01-abc123",
		"x-app":         "cli",
		"User-Agent":    "claude-cli/1.0",
	})
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)

	s.applyHeaders(req, orig, true, Upstream{})

	if got := req.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-abc123" {
		t.Errorf("OAuth token must stay a Bearer token, got Authorization=%q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("OAuth token must not be sent as x-api-key, got %q", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Errorf("OAuth requests need the oauth beta flag, got %q", got)
	}
	if got := req.Header.Get("x-app"); got != "cli" {
		t.Errorf("identity header x-app must be forwarded, got %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "claude-cli/1.0" {
		t.Errorf("User-Agent must be forwarded, got %q", got)
	}
}

func TestApplyHeadersAPIKeyUsesXAPIKey(t *testing.T) {
	s := &Server{cfg: &Config{}}
	orig := newReqWith(map[string]string{"x-api-key": "sk-ant-api03-xyz"})
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)

	s.applyHeaders(req, orig, true, Upstream{})

	if got := req.Header.Get("x-api-key"); got != "sk-ant-api03-xyz" {
		t.Errorf("API key must travel as x-api-key, got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("API key must not be sent as a Bearer token, got %q", got)
	}
}

func TestMergeBeta(t *testing.T) {
	cases := []struct{ existing, flag, want string }{
		{"", "oauth-2025-04-20", "oauth-2025-04-20"},
		{"claude-code-20250219", "oauth-2025-04-20", "claude-code-20250219,oauth-2025-04-20"},
		{"oauth-2025-04-20", "oauth-2025-04-20", "oauth-2025-04-20"},
		{"a, oauth-2025-04-20 ", "oauth-2025-04-20", "a, oauth-2025-04-20 "},
	}
	for _, c := range cases {
		if got := mergeBeta(c.existing, c.flag); got != c.want {
			t.Errorf("mergeBeta(%q,%q)=%q want %q", c.existing, c.flag, got, c.want)
		}
	}
}
