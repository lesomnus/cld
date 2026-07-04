package installer

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

func TestSpecFor(t *testing.T) {
	t.Run("local socket is bind-mounted", func(t *testing.T) {
		s, err := SpecFor("img", "", "/home/u/.cache/cld", "/home/u/.local/share/cld", 1000, 1001)
		require.NoError(t, err)
		require.Equal(t, []string{"serve"}, s.Cmd)
		require.Contains(t, s.Binds, "/var/run/docker.sock:/var/run/docker.sock")
		require.Contains(t, s.Binds, "/home/u/.cache/cld:/data/cache/cld")
		require.Contains(t, s.Binds, "/home/u/.local/share/cld:/data/share/cld")
		require.Contains(t, s.Env, "DOCKER_HOST=unix:///var/run/docker.sock")
		require.Contains(t, s.Env, "PUID=1000")
		require.Contains(t, s.Env, "PGID=1001")
	})

	t.Run("remote engine is refused", func(t *testing.T) {
		_, err := SpecFor("img", "tcp://docker:2375", "/home/u/.cache/cld", "/home/u/.local/share/cld", 1000, 1000)
		require.Error(t, err) // the shared cache-dir bind needs a local engine
	})
}

// TestSpecMirrorsCompose guards the reference docker-compose.yaml against drift:
// its `cld` service must match the spec `cld install` uses (values like the tag,
// $HOME and uid are templated in compose, so only structure is compared).
func TestSpecMirrorsCompose(t *testing.T) {
	data, err := os.ReadFile("../../docker-compose.yaml")
	if errors.Is(err, os.ErrNotExist) {
		// The reference compose file is deliberately excluded from the Docker
		// build context (.dockerignore), so this drift guard cannot run inside
		// the image build's test stage. It runs in local dev and the `test` CI
		// job, where the full checkout is present.
		t.Skip("docker-compose.yaml not in context (excluded by .dockerignore)")
	}
	require.NoError(t, err)

	var cf struct {
		Services map[string]struct {
			Image       string   `yaml:"image"`
			Command     []string `yaml:"command"`
			Environment []string `yaml:"environment"`
			Volumes     []string `yaml:"volumes"`
			Restart     string   `yaml:"restart"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(data, &cf))
	svc, ok := cf.Services["cld"]
	require.True(t, ok, "compose must have a `cld` service")

	spec, err := SpecFor("ghcr.io/lesomnus/cld:edge", "", "/home/u/.cache/cld", "/home/u/.local/share/cld", 1000, 1000)
	require.NoError(t, err)

	require.Equal(t, svc.Command, spec.Cmd, "command")
	require.Equal(t, svc.Restart, spec.Restart, "restart policy")
	require.Equal(t, imageRepo(svc.Image), imageRepo(spec.Image), "image repo")
	require.ElementsMatch(t, envKeys(svc.Environment), envKeys(spec.Env), "env var set")
	require.ElementsMatch(t, volumeTargets(svc.Volumes), volumeTargets(spec.Binds), "mount targets")
}

// imageRepo strips the tag: the first ':' after the last '/' (registry ports
// aside, which ghcr.io has none of).
func imageRepo(ref string) string {
	slash := strings.LastIndex(ref, "/")
	if i := strings.Index(ref[slash+1:], ":"); i >= 0 {
		return ref[:slash+1+i]
	}
	return ref
}

func envKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		keys = append(keys, k)
	}
	return keys
}

func volumeTargets(vols []string) []string {
	targets := make([]string, 0, len(vols))
	for _, v := range vols {
		parts := strings.Split(v, ":")
		require_at_least_two(parts)
		targets = append(targets, parts[1])
	}
	return targets
}

func require_at_least_two(parts []string) {
	if len(parts) < 2 {
		panic("volume must be src:dst")
	}
}
