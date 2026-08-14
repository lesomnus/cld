package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// parse decodes a cld.yaml body and evaluates it, which is where the session
// policy is validated and defaulted.
func parse(t *testing.T, body string) (*config.Config, error) {
	t.Helper()
	var c config.Config
	require.NoError(t, yaml.Unmarshal([]byte(body), &c))
	if err := c.Evaluate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func TestEnvParse(t *testing.T) {
	c, err := parse(t, `
env:
  FOO: bar
  EMPTY: ""
  GONE: null
`)
	require.NoError(t, err)

	require.Equal(t, "bar", *c.Env["FOO"])
	require.Equal(t, "", *c.Env["EMPTY"])

	// The distinction the whole "remove a variable" feature rests on.
	v, ok := c.Env["GONE"]
	require.True(t, ok)
	require.Nil(t, v)
}

func TestEnvValidation(t *testing.T) {
	t.Run("rejects an invalid name", func(t *testing.T) {
		_, err := parse(t, "env:\n  \"1BAD\": x\n")
		require.ErrorContains(t, err, "not a valid environment variable name")
	})

	t.Run("rejects a key cld manages", func(t *testing.T) {
		_, err := parse(t, "env:\n  CLAUDE_CONFIG_DIR: /elsewhere\n")
		require.ErrorContains(t, err, "set by cld")
	})

	t.Run("rejects a reserved prefix", func(t *testing.T) {
		_, err := parse(t, "env:\n  OTEL_METRICS_EXPORTER: none\n")
		require.ErrorContains(t, err, "set by cld")
	})

	t.Run("allows a soft default", func(t *testing.T) {
		// TERM/LANG are cld's defaults, not cld's promises: overridable.
		_, err := parse(t, "env:\n  TERM: screen-256color\n")
		require.NoError(t, err)
	})

	t.Run("validates project blocks too", func(t *testing.T) {
		_, err := parse(t, "projects:\n  - match: ~/w/**\n    env:\n      SSH_AUTH_SOCK: /tmp/x\n")
		require.ErrorContains(t, err, "projects[0].env")
	})
}

func TestScriptParse(t *testing.T) {
	t.Run("shorthand string", func(t *testing.T) {
		c, err := parse(t, "scripts:\n  setup: echo hi\n")
		require.NoError(t, err)
		require.Equal(t, []string{"sh", "-c", "echo hi"}, c.Scripts.Setup.Run.Cmd())
		require.Equal(t, config.ScriptTimeoutDefault, c.Scripts.Setup.TimeoutOrDefault())
		require.False(t, c.Scripts.Setup.Fatal())
	})

	t.Run("argv list runs with no shell", func(t *testing.T) {
		c, err := parse(t, "scripts:\n  setup: [make, dev-setup]\n")
		require.NoError(t, err)
		require.Equal(t, []string{"make", "dev-setup"}, c.Scripts.Setup.Run.Cmd())
	})

	t.Run("mapping form", func(t *testing.T) {
		c, err := parse(t, `
scripts:
  start:
    run: id
    user: root
    workdir: /tmp
    timeout: 30s
    on_error: fail
`)
		require.NoError(t, err)
		s := c.Scripts.Start
		require.Equal(t, []string{"sh", "-c", "id"}, s.Run.Cmd())
		require.Equal(t, "root", s.User)
		require.Equal(t, "/tmp", s.Workdir)
		require.Equal(t, 30*time.Second, s.TimeoutOrDefault())
		require.True(t, s.Fatal())
	})

	t.Run("rejects an empty run", func(t *testing.T) {
		_, err := parse(t, "scripts:\n  setup:\n    user: root\n")
		require.ErrorContains(t, err, "run is required")
	})

	t.Run("rejects an unknown on_error", func(t *testing.T) {
		_, err := parse(t, "scripts:\n  setup:\n    run: x\n    on_error: explode\n")
		require.ErrorContains(t, err, "on_error")
	})
}

func TestFileValidation(t *testing.T) {
	t.Run("accepts a home-relative source", func(t *testing.T) {
		c, err := parse(t, "files:\n  - src: ~/.docker/build01/\n    dst: ${HOME}/.docker-remote\n")
		require.NoError(t, err)
		mode, err := c.Files[0].Perm()
		require.NoError(t, err)
		require.EqualValues(t, config.FileModeDefault, mode)
	})

	t.Run("rejects a source outside the home", func(t *testing.T) {
		// The daemon only ever sees the host home, read-only.
		_, err := parse(t, "files:\n  - src: /etc/docker/certs\n    dst: /tmp/certs\n")
		require.ErrorContains(t, err, "under the home directory")
	})

	t.Run("rejects a traversal", func(t *testing.T) {
		_, err := parse(t, "files:\n  - src: ~/../etc\n    dst: /tmp/x\n")
		require.ErrorContains(t, err, "\"..\"")
	})

	t.Run("rejects a relative destination", func(t *testing.T) {
		_, err := parse(t, "files:\n  - src: ~/x\n    dst: relative/path\n")
		require.ErrorContains(t, err, "absolute container path")
	})

	t.Run("rejects a bad mode", func(t *testing.T) {
		_, err := parse(t, "files:\n  - src: ~/x\n    dst: /tmp/x\n    mode: \"rw-\"\n")
		require.ErrorContains(t, err, "invalid mode")
	})
}

func TestMatchProjects(t *testing.T) {
	c, err := parse(t, `
projects:
  - match: ~/work/**
    env: {A: "1"}
  - match: [~/work/acme/**, ~/other/**]
    env: {B: "2"}
  - match: ~/nope/**
    env: {C: "3"}
`)
	require.NoError(t, err)

	t.Run("every matching block applies, in file order", func(t *testing.T) {
		got := c.MatchProjects(home(t) + "/work/acme/api")
		require.Len(t, got, 2)
		require.Contains(t, got[0].Env, "A")
		require.Contains(t, got[1].Env, "B")
	})

	t.Run("no match", func(t *testing.T) {
		require.Empty(t, c.MatchProjects("/srv/elsewhere"))
	})
}

func TestConfigRoundTrip(t *testing.T) {
	// `cld config` prints the effective config; the shorthand forms must come
	// back out as something that parses again.
	c, err := parse(t, "scripts:\n  setup: echo hi\nprojects:\n  - match: ~/w/**\n")
	require.NoError(t, err)

	b, err := yaml.Marshal(c)
	require.NoError(t, err)

	again, err := parse(t, string(b))
	require.NoError(t, err)
	require.Equal(t, c.Scripts.Setup.Run.Cmd(), again.Scripts.Setup.Run.Cmd())
	require.Equal(t, c.Projects[0].Match, again.Projects[0].Match)
}

// home is where a "~/" glob expands to, which is what MatchProjects matches
// the workspace path against.
func home(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	require.NoError(t, err)
	return h
}
