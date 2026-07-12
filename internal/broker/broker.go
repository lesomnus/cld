// Package broker centralizes Claude Code subscription auth for the daemon so
// that many containers can share ONE login without the refresh-token rotation
// collisions that sharing a ~/.claude/.credentials.json across live containers
// causes. The daemon is the sole owner of the refresh token: it refreshes
// centrally and hands each session only a short-lived access token, injected at
// the wire by a reverse proxy the sessions point ANTHROPIC_BASE_URL at.
//
// Why a proxy and not an env var: an access token expires (~8h) and a session
// can outlive it, but ANTHROPIC_AUTH_TOKEN/CLAUDE_CODE_OAUTH_TOKEN are read once
// at process start and cannot be refreshed mid-session. The proxy rewrites the
// Authorization header on every request with the currently-valid token, so a
// running `claude` never restarts and never holds a refresh token itself.
package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults for the claude.ai subscription OAuth flow. These are not part of any
// documented, supported API — they are the values Claude Code itself uses,
// determined empirically — so treat them as liable to change and keep them in
// one place. The token endpoint is on claude.ai (NOT console.anthropic.com,
// which 404s the refresh grant).
const (
	DefaultTokenURL    = "https://claude.ai/v1/oauth/token"
	DefaultUpstreamURL = "https://api.anthropic.com"
	DefaultClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

// refreshMargin refreshes an access token this long before it actually expires,
// so a request never races a mid-flight expiry.
const refreshMargin = 5 * time.Minute

// Credentials is the daemon's single owned login. The refresh token rotates on
// every refresh; the daemon persists the rotation so a restart resumes cleanly.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (c *Credentials) valid(now time.Time) bool {
	return c != nil && c.AccessToken != "" && now.Before(c.ExpiresAt.Add(-refreshMargin))
}

// Store persists the rotating credentials across daemon restarts. It must be
// safe for the single Broker that owns it; the Broker serializes its own calls.
type Store interface {
	Load() (*Credentials, error)
	Save(*Credentials) error
}

// Broker owns the refresh token and vends short-lived access tokens. All token
// state is guarded by mu; a refresh is done under the lock so only one in-flight
// refresh ever hits the endpoint (later waiters see the fresh token).
type Broker struct {
	store Store
	hc    *http.Client

	tokenURL    string
	upstreamURL string
	clientID    string
	now         func() time.Time // overridable in tests

	mu    sync.Mutex
	creds *Credentials
}

// Option configures a Broker; the zero set yields the production defaults.
type Option func(*Broker)

func WithHTTPClient(hc *http.Client) Option { return func(b *Broker) { b.hc = hc } }
func WithTokenURL(u string) Option          { return func(b *Broker) { b.tokenURL = u } }
func WithUpstreamURL(u string) Option       { return func(b *Broker) { b.upstreamURL = u } }
func WithClientID(id string) Option         { return func(b *Broker) { b.clientID = id } }
func WithClock(now func() time.Time) Option { return func(b *Broker) { b.now = now } }

func New(store Store, opts ...Option) *Broker {
	b := &Broker{
		store:       store,
		hc:          &http.Client{Timeout: 30 * time.Second},
		tokenURL:    DefaultTokenURL,
		upstreamURL: DefaultUpstreamURL,
		clientID:    DefaultClientID,
		now:         time.Now,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Token returns a currently-valid access token, refreshing (and persisting the
// rotated refresh token) when the cached one is missing or within refreshMargin
// of expiry. Loads from the Store on first use so a daemon restart resumes
// without a new login.
func (b *Broker) Token(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.creds == nil {
		c, err := b.store.Load()
		if err != nil {
			return "", fmt.Errorf("load credentials: %w", err)
		}
		b.creds = c
	}
	if b.creds.valid(b.now()) {
		return b.creds.AccessToken, nil
	}
	if b.creds == nil || b.creds.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token: run `cld auth login` to set one")
	}
	if err := b.refresh(ctx); err != nil {
		return "", err
	}
	return b.creds.AccessToken, nil
}

// HasCredentials reports whether a refresh token is set, i.e. whether the broker
// is active. Loads from the Store on first use. A false result means cld should
// fall back to the older CLAUDE_CODE_OAUTH_TOKEN injection.
func (b *Broker) HasCredentials() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.creds == nil {
		c, err := b.store.Load()
		if err != nil {
			return false
		}
		b.creds = c
	}
	return b.creds.RefreshToken != ""
}

// SetCredentials replaces the owned login (from `cld auth login`) and persists
// it. Any cached token is dropped so the next Token() reflects the new login.
func (b *Broker) SetCredentials(c *Credentials) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c == nil || c.RefreshToken == "" {
		return fmt.Errorf("credentials must include a refresh token")
	}
	if err := b.store.Save(c); err != nil {
		return err
	}
	b.creds = c
	return nil
}

// refresh exchanges the refresh token for a new access token. Caller holds mu.
// The endpoint rotates the refresh token, so the response's refresh_token
// replaces ours and is persisted before we return — losing it would strand the
// login. expires_in is seconds from now.
func (b *Broker) refresh(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": b.creds.RefreshToken,
		"client_id":     b.clientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.tokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("refresh token: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh token: %s: %s", res.Status, strings.TrimSpace(string(raw)))
	}

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("refresh token: decode response: %w", err)
	}
	if out.AccessToken == "" {
		return fmt.Errorf("refresh token: response had no access_token")
	}

	next := &Credentials{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		ExpiresAt:    b.now().Add(time.Duration(out.ExpiresIn) * time.Second),
	}
	// The endpoint may (in principle) omit a rotated token; keep the current one
	// rather than blanking it, which would strand the login.
	if next.RefreshToken == "" {
		next.RefreshToken = b.creds.RefreshToken
	}
	if err := b.store.Save(next); err != nil {
		return fmt.Errorf("persist rotated credentials: %w", err)
	}
	b.creds = next
	return nil
}

// Handler is the reverse proxy each session points ANTHROPIC_BASE_URL at. It
// rewrites Authorization with the current access token on every request and
// forwards to the real upstream over its own validated TLS — it does NOT MITM
// api.anthropic.com; the session is configured to treat this proxy as its
// endpoint. On a token error it fails the request with 502 rather than leaking
// the session's placeholder token upstream.
func (b *Broker) Handler() http.Handler {
	upstream, _ := url.Parse(b.upstreamURL)
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // stream SSE immediately
		Director: func(r *http.Request) {
			r.URL.Scheme = upstream.Scheme
			r.URL.Host = upstream.Host
			r.Host = upstream.Host
			if tok, _ := r.Context().Value(tokenCtxKey{}).(string); tok != "" {
				r.Header.Set("Authorization", "Bearer "+tok)
			}
			// The session's own auth is a placeholder; never forward it.
			r.Header.Del("X-Api-Key")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, err := b.Token(r.Context())
		if err != nil {
			http.Error(w, "cld broker: "+err.Error(), http.StatusBadGateway)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), tokenCtxKey{}, tok))
		rp.ServeHTTP(w, r)
	})
}

type tokenCtxKey struct{}
