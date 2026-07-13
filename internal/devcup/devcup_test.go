package devcup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasConfig(t *testing.T) {
	t.Run("dotfolder config", func(t *testing.T) {
		ws := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(ws, ".devcontainer"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(ws, ".devcontainer", "devcontainer.json"), []byte("{}"), 0o644))
		require.True(t, HasConfig(ws))
	})
	t.Run("dotfile config", func(t *testing.T) {
		ws := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(ws, ".devcontainer.json"), []byte("{}"), 0o644))
		require.True(t, HasConfig(ws))
	})
	t.Run("absent", func(t *testing.T) {
		require.False(t, HasConfig(t.TempDir()))
	})
}

func TestDiscoverConfigs(t *testing.T) {
	write := func(t *testing.T, ws, rel string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("{}"), 0o644))
	}

	t.Run("none", func(t *testing.T) {
		require.Empty(t, DiscoverConfigs(t.TempDir()))
	})
	t.Run("standard locations are marked standard and ordered", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, ".devcontainer.json")
		write(t, ws, ".devcontainer/devcontainer.json")

		got := DiscoverConfigs(ws)
		require.Len(t, got, 2)
		require.Equal(t, ".devcontainer/devcontainer.json", got[0].Rel)
		require.True(t, got[0].Standard)
		require.Equal(t, ".devcontainer.json", got[1].Rel)
		require.True(t, got[1].Standard)
	})
	t.Run("sub-folder configs are discovered, sorted, and non-standard", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, ".devcontainer/devcontainer.json")
		write(t, ws, ".devcontainer/go/devcontainer.json")
		write(t, ws, ".devcontainer/alpine/devcontainer.json")

		got := DiscoverConfigs(ws)
		require.Len(t, got, 3)
		// primary standard first, then sub-folders sorted by name.
		require.Equal(t, ".devcontainer/devcontainer.json", got[0].Rel)
		require.Equal(t, ".devcontainer/alpine/devcontainer.json", got[1].Rel)
		require.Equal(t, "alpine", got[1].Label)
		require.False(t, got[1].Standard)
		require.Equal(t, ".devcontainer/go/devcontainer.json", got[2].Rel)
		require.Equal(t, "go", got[2].Label)
	})
	t.Run("a sub-folder without devcontainer.json is ignored", func(t *testing.T) {
		ws := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(ws, ".devcontainer", "empty"), 0o755))
		require.Empty(t, DiscoverConfigs(ws))
	})
	t.Run("paths are absolute and under the workspace", func(t *testing.T) {
		ws := t.TempDir()
		write(t, ws, ".devcontainer/go/devcontainer.json")
		got := DiscoverConfigs(ws)
		require.Len(t, got, 1)
		require.Equal(t, filepath.Join(ws, ".devcontainer", "go", "devcontainer.json"), got[0].Path)
	})
}

func TestResolve(t *testing.T) {
	o := Options{Workspace: "/w", RunnerImage: "img"}
	containerized := func(context.Context) error { return nil }

	t.Run("prefers the devcontainer binary", func(t *testing.T) {
		look := func(name string) (string, error) {
			if name == "devcontainer" {
				return "/usr/bin/devcontainer", nil
			}
			return "", errors.New("not found")
		}
		r := Resolve(o, look, containerized)
		require.Contains(t, r.Desc, "/usr/bin/devcontainer")
	})
	t.Run("falls back to the runner image", func(t *testing.T) {
		look := func(string) (string, error) { return "", errors.New("not found") }
		r := Resolve(o, look, containerized)
		require.Contains(t, r.Desc, "img")
	})
}

func TestAccessFor(t *testing.T) {
	t.Run("default socket", func(t *testing.T) {
		a, err := AccessFor("")
		require.NoError(t, err)
		require.Equal(t, "/var/run/docker.sock:/var/run/docker.sock", a.Bind)
		require.Empty(t, a.Env)
	})
	t.Run("unix socket path is normalized into the runner", func(t *testing.T) {
		a, err := AccessFor("unix:///run/user/1000/docker.sock")
		require.NoError(t, err)
		require.Equal(t, "/run/user/1000/docker.sock:/var/run/docker.sock", a.Bind)
		require.Empty(t, a.Env)
	})
	t.Run("tcp passes through as env", func(t *testing.T) {
		a, err := AccessFor("tcp://docker:2375")
		require.NoError(t, err)
		require.Empty(t, a.Bind)
		require.Equal(t, "DOCKER_HOST=tcp://docker:2375", a.Env)
	})
	t.Run("unsupported scheme", func(t *testing.T) {
		_, err := AccessFor("ssh://host")
		require.Error(t, err)
	})
}

func TestRunContainerizedRefusesRemoteEngine(t *testing.T) {
	// A tcp DOCKER_HOST means the engine may be remote and cannot see the
	// local workspace; RunContainerized must refuse rather than mount an empty
	// directory. It fails before any Docker call, so a nil client is fine.
	t.Setenv("DOCKER_HOST", "tcp://remote:2375")
	err := RunContainerized(context.Background(), nil, Options{
		Workspace:   "/w",
		RunnerImage: "img",
		Stderr:      io.Discard,
	})
	require.ErrorContains(t, err, "local Docker engine")
}

func TestUpArgs(t *testing.T) {
	o := Options{Workspace: "/w", Args: []string{"--remove-existing-container"}}
	require.Equal(t,
		[]string{"up", "--workspace-folder", "/w", "--remove-existing-container"},
		o.up_args())
}
