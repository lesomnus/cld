package config_test

import (
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func TestDockerFor(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		c, err := parse(t, "")
		require.NoError(t, err)

		got := c.DockerFor("/work/api")
		require.False(t, got.Enabled())
		require.Equal(t, config.DockerModeOff, got.Mode)
		require.Equal(t, config.DockerImageDefault, got.Image)
	})

	t.Run("a project block turns it on for its workspaces only", func(t *testing.T) {
		c, err := parse(t, `
projects:
  - match: /work/infra/**
    docker: {mode: dind}
`)
		require.NoError(t, err)
		require.True(t, c.DockerFor("/work/infra/api").Enabled())
		require.False(t, c.DockerFor("/work/other").Enabled())
	})

	t.Run("a project block overrides one field at a time", func(t *testing.T) {
		c, err := parse(t, `
docker:
  mode: dind
  image: docker:28-dind
projects:
  - match: /work/**
    docker: {image: docker:dind-rootless}
`)
		require.NoError(t, err)

		got := c.DockerFor("/work/api")
		require.True(t, got.Enabled(), "the global mode must survive an image-only override")
		require.Equal(t, "docker:dind-rootless", got.Image)

		require.Equal(t, "docker:28-dind", c.DockerFor("/elsewhere").Image)
	})

	t.Run("a project block can turn it back off", func(t *testing.T) {
		c, err := parse(t, `
docker: {mode: dind}
projects:
  - match: /work/secret/**
    docker: {mode: off}
`)
		require.NoError(t, err)
		require.True(t, c.DockerFor("/work/api").Enabled())
		require.False(t, c.DockerFor("/work/secret/thing").Enabled())
	})

	t.Run("rejects an unknown mode", func(t *testing.T) {
		_, err := parse(t, "docker: {mode: host-socket}\n")
		require.ErrorContains(t, err, "mode")

		_, err = parse(t, "projects:\n  - match: /w/**\n    docker: {mode: nope}\n")
		require.ErrorContains(t, err, "projects[0].docker")
	})
}
