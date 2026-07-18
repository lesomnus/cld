// Package usage queries Anthropic's undocumented subscription usage endpoint,
// the same one Claude Code's `/usage` command reads. Given a claude.ai OAuth
// access token it returns the account's rate-limit window state (the 5-hour and
// weekly utilization and reset times).
//
// This is NOT a documented, supported API — the path, headers, and response
// shape were determined empirically and can change without notice, exactly like
// the token endpoint in internal/broker. In particular the `User-Agent:
// claude-code/<version>` header is REQUIRED: without it the request lands in an
// aggressively rate-limited bucket that returns persistent 429s. With it the
// endpoint tolerates polling at ~180s intervals, per access token.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoint is the undocumented usage endpoint. It lives on api.anthropic.com and
// authenticates with the subscription OAuth access token (Bearer), not an API key.
const Endpoint = "https://api.anthropic.com/api/oauth/usage"

// betaHeader is the anthropic-beta value the OAuth surface requires.
const betaHeader = "oauth-2025-04-20"

// FallbackVersion is used to build the User-Agent when the caller has no real
// claude version to offer. Any `claude-code/<x>` string keeps the request out
// of the rate-limited bucket; a real container version is preferred when known.
const FallbackVersion = "2.1.205"

// Window is one rate-limit window's state: how much of it is consumed (0–100)
// and when it resets. ResetsAt is zero when the endpoint reports it as null.
type Window struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

// Limit is one entry of the endpoint's unified `limits` array — a cleaner,
// forward-compatible view than the individual `seven_day_*` fields, carrying its
// own severity and active flag so a renderer needs no side lookups.
type Limit struct {
	Kind     string    `json:"kind"`
	Group    string    `json:"group"`
	Percent  float64   `json:"percent"`
	Severity string    `json:"severity"`
	ResetsAt time.Time `json:"resets_at"`
	IsActive bool      `json:"is_active"`
}

// Usage is the parsed subset of the endpoint's response we rely on. Unknown
// fields — including internal code-named buckets the endpoint keeps adding
// (omelette, iguana_necktie, …) — are ignored on purpose so a new bucket never
// breaks parsing.
type Usage struct {
	FiveHour Window  `json:"five_hour"`
	SevenDay Window  `json:"seven_day"`
	Limits   []Limit `json:"limits,omitempty"`
}

// StatusError is returned when the endpoint answers with a non-200 status, so a
// caller can distinguish auth failures (401, a stale token) from throttling
// (429, poll too fast / missing User-Agent) from everything else.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("usage endpoint: %s: %s", http.StatusText(e.Code), e.Body)
	}
	return fmt.Sprintf("usage endpoint: %s", http.StatusText(e.Code))
}

// Fetch queries the usage endpoint with the given OAuth access token, tagging
// the request as claude-code/<version> so it avoids the throttled bucket. A nil
// client uses http.DefaultClient; an empty version uses FallbackVersion.
func Fetch(ctx context.Context, hc *http.Client, token, version string) (*Usage, error) {
	return FetchAt(ctx, hc, Endpoint, token, version)
}

// FetchAt is Fetch against an explicit URL, so a test can point it at a stub
// server. Production callers use Fetch, which targets Endpoint.
func FetchAt(ctx context.Context, hc *http.Client, url, token, version string) (*Usage, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if version == "" {
		version = FallbackVersion
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", betaHeader)
	req.Header.Set("User-Agent", "claude-code/"+version)
	req.Header.Set("Content-Type", "application/json")

	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode != http.StatusOK {
		return nil, &StatusError{Code: res.StatusCode, Body: strings.TrimSpace(string(raw))}
	}

	var u Usage
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}
	return &u, nil
}
