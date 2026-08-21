package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func quiet_log() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// load reads a config file the way the daemon does at startup.
func load(t *testing.T, path string) *config.Config {
	t.Helper()
	c, err := config.ReadFromFile(path)
	require.NoError(t, err)
	require.NoError(t, c.Evaluate())
	return c
}

func write(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	// The stamp is mtime+size, so a same-size rewrite within the clock's
	// resolution still has to be noticed: push the mtime forward.
	touch(t, path)
}

// touch pushes the file's mtime forward so an edit is unambiguous even when two
// writes land in the same instant.
func touch(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(path, fi.ModTime().Add(time.Second), fi.ModTime().Add(time.Second)))
}

// The daemon re-reads its config while it runs, so editing cld.yaml applies to
// the next provisioning instead of needing a restart — which is exactly the
// trap this came from: the engine override was already live, so one file in the
// config directory applied at once and the other did not.
func TestPolicyReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cld.yaml")
	write(t, path, "env:\n  A: \"1\"\n")

	s := new_policy_store(load(t, path), quiet_log())
	require.Equal(t, "1", *s.get().Env["A"])

	t.Run("an edit is picked up", func(t *testing.T) {
		write(t, path, "env:\n  A: \"2\"\n")
		s.refresh()
		require.Equal(t, "2", *s.get().Env["A"])
	})

	t.Run("an unchanged file is not re-read", func(t *testing.T) {
		// Mutating the loaded config is how the tests elsewhere drive the
		// daemon; a refresh that reloaded every time would undo them.
		s.get().Env["A"] = strp("in-memory")
		s.refresh()
		require.Equal(t, "in-memory", *s.get().Env["A"])
	})

	t.Run("a broken file keeps the last good one", func(t *testing.T) {
		// A typo must not wipe out the settings every container is using.
		write(t, path, "env:\n  1BAD: x\n")
		s.refresh()
		require.Equal(t, "in-memory", *s.get().Env["A"])

		t.Run("and is picked up once fixed", func(t *testing.T) {
			write(t, path, "env:\n  A: \"3\"\n")
			s.refresh()
			require.Equal(t, "3", *s.get().Env["A"])
		})
	})

	t.Run("a removed file falls back to defaults", func(t *testing.T) {
		require.NoError(t, os.Remove(path))
		s.refresh()
		require.Empty(t, s.get().Env, "deleting the config must not leave its settings behind")
	})
}

// A daemon that started with no config file watches where one would appear, so
// creating it later needs no restart either.
func TestPolicyAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cld.yaml")

	base := &config.Config{}
	require.NoError(t, base.Evaluate())
	s := new_policy_store(base, quiet_log())

	s.refresh()
	require.Empty(t, s.get().Env, "nothing is watched until told what to watch")

	s.watch([]string{path})
	write(t, path, "env:\n  A: \"1\"\n")
	s.refresh()
	require.Equal(t, "1", *s.get().Env["A"])
}

// Everything else — directories, auth, telemetry, sync — keeps what it started
// with: those bind sockets, relays and watchers for the daemon's lifetime.
func TestPolicyLeavesTheDaemonsOwnConfigAlone(t *testing.T) {
	d, cfg := newTestDaemon(t)
	require.Same(t, cfg, d.cfg)
	require.Same(t, cfg, d.policy(), "with nothing to watch, the policy is the loaded config")
}
