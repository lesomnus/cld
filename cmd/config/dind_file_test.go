package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// withOverride writes cld.yaml and an override file into a temp config
// directory and loads the pair the way a running cld would.
func withOverride(t *testing.T, cldYaml, name, body string) *config.Config {
	t.Helper()
	dir := t.TempDir()

	p := filepath.Join(dir, "cld.yaml")
	require.NoError(t, os.WriteFile(p, []byte(cldYaml), 0o644))
	if name != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}

	c, err := config.ReadFromFile(p)
	require.NoError(t, err)
	require.NoError(t, c.Evaluate())
	return c
}

func TestLoadDindOverride(t *testing.T) {
	t.Run("picked up from beside cld.yaml", func(t *testing.T) {
		c := withOverride(t, "docker: {mode: dind}\n", config.DindFileName, `
services:
  dind:
    command: ["--insecure-registry", "reg:5000"]
    environment:
      HTTP_PROXY: http://proxy:3128
    volumes:
      - /host/certs:/etc/docker/certs.d:ro
    cpus: 2.5
`)
		got, err := c.LoadDindOverride("/work/api")
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, []string{"--insecure-registry", "reg:5000"}, []string(got.Command))
		require.Equal(t, "http://proxy:3128", got.Environment["HTTP_PROXY"])
		require.Equal(t, []string{"/host/certs:/etc/docker/certs.d:ro"}, got.Volumes)
		require.Equal(t, 2.5, got.Cpus)
	})

	t.Run("absent when there is no file", func(t *testing.T) {
		c := withOverride(t, "docker: {mode: dind}\n", "", "")
		got, err := c.LoadDindOverride("/work/api")
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("a file named explicitly must exist", func(t *testing.T) {
		// Silently ignoring it would leave the user believing settings apply.
		c := withOverride(t, "docker: {mode: dind, compose: nope.yaml}\n", "", "")
		_, err := c.LoadDindOverride("/work/api")
		require.ErrorContains(t, err, "no such file")
	})

	t.Run("a project block can point at its own file", func(t *testing.T) {
		c := withOverride(t, `
docker: {mode: dind}
projects:
  - match: /work/infra/**
    docker: {compose: infra.yaml}
`, "infra.yaml", "services:\n  dind:\n    cpus: 8\n")

		got, err := c.LoadDindOverride("/work/infra/api")
		require.NoError(t, err)
		require.Equal(t, float64(8), got.Cpus)

		// Another workspace falls back to the default name, which is absent.
		got, err = c.LoadDindOverride("/work/other")
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("environment as a list", func(t *testing.T) {
		c := withOverride(t, "docker: {mode: dind}\n", config.DindFileName,
			"services:\n  dind:\n    environment:\n      - A=1\n      - B=2\n")
		got, err := c.LoadDindOverride("/work/api")
		require.NoError(t, err)
		require.Equal(t, config.EnvList{"A": "1", "B": "2"}, got.Environment)
	})

	t.Run("rejects a key cld cannot apply", func(t *testing.T) {
		// Accepting a healthcheck and ignoring it is the failure mode worth
		// avoiding: the user would believe it is in effect.
		c := withOverride(t, "docker: {mode: dind}\n", config.DindFileName,
			"services:\n  dind:\n    healthcheck:\n      test: [CMD, true]\n")
		_, err := c.LoadDindOverride("/work/api")
		require.ErrorContains(t, err, "healthcheck")
	})

	t.Run("rejects a wrong service name", func(t *testing.T) {
		c := withOverride(t, "docker: {mode: dind}\n", config.DindFileName,
			"services:\n  docker:\n    cpus: 2\n")
		_, err := c.LoadDindOverride("/work/api")
		require.ErrorContains(t, err, "dind")
	})

	t.Run("rejects a relative volume source", func(t *testing.T) {
		// The engine resolves it on the host, where the daemon's own view of
		// paths — and "~" — means nothing.
		c := withOverride(t, "docker: {mode: dind}\n", config.DindFileName,
			"services:\n  dind:\n    volumes: [\"~/certs:/certs\"]\n")
		_, err := c.LoadDindOverride("/work/api")
		require.ErrorContains(t, err, "absolute host path")
	})
}

func TestDindFileParsesAsCompose(t *testing.T) {
	// The file is compose-shaped on purpose: `docker compose -f cld.dind.yaml
	// config` should still make sense of it.
	body := `
services:
  dind:
    image: docker:dind-rootless
    privileged: false
    ports: ["12375:2375"]
`
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(body), &doc))
	require.Contains(t, doc.Services, "dind")
}
