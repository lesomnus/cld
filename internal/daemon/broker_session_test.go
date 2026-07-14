package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/broker"
	"github.com/stretchr/testify/require"
)

// newTestDaemon builds a daemon with no docker client; enough for the pure
// policy helpers (session_env, broker_session) that touch only cfg and broker.
func newTestDaemon(t *testing.T) (*Daemon, *config.Config) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{CacheDir: filepath.Join(tmp, "cache"), DataDir: filepath.Join(tmp, "data")}
	d, err := New(cfg, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)
	return d, cfg
}

func seedBrokerLogin(t *testing.T, cfg *config.Config) {
	t.Helper()
	err := broker.FileStore{Path: cfg.BrokerCredentialsPath()}.Save(&broker.Credentials{
		AccessToken:  "access",
		RefreshToken: "refresh",
	})
	require.NoError(t, err)
}

// enableProxy turns on the per-project proxy preference for e, so broker_session
// can activate (it is opt-in per project).
func enableProxy(t *testing.T, d *Daemon, e *entry) {
	t.Helper()
	require.NoError(t, d.proxy.set(d.backup_key(e), true))
}

func TestBrokerSessionGate(t *testing.T) {
	t.Run("inactive without a login", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		e := &entry{arch_ok: true}
		enableProxy(t, d, e)
		require.False(t, d.broker_session(e))
	})

	t.Run("inactive when the project has not opted in, even with a login", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		seedBrokerLogin(t, cfg)
		require.False(t, d.broker_session(&entry{arch_ok: true}))
	})

	t.Run("active with a login, matching arch, and project opt-in", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		seedBrokerLogin(t, cfg)
		e := &entry{arch_ok: true}
		enableProxy(t, d, e)
		require.True(t, d.broker_session(e))
	})

	t.Run("inactive on arch mismatch (no in-container proxy possible)", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		seedBrokerLogin(t, cfg)
		e := &entry{arch_ok: false}
		enableProxy(t, d, e)
		require.False(t, d.broker_session(e))
	})
}

func TestProxyStore(t *testing.T) {
	d, _ := newTestDaemon(t)
	e := &entry{}
	key := d.backup_key(e)

	require.False(t, d.proxy.get(key), "off by default")

	require.NoError(t, d.proxy.set(key, true))
	require.True(t, d.proxy.get(key))

	require.NoError(t, d.proxy.set(key, false))
	require.False(t, d.proxy.get(key))

	// Clearing an absent key is a no-op, not an error.
	require.NoError(t, d.proxy.set(key, false))
	d.proxy.clear(key)
	require.False(t, d.proxy.get(key))
}

func TestSessionEnvBrokerVars(t *testing.T) {
	e := &entry{arch_ok: true, cfg_dir: "/home/u/.cld/claude"}
	hasBaseURL := func(env []string) bool {
		return slices.ContainsFunc(env, func(s string) bool {
			return strings.HasPrefix(s, "ANTHROPIC_BASE_URL=")
		})
	}

	// Without a login: no broker vars.
	t.Run("absent without a login", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		enableProxy(t, d, e)
		require.False(t, hasBaseURL(d.session_env(e)))
	})

	// With a login but no project opt-in: still the default per-container login.
	t.Run("absent without project opt-in", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		require.NoError(t, d.broker.SetCredentials(&broker.Credentials{RefreshToken: "refresh"}))
		require.False(t, hasBaseURL(d.session_env(e)))
	})

	// With a login AND the project opted in: the proxy base URL, a placeholder
	// token, and the tool-search re-enable are all present. Seed via the broker
	// so its cache reflects the login exactly as `cld auth login`
	// (SetCredentials) does in production.
	t.Run("present with a login and project opt-in", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		require.NoError(t, d.broker.SetCredentials(&broker.Credentials{RefreshToken: "refresh"}))
		enableProxy(t, d, e)
		env := d.session_env(e)
		require.Contains(t, env, "ANTHROPIC_BASE_URL=http://"+proxyListenAddr)
		require.Contains(t, env, "ANTHROPIC_AUTH_TOKEN=cld-broker-placeholder")
		require.Contains(t, env, "ENABLE_TOOL_SEARCH=true")
	})
}
