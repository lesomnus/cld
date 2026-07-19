package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/cld/internal/broker"
	"github.com/lesomnus/cld/internal/usage"
	"github.com/stretchr/testify/require"
)

// TestUsageTargetsSelfScope pins the security property of the relay's usage
// route: a container-scoped report contains only that container's own login,
// never another project's. It asserts on the selected targets (not their
// fetches), so no docker is needed.
func TestUsageTargetsSelfScope(t *testing.T) {
	// A broker with no stored credentials, so HasCredentials is false and no
	// broker target is ever added.
	d := &Daemon{
		entries: map[string]*entry{},
		broker:  broker.New(broker.FileStore{Path: filepath.Join(t.TempDir(), "none.json")}),
	}
	mk := func(id, name string) {
		// arch_ok=false makes broker_session short-circuit to false without
		// touching d.cfg/d.proxy, so these are plain per-container sessions.
		e := &entry{id: id, cfg_dir: "/cfg"}
		e.item = Item{ID: id, Name: name, Display: name, Status: StatusReady, Version: "1.0.0"}
		e.publish()
		d.entries[id] = e
	}
	mk("idA", "alpha")
	mk("idB", "bravo")

	keys := func(ts []usageTarget) map[string]bool {
		m := map[string]bool{}
		for _, t := range ts {
			m[t.key] = true
		}
		return m
	}

	// With no docker client the account is unknown, so each session falls back
	// to its own "ctr:<id>" source. Host scope ("") sees every ready session.
	require.Equal(t, map[string]bool{"ctr:idA": true, "ctr:idB": true}, keys(d.usageTargets(context.Background(), "")))

	// Container scope sees only its own session.
	require.Equal(t, map[string]bool{"ctr:idA": true}, keys(d.usageTargets(context.Background(), "idA")))
}

// TestUsageTargetsBrokerUnused pins option 1: a stored broker login is NOT
// reported unless a session actually authenticates through it. Here credentials
// exist but no session is a broker session (arch_ok=false short-circuits
// broker_session), so no "broker" source appears.
func TestUsageTargetsBrokerUnused(t *testing.T) {
	brokerPath := filepath.Join(t.TempDir(), "broker.json")
	require.NoError(t, os.WriteFile(brokerPath, []byte(`{"refresh_token":"r","access_token":"a"}`), 0o600))

	d := &Daemon{
		entries: map[string]*entry{},
		broker:  broker.New(broker.FileStore{Path: brokerPath}),
	}
	require.True(t, d.broker.HasCredentials(), "the broker login is stored")

	e := &entry{id: "idA", cfg_dir: "/cfg"}
	e.item = Item{ID: "idA", Name: "alpha", Display: "alpha", Status: StatusReady}
	e.publish()
	d.entries["idA"] = e

	for _, tgt := range d.usageTargets(context.Background(), "") {
		require.NotEqual(t, "broker", tgt.key, "an unused broker login must not be reported")
	}
}

// TestAccountUsageFallthrough pins the token-fallthrough policy: an account's
// usage is answered by the first of its containers that can, so one container
// with a bad or missing token never blanks the line when a sibling could answer.
func TestAccountUsageFallthrough(t *testing.T) {
	e := func(id string) *entry { return &entry{id: id} }
	ok := &usage.Usage{}

	t.Run("a sibling answers after the representative's token fails", func(t *testing.T) {
		var tried []string
		src := accountUsage("acct", []*entry{e("a"), e("b")}, func(en *entry) (*usage.Usage, error) {
			tried = append(tried, en.id)
			if en.id == "a" {
				return nil, &usage.StatusError{Code: 401}
			}
			return ok, nil
		})
		require.Empty(t, src.Error)
		require.Same(t, ok, src.Usage)
		require.Equal(t, []string{"a", "b"}, tried, "must fall through to the sibling after 'a' fails")
	})

	t.Run("stops at the first success without trying later siblings", func(t *testing.T) {
		var n int
		src := accountUsage("acct", []*entry{e("a"), e("b")}, func(en *entry) (*usage.Usage, error) {
			n++
			return ok, nil
		})
		require.Same(t, ok, src.Usage)
		require.Equal(t, 1, n, "a success must short-circuit the remaining candidates")
	})

	t.Run("reports the real error over a bare no-login", func(t *testing.T) {
		src := accountUsage("acct", []*entry{e("a"), e("b")}, func(en *entry) (*usage.Usage, error) {
			if en.id == "a" {
				return nil, errNoLogin
			}
			return nil, &usage.StatusError{Code: 429}
		})
		require.Nil(t, src.Usage)
		require.Contains(t, src.Error, "Too Many Requests", "the actionable endpoint error must win over 'no login'")
	})

	t.Run("all not-logged-in surfaces the no-login reason", func(t *testing.T) {
		src := accountUsage("acct", []*entry{e("a"), e("b")}, func(en *entry) (*usage.Usage, error) {
			return nil, errNoLogin
		})
		require.Nil(t, src.Usage)
		require.Equal(t, errNoLogin.Error(), src.Error)
	})
}

func TestParseAccountIdentity(t *testing.T) {
	// The shape of a real .claude.json oauthAccount, trimmed to the fields used.
	uuid, label := parseAccountIdentity([]byte(`{
		"oauthAccount": {"accountUuid": "8afc692f-1", "displayName": "Somnus", "emailAddress": "me@example.com"},
		"projects": {"/workspace": {"history": []}}
	}`))
	require.Equal(t, "8afc692f-1", uuid)
	require.Equal(t, "Somnus", label, "displayName wins as the label")

	// Falls back to email when there is no display name.
	_, label = parseAccountIdentity([]byte(`{"oauthAccount":{"accountUuid":"u","emailAddress":"me@example.com"}}`))
	require.Equal(t, "me@example.com", label)

	// No oauthAccount / junk yields empties, so the caller falls back to a
	// per-container source rather than dropping the session.
	uuid, label = parseAccountIdentity([]byte(`{"projects":{}}`))
	require.Empty(t, uuid)
	require.Empty(t, label)
	uuid, _ = parseAccountIdentity([]byte(`not json`))
	require.Empty(t, uuid)
}

// target builds a usageTarget whose fetch increments calls and returns a fixed
// source, so a test can assert exactly when a real fetch happened.
func target(key, label string, calls *int32, src UsageSource) usageTarget {
	return usageTarget{key: key, label: label, fetch: func(context.Context) UsageSource {
		atomic.AddInt32(calls, 1)
		return src
	}}
}

func TestUsageCacheMemoizes(t *testing.T) {
	now := time.Unix(0, 0)
	c := &usageCache{entries: map[string]usageEntry{}, now: func() time.Time { return now }}

	var calls int32
	ok := UsageSource{Label: "broker", Usage: &usage.Usage{}}
	tgts := []usageTarget{target("broker", "broker", &calls, ok)}

	// First call fetches; a second call within the TTL is served from cache.
	rep := c.collect(context.Background(), tgts)
	require.Len(t, rep.Sources, 1)
	require.Equal(t, int32(1), calls)

	now = now.Add(UsageTTL - time.Second)
	c.collect(context.Background(), tgts)
	require.Equal(t, int32(1), calls, "a hit within TTL must not re-fetch")

	// Past the TTL it fetches again.
	now = now.Add(2 * time.Second)
	c.collect(context.Background(), tgts)
	require.Equal(t, int32(2), calls, "an entry past TTL must re-fetch")
}

func TestUsageCacheErrorShortTTL(t *testing.T) {
	now := time.Unix(0, 0)
	c := &usageCache{entries: map[string]usageEntry{}, now: func() time.Time { return now }}

	var calls int32
	fail := UsageSource{Label: "broker", Error: "boom"}
	tgts := []usageTarget{target("broker", "broker", &calls, fail)}

	c.collect(context.Background(), tgts)
	require.Equal(t, int32(1), calls)

	// A failure is cached only for the shorter usageErrTTL: still cached before,
	// re-fetched after.
	now = now.Add(usageErrTTL - time.Second)
	c.collect(context.Background(), tgts)
	require.Equal(t, int32(1), calls)

	now = now.Add(2 * time.Second)
	c.collect(context.Background(), tgts)
	require.Equal(t, int32(2), calls, "a cached error must expire at usageErrTTL, well before UsageTTL")
}

// TestUsageCacheDropsCanceled pins that a fetch failing because the CALLER went
// away (a canceled/timed-out context) is not cached: otherwise a transient
// client timeout on a slow endpoint would pin "context canceled" on the source
// for the whole usageErrTTL, so every fast follow-up call parrots it back.
func TestUsageCacheDropsCanceled(t *testing.T) {
	now := time.Unix(0, 0)
	c := &usageCache{entries: map[string]usageEntry{}, now: func() time.Time { return now }}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already given up

	var calls int32
	fail := UsageSource{Label: "acct", Error: `query usage: Get "…": context canceled`}
	rep := c.collect(ctx, []usageTarget{target("acct", "acct", &calls, fail)})

	require.Equal(t, int32(1), calls)
	_, ok := c.entries["acct"]
	require.False(t, ok, "a caller-canceled fetch must not be cached")
	require.Empty(t, rep.Sources, "with nothing cached the poisoned source is omitted, not served")
}

// TestUsageCacheCanceledKeepsPrior pins the other half: a canceled refresh must
// not overwrite a prior good value — the stale-but-good number is served instead
// of the cancellation artifact.
func TestUsageCacheCanceledKeepsPrior(t *testing.T) {
	now := time.Unix(0, 0)
	c := &usageCache{entries: map[string]usageEntry{}, now: func() time.Time { return now }}

	var calls int32
	good := UsageSource{Label: "acct", Usage: &usage.Usage{}}
	c.collect(context.Background(), []usageTarget{target("acct", "acct", &calls, good)})
	require.Equal(t, int32(1), calls)

	// Past the TTL the entry is stale, so a refresh is attempted — but under an
	// already-canceled context.
	now = now.Add(UsageTTL + time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fail := UsageSource{Label: "acct", Error: "context canceled"}
	rep := c.collect(ctx, []usageTarget{target("acct", "acct", &calls, fail)})

	require.Len(t, rep.Sources, 1)
	require.NotNil(t, rep.Sources[0].Usage, "the prior good value must still be served")
	require.Empty(t, rep.Sources[0].Error)
	require.NotNil(t, c.entries["acct"].src.Usage, "a canceled refresh must not overwrite the good entry")
}

func TestUsageCacheEvictsDeparted(t *testing.T) {
	now := time.Unix(0, 0)
	c := &usageCache{entries: map[string]usageEntry{}, now: func() time.Time { return now }}

	var a, b int32
	srcA := UsageSource{Label: "a", Usage: &usage.Usage{}}
	srcB := UsageSource{Label: "b", Usage: &usage.Usage{}}

	c.collect(context.Background(), []usageTarget{
		target("a", "a", &a, srcA),
		target("b", "b", &b, srcB),
	})
	require.Len(t, c.entries, 2)

	// "b" is gone this round; its cache entry must be evicted so the map does
	// not grow with dead sessions, and only "a" is reported.
	rep := c.collect(context.Background(), []usageTarget{target("a", "a", &a, srcA)})
	require.Len(t, rep.Sources, 1)
	require.Equal(t, "a", rep.Sources[0].Label)
	require.Len(t, c.entries, 1)
	_, ok := c.entries["b"]
	require.False(t, ok, "a departed source must be evicted from the cache")
}

func TestUsageCacheReportOrder(t *testing.T) {
	now := time.Unix(0, 0)
	c := &usageCache{entries: map[string]usageEntry{}, now: func() time.Time { return now }}

	var n int32
	// Report order must follow the target order, not cache-map iteration.
	tgts := []usageTarget{
		target("broker", "broker", &n, UsageSource{Label: "broker", Usage: &usage.Usage{}}),
		target("s1", "alpha", &n, UsageSource{Label: "alpha", Usage: &usage.Usage{}}),
		target("s2", "beta", &n, UsageSource{Label: "beta", Usage: &usage.Usage{}}),
	}
	rep := c.collect(context.Background(), tgts)
	require.Equal(t, []string{"broker", "alpha", "beta"},
		[]string{rep.Sources[0].Label, rep.Sources[1].Label, rep.Sources[2].Label})
}
