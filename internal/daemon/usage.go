package daemon

import (
	"context"
	"encoding/json"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/lesomnus/cld/internal/usage"
)

// UsageTTL is how long a source's fetched usage is reused before the daemon
// re-queries. The endpoint is per-token rate limited (safe around 180s), so a
// short cache keeps `cld watch`/`cld usage` polling cheap without ever hammering
// it — a minute is a good balance between freshness and headroom. It is
// exported so pollers (the `cld watch` footer) can align their own interval to
// it: querying faster than this only ever returns the same cached value.
const UsageTTL = time.Minute

// usageErrTTL caches a failed fetch briefly so a broken source (a 429, an
// expired token) is not retried on every poll, yet recovers within a minute.
const usageErrTTL = 30 * time.Second

// UsageSource is one login's usage: the broker login, or one running session's
// per-container login. Exactly one of Usage/Error is set.
type UsageSource struct {
	// Label identifies the source: "broker" for the shared broker login, or a
	// session's display name for a per-container login.
	Label string       `json:"label"`
	Usage *usage.Usage `json:"usage,omitempty"`
	Error string       `json:"error,omitempty"`
}

// UsageReport is every login the daemon can see usage for, broker first then
// sessions by name. It is empty when there is no broker login and no ready
// session to read a token from.
type UsageReport struct {
	Sources []UsageSource `json:"sources"`
}

// usageEntry is a cached per-source result, stamped with when it was fetched so
// the daemon can honor UsageTTL / usageErrTTL without a background loop.
type usageEntry struct {
	src UsageSource
	at  time.Time
}

// usageCache memoizes usage per source key ("broker" or a container id) so a
// caller polling every second only triggers a real fetch once per TTL. Keying
// by source (not token) lets a cache hit skip both the container read and the
// endpoint call.
type usageCache struct {
	mu      sync.Mutex
	entries map[string]usageEntry
	now     func() time.Time // overridable in tests
}

func newUsageCache() *usageCache {
	return &usageCache{entries: map[string]usageEntry{}, now: time.Now}
}

// fresh reports whether a cached entry is still within its TTL: UsageTTL for a
// success, the shorter usageErrTTL for a failure.
func (c *usageCache) fresh(e usageEntry, now time.Time) bool {
	ttl := UsageTTL
	if e.src.Error != "" {
		ttl = usageErrTTL
	}
	return now.Sub(e.at) < ttl
}

// usageTarget is a source the daemon wants usage for this round: a stable cache
// key, its display label, and a fetch closure that produces the source's result
// (used only on a cache miss).
type usageTarget struct {
	key   string
	label string
	fetch func(ctx context.Context) UsageSource
}

// Usage collects usage for every login the daemon can see: the broker login (if
// one is set), plus each ready session's own per-container login. Results are
// memoized per source for UsageTTL, so frequent polling stays cheap, and a
// source's fetch failure is reported inline (as Error) rather than failing the
// whole report. selfID scopes the report: "" (the trusted host API) reports the
// broker login and every ready session; a non-empty container id (the
// in-container relay) reports only that container's own login, so a container
// can never see another project's usage.
func (d *Daemon) Usage(ctx context.Context, selfID string) *UsageReport {
	return d.usage.collect(ctx, d.usageTargets(ctx, selfID))
}

// collect memoizes each target's result per source key, fetching only the
// stale/missing ones and reusing everything still within TTL. Fetches run
// without the lock held so a slow endpoint never blocks a concurrent reader.
func (c *usageCache) collect(ctx context.Context, targets []usageTarget) *UsageReport {
	now := c.now()

	// Snapshot which targets are already fresh and which must be fetched, under
	// the lock; do the (network) fetches without holding it so a slow endpoint
	// never blocks a concurrent caller reading fresh entries.
	c.mu.Lock()
	// Drop cache entries for sources that are gone (ended sessions), so the map
	// does not grow without bound.
	live := make(map[string]bool, len(targets))
	for _, t := range targets {
		live[t.key] = true
	}
	for k := range c.entries {
		if !live[k] {
			delete(c.entries, k)
		}
	}
	var stale []usageTarget
	for _, t := range targets {
		if e, ok := c.entries[t.key]; !ok || !c.fresh(e, now) {
			stale = append(stale, t)
		}
	}
	c.mu.Unlock()

	for _, t := range stale {
		src := t.fetch(ctx)
		c.mu.Lock()
		c.entries[t.key] = usageEntry{src: src, at: c.now()}
		c.mu.Unlock()
	}

	report := &UsageReport{Sources: make([]UsageSource, 0, len(targets))}
	c.mu.Lock()
	for _, t := range targets {
		if e, ok := c.entries[t.key]; ok {
			report.Sources = append(report.Sources, e.src)
		}
	}
	c.mu.Unlock()
	return report
}

// usageTargets builds the list of sources to report, ordered broker-first then
// sessions. The broker login (when set) is queried with a freshly refreshed
// access token; each session is queried with the live token read from its own
// container. Broker sessions are skipped in the session loop — their container
// holds only a proxy placeholder, so the broker source covers them.
//
// Sessions are deduped by Claude ACCOUNT, not by container: usage is an
// account-wide figure, so many devcontainers logged into one account would all
// return the same numbers. Reading each session's account UUID (from its
// .claude.json) lets the daemon query the endpoint ONCE per distinct account
// and report one line for it — rather than one request per container. Sessions
// whose account cannot be read fall back to being their own source, so nothing
// is silently dropped.
//
// selfID confines the report (see Usage): when non-empty, only that container's
// own login is reported — the broker source only if that container itself
// authenticates through the broker, and no other session at all.
func (d *Daemon) usageTargets(ctx context.Context, selfID string) []usageTarget {
	d.mu.Lock()
	entries := make([]*entry, 0, len(d.entries))
	for _, e := range d.entries {
		entries = append(entries, e)
	}
	d.mu.Unlock()

	var targets []usageTarget

	// Report the broker login only when a session in scope actually
	// authenticates through it — a stored-but-unused broker login is noise (its
	// numbers just duplicate a per-account line). broker_session already implies
	// the broker has credentials, so this covers both "no login" and "login but
	// unused". Under a container scope only that container's own use counts, so a
	// session never learns the shared login's usage unless it uses it itself.
	includeBroker := false
	for _, e := range entries {
		if selfID != "" && e.id != selfID {
			continue
		}
		if d.broker_session(e) {
			includeBroker = true
			break
		}
	}
	if includeBroker {
		version := usage.FallbackVersion
		for _, e := range entries {
			if v := e.snapshot().Version; v != "" {
				version = v
				break
			}
		}
		targets = append(targets, usageTarget{
			key:   "broker",
			label: "broker",
			fetch: func(ctx context.Context) UsageSource {
				return d.fetchBrokerUsage(ctx, version)
			},
		})
	}

	// Gather candidate sessions (scope + ready + non-broker), ordered by session
	// name so the representative picked per account is deterministic.
	type cand struct {
		e           *entry
		sessionName string
		version     string
	}
	var cands []cand
	for _, e := range entries {
		if selfID != "" && e.id != selfID {
			continue
		}
		it := e.snapshot()
		if it.Status != StatusReady || d.broker_session(e) {
			continue
		}
		name := it.Display
		if name == "" {
			name = it.Name
		}
		cands = append(cands, cand{e: e, sessionName: name, version: it.Version})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].sessionName < cands[j].sessionName })

	// One target per distinct account: the first session (by name) of each
	// account is its representative and the source that gets queried.
	seen := map[string]bool{}
	for _, cd := range cands {
		uuid, acctLabel := d.sessionIdentity(ctx, cd.e)
		key := "ctr:" + cd.e.id // fallback: account unknown, treat as its own source
		label := cd.sessionName
		if uuid != "" {
			key = "acct:" + uuid
			if acctLabel != "" {
				label = acctLabel
			}
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		e, version, lbl := cd.e, cd.version, label
		targets = append(targets, usageTarget{
			key:   key,
			label: lbl,
			fetch: func(ctx context.Context) UsageSource {
				return d.fetchSessionUsage(ctx, e, lbl, version)
			},
		})
	}
	return targets
}

// sessionIdentity reads a session's Claude account identity from its live
// .claude.json (the oauthAccount object): the stable account UUID used to dedupe
// sessions sharing one account, and a human label (display name, else email).
// Empty strings when the file is missing or unreadable, so the caller falls back
// to treating the session as its own source rather than dropping it.
func (d *Daemon) sessionIdentity(ctx context.Context, e *entry) (uuid, label string) {
	if d.cli == nil || e.cfg_dir == "" {
		return "", ""
	}
	raw, ok, err := dockerx.ReadFile(ctx, d.cli, e.id, path.Join(e.cfg_dir, ".claude.json"))
	if err != nil || !ok {
		return "", ""
	}
	return parseAccountIdentity(raw)
}

// parseAccountIdentity pulls the account UUID and a human label out of a
// .claude.json body (the oauthAccount object). Unparseable input yields empty
// strings. Split out from sessionIdentity so the field mapping is unit-testable
// without a container.
func parseAccountIdentity(raw []byte) (uuid, label string) {
	var doc struct {
		OAuthAccount struct {
			AccountUUID  string `json:"accountUuid"`
			DisplayName  string `json:"displayName"`
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", ""
	}
	label = doc.OAuthAccount.DisplayName
	if label == "" {
		label = doc.OAuthAccount.EmailAddress
	}
	return doc.OAuthAccount.AccountUUID, label
}

// fetchBrokerUsage queries usage for the shared broker login. Broker.Token
// returns a currently-valid access token (refreshing if needed), so this never
// fails on a stale token.
func (d *Daemon) fetchBrokerUsage(ctx context.Context, version string) UsageSource {
	src := UsageSource{Label: "broker"}
	tok, err := d.broker.Token(ctx)
	if err != nil {
		src.Error = err.Error()
		return src
	}
	u, err := usage.Fetch(ctx, d.usage_hc, tok, version)
	if err != nil {
		src.Error = err.Error()
		return src
	}
	src.Usage = u
	return src
}

// fetchSessionUsage reads a running session's live OAuth token from its own
// container and queries usage with it. The live file (not the host-side backup)
// is used so the token is the one claude is currently refreshing in place, which
// avoids a stale-token 401.
func (d *Daemon) fetchSessionUsage(ctx context.Context, e *entry, label, version string) UsageSource {
	src := UsageSource{Label: label}
	tok, err := d.sessionToken(ctx, e)
	if err != nil {
		src.Error = err.Error()
		return src
	}
	if tok == "" {
		src.Error = "no login in container"
		return src
	}
	u, err := usage.Fetch(ctx, d.usage_hc, tok, version)
	if err != nil {
		src.Error = err.Error()
		return src
	}
	src.Usage = u
	return src
}

// sessionToken reads the claudeAiOauth.accessToken from a container's live
// .credentials.json. A missing file yields an empty token (the session has not
// logged in yet), not an error.
func (d *Daemon) sessionToken(ctx context.Context, e *entry) (string, error) {
	if e.cfg_dir == "" {
		return "", nil
	}
	raw, ok, err := dockerx.ReadFile(ctx, d.cli, e.id, path.Join(e.cfg_dir, ".credentials.json"))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	var doc struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	return doc.ClaudeAiOauth.AccessToken, nil
}
