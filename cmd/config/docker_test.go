package config_test

import (
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func TestDockerFor(t *testing.T) {
	t.Run("a shared engine by default", func(t *testing.T) {
		c, err := parse(t, "")
		require.NoError(t, err)

		got := c.DockerFor("/work/api")
		require.True(t, got.Enabled())
		require.True(t, got.Shared())
		require.Equal(t, config.DockerImageDefault, got.Image)
	})

	t.Run("a project block can opt out", func(t *testing.T) {
		c, err := parse(t, `
projects:
  - match: /work/secret/**
    docker: {mode: off}
`)
		require.NoError(t, err)
		require.True(t, c.DockerFor("/work/api").Enabled())
		require.False(t, c.DockerFor("/work/secret/thing").Enabled())
	})

	t.Run("a project block can take an engine of its own", func(t *testing.T) {
		c, err := parse(t, `
projects:
  - match: /work/infra/**
    docker: {scope: project}
`)
		require.NoError(t, err)
		require.False(t, c.DockerFor("/work/infra/api").Shared())
		require.True(t, c.DockerFor("/work/other").Shared())
	})

	t.Run("everything off globally", func(t *testing.T) {
		c, err := parse(t, "docker: {mode: off}\n")
		require.NoError(t, err)
		require.False(t, c.DockerFor("/work/api").Enabled())
	})

	t.Run("the global image and override apply to the shared engine", func(t *testing.T) {
		c, err := parse(t, "docker: {image: docker:28-dind, compose: shared.yaml}\n")
		require.NoError(t, err)

		got := c.DockerFor("/work/api")
		require.Equal(t, "docker:28-dind", got.Image)
		require.Equal(t, "shared.yaml", got.Compose)
	})

	t.Run("a project engine can be shaped", func(t *testing.T) {
		c, err := parse(t, `
projects:
  - match: /work/infra/**
    docker: {scope: project, image: docker:dind-rootless, compose: infra.yaml}
`)
		require.NoError(t, err)

		got := c.DockerFor("/work/infra/api")
		require.False(t, got.Shared())
		require.Equal(t, "docker:dind-rootless", got.Image)
		require.Equal(t, "infra.yaml", got.Compose)
	})

	t.Run("shaping the shared engine from one project is rejected", func(t *testing.T) {
		// Two projects resolving different specs for one engine would rebuild
		// it out from under each other on every reconcile.
		_, err := parse(t, "projects:\n  - match: /w/**\n    docker: {image: docker:28-dind}\n")
		require.ErrorContains(t, err, "scope: project")

		_, err = parse(t, "projects:\n  - match: /w/**\n    docker: {compose: x.yaml}\n")
		require.ErrorContains(t, err, "scope: project")
	})

	t.Run("a project cannot reshape the shared engine even in code", func(t *testing.T) {
		// The load-time rule above cannot see a Config built programmatically,
		// so resolution enforces it too.
		c := &config.Config{
			Docker: config.DockerConfig{Image: "docker:dind"},
			Projects: []config.ProjectConfig{{
				Match:  config.StringList{"/work/**"},
				Docker: config.DockerConfig{Image: "sneaky:latest", Compose: "sneaky.yaml"},
			}},
		}
		got := c.DockerFor("/work/api")
		require.Equal(t, "docker:dind", got.Image)
		require.Empty(t, got.Compose)
	})

	t.Run("rejects an unknown mode or scope", func(t *testing.T) {
		_, err := parse(t, "docker: {mode: host-socket}\n")
		require.ErrorContains(t, err, "mode")

		_, err = parse(t, "docker: {scope: global}\n")
		require.ErrorContains(t, err, "scope")

		_, err = parse(t, "projects:\n  - match: /w/**\n    docker: {mode: nope}\n")
		require.ErrorContains(t, err, "projects[0].docker")
	})
}
