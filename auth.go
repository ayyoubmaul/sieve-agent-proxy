package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Credential is a single stored provider credential. It is self-describing:
// it carries everything needed to inject auth headers and (for OAuth) to
// refresh itself, so the request path never needs the provider registry.
type Credential struct {
	Type string `json:"type"` // "apikey" | "oauth"

	// apikey
	Key string `json:"key,omitempty"`

	// oauth
	Access   string `json:"access,omitempty"`
	Refresh  string `json:"refresh,omitempty"`
	Expires  int64  `json:"expires,omitempty"` // unix seconds
	ClientID string `json:"client_id,omitempty"`
	TokenURL string `json:"token_url,omitempty"`

	// injection
	Header string            `json:"header,omitempty"` // "authorization" (default) | "x-api-key"
	Extra  map[string]string `json:"extra,omitempty"`  // extra headers (e.g. account id)
}

type authStore struct {
	Providers map[string]*Credential `json:"providers"`
}

// tokenHTTP is used for OAuth token endpoint calls. It has a timeout so a hung
// endpoint can't stall a refresh (which runs while the credential lock is held).
var tokenHTTP = &http.Client{Timeout: 30 * time.Second}

// Auth is the on-disk credential manager. Safe for concurrent use.
type Auth struct {
	mu   sync.Mutex
	path string
	data authStore
}

// authPath resolves the credentials file location.
// Override with AUTH_FILE; defaults to ~/.sieve/auth.json.
func authPath() string {
	if p := os.Getenv("AUTH_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".sieve-auth.json"
	}
	return filepath.Join(home, ".sieve", "auth.json")
}

// LoadAuth reads the credentials file (or starts empty if absent).
func LoadAuth() *Auth {
	a := &Auth{path: authPath(), data: authStore{Providers: map[string]*Credential{}}}
	b, err := os.ReadFile(a.path)
	if err != nil {
		return a // no file yet — empty store
	}
	var parsed authStore
	if json.Unmarshal(b, &parsed) == nil && parsed.Providers != nil {
		a.data = parsed
	}
	return a
}

// saveLocked persists the store. Caller must hold a.mu.
func (a *Auth) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a.data, "", "  ")
	if err != nil {
		return err
	}
	// 0600: owner read/write only (mirrors OpenCode's auth.json perms)
	return os.WriteFile(a.path, b, 0o600)
}

// Set stores (or replaces) a credential and persists immediately.
func (a *Auth) Set(id string, c *Credential) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.data.Providers == nil {
		a.data.Providers = map[string]*Credential{}
	}
	a.data.Providers[id] = c
	return a.saveLocked()
}

// Delete removes a credential.
func (a *Auth) Delete(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.data.Providers, id)
	return a.saveLocked()
}

// List returns the stored provider ids and their types.
func (a *Auth) List() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for id, c := range a.data.Providers {
		out[id] = c.Type
	}
	return out
}

// Has reports whether a credential exists for id.
func (a *Auth) Has(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.data.Providers[id]
	return ok
}

// Inject attaches the stored credential for id to req, refreshing an expired
// OAuth token first if needed. Returns an error if no credential exists.
func (a *Auth) Inject(req *http.Request, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	c := a.data.Providers[id]
	if c == nil {
		return fmt.Errorf("no stored credential for provider %q", id)
	}

	if c.Type == "oauth" && c.Refresh != "" && c.Expires > 0 &&
		time.Now().Unix() > c.Expires-300 { // refresh within 5 min of expiry
		if err := a.refreshLocked(c); err != nil {
			return fmt.Errorf("token refresh failed for %q: %w", id, err)
		}
	}

	value := c.Key
	if c.Type == "oauth" {
		value = c.Access
	}
	if strings.EqualFold(c.Header, "x-api-key") {
		req.Header.Set("x-api-key", value)
	} else {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	for k, v := range c.Extra {
		req.Header.Set(k, v)
	}
	return nil
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	IDToken      string `json:"id_token"`
}

// refreshLocked exchanges the refresh token for a fresh access token.
// Caller must hold a.mu.
func (a *Auth) refreshLocked(c *Credential) error {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", c.Refresh)
	if c.ClientID != "" {
		form.Set("client_id", c.ClientID)
	}

	resp, err := tokenHTTP.PostForm(c.TokenURL, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr tokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return err
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("token endpoint returned no access_token")
	}

	c.Access = tr.AccessToken
	if tr.RefreshToken != "" {
		c.Refresh = tr.RefreshToken // some providers rotate refresh tokens
	}
	ttl := tr.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	c.Expires = time.Now().Unix() + ttl
	return a.saveLocked()
}
