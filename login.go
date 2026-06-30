package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// oauthProvider describes how to run an OAuth 2.0 PKCE login for a provider.
//
// These are TEMPLATES. The values below are public client identifiers that CLI
// tools use, included as working examples. Before relying on subscription-based
// OAuth through a third-party proxy, confirm the provider's terms of service
// allow it — for many providers an API key is the sanctioned path.
type oauthProvider struct {
	ClientID     string
	AuthorizeURL string
	TokenURL     string
	Scopes       string
	RedirectPort int
	RedirectPath string            // callback path; defaults to "/callback"
	Header       string            // injected header style: "authorization" | "x-api-key"
	Extra        map[string]string // static extra headers, if any
	AuthExtra    map[string]string // extra query params on the authorize URL
	// AccountIDHeader, if set, extracts chatgpt_account_id from the returned
	// id_token and stores it as this header (e.g. "ChatGPT-Account-Id").
	AccountIDHeader string
}

// oauthProviders is the built-in registry. Add your own here, or rely on the
// API-key path for any provider not listed.
var oauthProviders = map[string]oauthProvider{
	// ChatGPT subscription login via the Codex public OAuth client.
	// See the caveats in the README: the resulting token authenticates against
	// the ChatGPT backend (Responses API), not api.openai.com/v1, and using a
	// ChatGPT subscription through a third-party tool is a ToS gray area.
	"chatgpt": {
		ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		AuthorizeURL: "https://auth.openai.com/oauth/authorize",
		TokenURL:     "https://auth.openai.com/oauth/token",
		Scopes:       "openid profile email offline_access",
		RedirectPort: 1455,
		RedirectPath: "/auth/callback",
		Header:       "authorization",
		AuthExtra: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
		},
		AccountIDHeader: "ChatGPT-Account-Id",
	},
}

// RunLogin dispatches to the OAuth flow if the provider is registered,
// otherwise saves an API key for it.
func RunLogin(store *Auth, providerID, keyFlag, headerFlag string) error {
	if p, ok := oauthProviders[providerID]; ok {
		return oauthLogin(store, providerID, p)
	}
	return apiKeyLogin(store, providerID, keyFlag, headerFlag)
}

func apiKeyLogin(store *Auth, providerID, keyFlag, headerFlag string) error {
	key := strings.TrimSpace(keyFlag)
	if key == "" {
		fmt.Printf("Enter API key for %q: ", providerID)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		key = strings.TrimSpace(line)
	}
	if key == "" {
		return fmt.Errorf("no API key provided")
	}

	header := strings.ToLower(strings.TrimSpace(headerFlag))
	if header == "" {
		// Default to Bearer; use x-api-key only if the id clearly means Anthropic.
		if strings.Contains(strings.ToLower(providerID), "anthropic") {
			header = "x-api-key"
		} else {
			header = "authorization"
		}
	}

	cred := &Credential{Type: "apikey", Key: key, Header: header}
	if err := store.Set(providerID, cred); err != nil {
		return err
	}
	fmt.Printf("✅ Saved API key for %q (header: %s)\n", providerID, header)
	fmt.Printf("   Set AUTH_PROVIDER=%s in your .env to auto-inject it.\n", providerID)
	return nil
}

func oauthLogin(store *Auth, providerID string, p oauthProvider) error {
	verifier, challenge, err := pkce()
	if err != nil {
		return err
	}
	state, err := randString(16)
	if err != nil {
		return err
	}
	redirectPath := p.RedirectPath
	if redirectPath == "" {
		redirectPath = "/callback"
	}
	redirect := fmt.Sprintf("http://localhost:%d%s", p.RedirectPort, redirectPath)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("scope", p.Scopes)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	for k, v := range p.AuthExtra {
		q.Set(k, v)
	}
	authURL := p.AuthorizeURL + "?" + q.Encode()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(redirectPath, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization error: "+e, http.StatusBadRequest)
			errCh <- fmt.Errorf("authorization error: %s", e)
			return
		}
		code := r.URL.Query().Get("code")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body style='font-family:sans-serif;background:#0a0a0a;color:#eee;text-align:center;padding-top:80px'><h2>✅ Login complete</h2><p>You can close this tab and return to the terminal.</p></body></html>")
		codeCh <- code
	})

	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", p.RedirectPort), Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	fmt.Printf("Opening browser to authorize %q…\n", providerID)
	fmt.Printf("If it doesn't open, visit:\n\n  %s\n\n", authURL)
	_ = openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Minute):
		return fmt.Errorf("timed out waiting for browser authorization")
	}
	if code == "" {
		return fmt.Errorf("no authorization code received")
	}

	// Exchange the code for tokens.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", p.ClientID)
	form.Set("code_verifier", verifier)

	resp, err := tokenHTTP.PostForm(p.TokenURL, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return err
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("token exchange returned no access_token")
	}

	ttl := tr.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	header := p.Header
	if header == "" {
		header = "authorization"
	}

	// Merge static extra headers with any extracted account id.
	extra := map[string]string{}
	for k, v := range p.Extra {
		extra[k] = v
	}
	if p.AccountIDHeader != "" && tr.IDToken != "" {
		if acct := parseAccountID(tr.IDToken); acct != "" {
			extra[p.AccountIDHeader] = acct
			fmt.Printf("   account id: %s\n", acct)
		} else {
			fmt.Printf("   ⚠️  could not extract account id from id_token — backend may 401\n")
		}
	}
	if len(extra) == 0 {
		extra = nil
	}

	cred := &Credential{
		Type:     "oauth",
		Access:   tr.AccessToken,
		Refresh:  tr.RefreshToken,
		Expires:  time.Now().Unix() + ttl,
		ClientID: p.ClientID,
		TokenURL: p.TokenURL,
		Header:   header,
		Extra:    extra,
	}
	if err := store.Set(providerID, cred); err != nil {
		return err
	}
	fmt.Printf("\n✅ OAuth login complete for %q (token expires in %ds, auto-refreshed)\n", providerID, ttl)
	fmt.Printf("   Set AUTH_PROVIDER=%s in your .env to auto-inject it.\n", providerID)
	return nil
}

// ── PKCE + helpers ───────────────────────────────────────────────────────

func pkce() (verifier, challenge string, err error) {
	buf := make([]byte, 48)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

// parseAccountID extracts chatgpt_account_id from an OpenAI id_token (JWT).
// The claim lives under "https://api.openai.com/auth".chatgpt_account_id.
func parseAccountID(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	// JWT uses base64url without padding.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}
