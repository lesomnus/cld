package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/stretchr/testify/require"
)

// provision_entry is an entry filled in as resolve() would leave it for a
// running container, so the placement and script steps can be driven directly.
func provision_entry(id string) *entry {
	e := &entry{
		id:         id,
		user:       "root",
		home:       "/root",
		cache_home: "/root/.cache",
		cfg_dir:    "/root/.claude",
		started_at: "gen-1",
	}
	e.item = Item{ID: id, Name: "alpha", LocalFolder: "/work/api", Workspace: "/workspace"}
	return e
}

func in_container(t *testing.T, d *Daemon, id string, script string) (string, int) {
	t.Helper()
	out, code, err := dockerx.ExecOutput(t.Context(), d.cli, id, "root", []string{"sh", "-c", script})
	require.NoError(t, err)
	return strings.TrimSpace(out), code
}

// TestInstallFiles drives the placement against a real container: the parts
// that cannot be unit-tested are the copy itself, its ownership and mode, and
// the marker that decides whether to copy again.
func TestInstallFiles(t *testing.T) {
	cli := require_docker(t)
	id := run_container_labeled(t, cli, "", map[string]string{})

	// The daemon reads sources through its host-home mount; point it at a fake.
	host_home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(host_home, "token"), []byte("secret-v1"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(host_home, "certs", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(host_home, "certs", "ca.pem"), []byte("ca"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(host_home, "certs", "sub", "key.pem"), []byte("key"), 0o644))

	cfg := &config.Config{CacheDir: t.TempDir(), DataDir: t.TempDir(), HostHome: host_home, Docker: dockerOff}
	cfg.Files = []config.FileSpec{
		{Src: "~/token", Dst: "${HOME}/.config/svc/token"},
		{Src: "~/certs", Dst: "${HOME}/.docker-remote", Mode: "0640"},
	}
	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)

	e := provision_entry(id)
	d.install_files(t.Context(), e, id)

	t.Run("places a file, creating its parents", func(t *testing.T) {
		out, code := in_container(t, d, id, "cat /root/.config/svc/token")
		require.Equal(t, 0, code)
		require.Equal(t, "secret-v1", out)
	})

	t.Run("defaults to a private mode", func(t *testing.T) {
		// Credentials are the motivating case, so the source's 0644 must not
		// carry over.
		out, _ := in_container(t, d, id, "stat -c %a /root/.config/svc/token")
		require.Equal(t, "600", out)
	})

	t.Run("places a tree with the configured mode", func(t *testing.T) {
		out, code := in_container(t, d, id, "cat /root/.docker-remote/sub/key.pem")
		require.Equal(t, 0, code)
		require.Equal(t, "key", out)

		mode, _ := in_container(t, d, id, "stat -c %a /root/.docker-remote/sub/key.pem")
		require.Equal(t, "640", mode)

		// A directory needs execute wherever it grants read, or the file above
		// would be unreachable.
		dir, _ := in_container(t, d, id, "stat -c %a /root/.docker-remote/sub")
		require.Equal(t, "750", dir)
	})

	t.Run("an unchanged source is not copied again", func(t *testing.T) {
		in_container(t, d, id, "echo tampered > /root/.config/svc/token")
		d.install_files(t.Context(), e, id)

		out, _ := in_container(t, d, id, "cat /root/.config/svc/token")
		require.Equal(t, "tampered", out, "the hash matched, so nothing should have been written")
	})

	t.Run("a rotated source is copied again", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(host_home, "token"), []byte("secret-v2"), 0o644))
		d.install_files(t.Context(), e, id)

		out, _ := in_container(t, d, id, "cat /root/.config/svc/token")
		require.Equal(t, "secret-v2", out)
	})

	t.Run("a file removed from a tree does not survive", func(t *testing.T) {
		require.NoError(t, os.Remove(filepath.Join(host_home, "certs", "ca.pem")))
		d.install_files(t.Context(), e, id)

		_, code := in_container(t, d, id, "test -e /root/.docker-remote/ca.pem")
		require.NotEqual(t, 0, code, "a tree is replaced, not overlaid")
	})

	t.Run("a missing source is logged, not fatal", func(t *testing.T) {
		cfg.Files = append(cfg.Files, config.FileSpec{Src: "~/nope", Dst: "/tmp/nope"})
		d.install_files(t.Context(), e, id) // must not panic or block
		_, code := in_container(t, d, id, "test -e /tmp/nope")
		require.NotEqual(t, 0, code)
	})
}

// TestRunScripts drives the script steps against a real container: what the
// script sees, and when it runs a second time.
func TestRunScripts(t *testing.T) {
	cli := require_docker(t)
	id := run_container_labeled(t, cli, "", map[string]string{})

	cfg := &config.Config{CacheDir: t.TempDir(), DataDir: t.TempDir(), Docker: dockerOff}
	cfg.Env = config.EnvMap{"GREETING": strp("hello")}
	cfg.Scripts = config.ScriptSet{
		Setup: &config.ScriptSpec{Run: config.ScriptRun{
			Shell: `echo "$GREETING $CLD_EVENT $CLD_NAME $PWD" >> /tmp/setup.log`,
		}},
	}
	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)

	e := provision_entry(id)
	// The workspace is the default working directory; alpine has no /workspace.
	e.item.Workspace = "/tmp"

	require.NoError(t, d.run_scripts(t.Context(), e, id, scriptSetup))

	t.Run("runs with the session env and the CLD_ context", func(t *testing.T) {
		out, code := in_container(t, d, id, "cat /tmp/setup.log")
		require.Equal(t, 0, code)
		require.Equal(t, "hello setup alpha /tmp", out)
	})

	t.Run("does not run again for the same definition", func(t *testing.T) {
		require.NoError(t, d.run_scripts(t.Context(), e, id, scriptSetup))
		out, _ := in_container(t, d, id, "wc -l < /tmp/setup.log")
		require.Equal(t, "1", strings.TrimSpace(out))
	})

	t.Run("runs again once the definition changes", func(t *testing.T) {
		cfg.Scripts.Setup.Run.Shell = `echo "edited" >> /tmp/setup.log`
		require.NoError(t, d.run_scripts(t.Context(), e, id, scriptSetup))
		out, _ := in_container(t, d, id, "tail -1 /tmp/setup.log")
		require.Equal(t, "edited", out)
	})

	t.Run("start runs once per container generation", func(t *testing.T) {
		cfg.Scripts.Start = &config.ScriptSpec{Run: config.ScriptRun{Shell: `echo start >> /tmp/start.log`}}
		require.NoError(t, d.run_scripts(t.Context(), e, id, scriptStart))
		require.NoError(t, d.run_scripts(t.Context(), e, id, scriptStart))
		out, _ := in_container(t, d, id, "wc -l < /tmp/start.log")
		require.Equal(t, "1", strings.TrimSpace(out))

		// A restart is a new generation, so they run again.
		e.started_at = "gen-2"
		require.NoError(t, d.run_scripts(t.Context(), e, id, scriptStart))
		out, _ = in_container(t, d, id, "wc -l < /tmp/start.log")
		require.Equal(t, "2", strings.TrimSpace(out))
	})

	t.Run("a failure is warned about by default", func(t *testing.T) {
		cfg.Scripts.Setup.Run.Shell = "exit 3"
		require.NoError(t, d.run_scripts(t.Context(), e, id, scriptSetup))
	})

	t.Run("on_error fail reports the failure and is retried", func(t *testing.T) {
		cfg.Scripts.Setup.Run.Shell = "echo boom >&2; exit 3"
		cfg.Scripts.Setup.OnError = config.OnErrorFail

		err := d.run_scripts(t.Context(), e, id, scriptSetup)
		require.ErrorContains(t, err, "boom")

		// Not recorded as done: the user said this one has to succeed.
		require.Error(t, d.run_scripts(t.Context(), e, id, scriptSetup))
	})

	t.Run("an argv script runs with no shell", func(t *testing.T) {
		cfg.Scripts.Setup = &config.ScriptSpec{
			Run: config.ScriptRun{Argv: []string{"touch", "/tmp/argv-ran"}},
		}
		require.NoError(t, d.run_scripts(t.Context(), e, id, scriptSetup))
		_, code := in_container(t, d, id, "test -e /tmp/argv-ran")
		require.Equal(t, 0, code)
	})
}

// TestSessionEnvUnsetInContainer proves the one thing a docker exec cannot do:
// removing a variable the container passes down to it.
func TestSessionEnvUnsetInContainer(t *testing.T) {
	cli := require_docker(t)
	id := run_container_labeled(t, cli, "", map[string]string{})

	cfg := &config.Config{CacheDir: t.TempDir(), DataDir: t.TempDir(), Docker: dockerOff}
	cfg.Env = config.EnvMap{"REMOVED": nil, "KEPT": strp("yes")}
	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)

	e := provision_entry(id)
	e.container_env = []string{"REMOVED=from-image"}

	env := d.session_env(e)
	out, code, err := dockerx.Exec(t.Context(), d.cli, id, dockerx.ExecOptions{
		User: "root",
		Env:  env.Overrides(),
		Cmd:  with_unset(env.Unset(), []string{"sh", "-c", `echo "kept=$KEPT removed=${REMOVED-<absent>}"`}),
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Equal(t, "kept=yes removed=<absent>", strings.TrimSpace(out))
}
