package daemon

import (
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/broker"
	"github.com/stretchr/testify/require"
)

func strp(s string) *string { return &s }

// resolved maps the effective session environment to key -> value, dropping
// the variables the container already had, which are inherited rather than
// re-sent.
func resolved(d *Daemon, e *entry) map[string]string {
	return envMap(d.session_env(e).Overrides())
}

func TestSessionEnvUserConfig(t *testing.T) {
	t.Run("global env reaches the session", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"EDITOR": strp("vim")}
		require.Equal(t, "vim", resolved(d, &entry{})["EDITOR"])
	})

	t.Run("a project block applies to its workspace only", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		cfg.Projects = []config.ProjectConfig{{
			Match: config.StringList{"/work/acme/**"},
			Env:   config.EnvMap{"AWS_PROFILE": strp("acme")},
		}}

		e := &entry{item: Item{LocalFolder: "/work/acme/api"}}
		require.Equal(t, "acme", resolved(d, e)["AWS_PROFILE"])

		other := &entry{item: Item{LocalFolder: "/work/other"}}
		require.NotContains(t, resolved(d, other), "AWS_PROFILE")
	})

	t.Run("a project block overrides the global env", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"AWS_PROFILE": strp("default")}
		cfg.Projects = []config.ProjectConfig{{
			Match: config.StringList{"/work/**"},
			Env:   config.EnvMap{"AWS_PROFILE": strp("acme")},
		}}
		e := &entry{item: Item{LocalFolder: "/work/api"}}
		require.Equal(t, "acme", resolved(d, e)["AWS_PROFILE"])
	})

	t.Run("user env beats the devcontainer and cld's own defaults", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"FOO": strp("user"), "TERM": strp("screen-256color")}
		e := &entry{remote_env: map[string]string{"FOO": "remote"}}

		env := resolved(d, e)
		require.Equal(t, "user", env["FOO"])
		require.Equal(t, "screen-256color", env["TERM"])
	})

	t.Run("cld's managed keys are last", func(t *testing.T) {
		// config rejects these, so this only pins the layer order itself.
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"CLAUDE_CONFIG_DIR": strp("/hijacked")}
		e := &entry{cfg_dir: "/home/u/.claude"}
		require.Equal(t, "/home/u/.claude", resolved(d, e)["CLAUDE_CONFIG_DIR"])
	})
}

func TestSessionEnvExpansion(t *testing.T) {
	t.Run("extends a value the container already had", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"PATH": strp("${PATH}:/opt/bin")}
		e := &entry{container_env: []string{"PATH=/usr/bin"}}
		require.Equal(t, "/usr/bin:/opt/bin", resolved(d, e)["PATH"])
	})

	t.Run("defers to a value the container already had", func(t *testing.T) {
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"GOFLAGS": strp("${GOFLAGS:--mod=mod}")}

		e := &entry{container_env: []string{"GOFLAGS=-race"}}
		require.Equal(t, "-race", resolved(d, e)["GOFLAGS"])

		bare := &entry{}
		require.Equal(t, "-mod=mod", resolved(d, bare)["GOFLAGS"])
	})

	t.Run("reads the daemon's own environment", func(t *testing.T) {
		// How a secret reaches a session without being written into cld.yaml.
		t.Setenv("CLD_TEST_SECRET", "s3cret")
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"TOKEN": strp("${env:CLD_TEST_SECRET}")}
		require.Equal(t, "s3cret", resolved(d, &entry{})["TOKEN"])
	})

	t.Run("localEnv resolves to nothing", func(t *testing.T) {
		// The daemon runs in a container; it cannot see the host user's env.
		t.Setenv("CLD_TEST_SECRET", "s3cret")
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"TOKEN": strp("${localEnv:CLD_TEST_SECRET}")}
		require.Equal(t, "", resolved(d, &entry{})["TOKEN"])
	})
}

func TestSessionEnvUnset(t *testing.T) {
	d, cfg := newTestDaemon(t)
	cfg.Env = config.EnvMap{"LESS": nil}
	e := &entry{container_env: []string{"LESS=-R"}}

	env := d.session_env(e)
	require.Equal(t, []string{"LESS"}, env.Unset())
	require.NotContains(t, envMap(env.Overrides()), "LESS")

	t.Run("the command drops it, since an exec cannot", func(t *testing.T) {
		// A docker exec adds and replaces variables but never removes one the
		// container already had, so it has to happen in the command itself.
		argv := with_unset(env.Unset(), []string{"claude"})
		require.Equal(t, []string{"sh", "-c", `unset LESS; exec "$@"`, "sh", "claude"}, argv)
	})

	t.Run("no removals leaves the command alone", func(t *testing.T) {
		require.Equal(t, []string{"claude"}, with_unset(nil, []string{"claude"}))
	})
}

func TestSessionEnvOrigins(t *testing.T) {
	d, cfg := newTestDaemon(t)
	cfg.Env = config.EnvMap{"FROM_CONFIG": strp("1")}
	cfg.Projects = []config.ProjectConfig{{
		Match: config.StringList{"/work/**"},
		Env:   config.EnvMap{"FROM_PROJECT": strp("1")},
	}}
	e := &entry{
		item:          Item{LocalFolder: "/work/api"},
		cfg_dir:       "/home/u/.claude",
		container_env: []string{"FROM_IMAGE=1"},
		remote_env:    map[string]string{"FROM_REMOTE": "1"},
	}

	origins := map[string]string{}
	for _, v := range d.session_env(e).Vars {
		origins[v.Key] = v.Origin
	}

	// Every value must be traceable to the place a user would go to change it.
	require.Equal(t, "container", origins["FROM_IMAGE"])
	require.Equal(t, envOriginRemote, origins["FROM_REMOTE"])
	require.Equal(t, envOriginDefault, origins["LANG"])
	require.Equal(t, envOriginConfig, origins["FROM_CONFIG"])
	require.Equal(t, "cld.yaml projects[/work/**]", origins["FROM_PROJECT"])
	require.Equal(t, envOriginManaged, origins["CLAUDE_CONFIG_DIR"])
}

// Everything the managed layer sets silently wins over user config, so every
// one of those keys must be rejected at config load — otherwise a user sets
// one, watches it get ignored, and has nothing to explain why.
func TestSessionEnvManagedKeysAreReserved(t *testing.T) {
	d, _ := newTestDaemon(t)
	require.NoError(t, d.broker.SetCredentials(&broker.Credentials{RefreshToken: "refresh"}))

	// Everything that gates a managed variable, turned on at once.
	e := &entry{arch_ok: true, git_config: true, cfg_dir: "/home/u/.claude"}
	enableProxy(t, d, e)
	require.True(t, d.broker_session(e))
	require.True(t, d.telemetry_session(e))

	managed := d.env_managed(e)
	require.Greater(t, len(managed), 10, "the gates above should all be open")
	for k := range managed {
		require.True(t, config.EnvKeyReserved(k), "%s is set by cld but not reserved in config", k)
	}
}
