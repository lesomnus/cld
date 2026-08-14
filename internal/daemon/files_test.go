package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func TestFileSpecs(t *testing.T) {
	d, cfg := newTestDaemon(t)
	cfg.Files = []config.FileSpec{{Src: "~/global", Dst: "/g"}}
	cfg.Projects = []config.ProjectConfig{
		{Match: config.StringList{"/work/**"}, Files: []config.FileSpec{{Src: "~/work", Dst: "/w"}}},
		{Match: config.StringList{"/elsewhere/**"}, Files: []config.FileSpec{{Src: "~/no", Dst: "/n"}}},
	}

	got := d.file_specs(&entry{item: Item{LocalFolder: "/work/api"}})
	require.Len(t, got, 2)
	require.Equal(t, "/g", got[0].Dst)
	require.Equal(t, "/w", got[1].Dst)
}

func TestExpandContainerPath(t *testing.T) {
	d, _ := newTestDaemon(t)
	e := &entry{home: "/home/dev", item: Item{Workspace: "/workspace"}}

	require.Equal(t, "/home/dev/.docker", d.expand_container_path(e, "${HOME}/.docker"))
	require.Equal(t, "/workspace/.env", d.expand_container_path(e, "${CLD_WORKSPACE}/.env"))
	require.Equal(t, "/etc/x", d.expand_container_path(e, "/etc/x"))

	t.Run("an unknown reference resolves to nothing, leaving a relative path", func(t *testing.T) {
		// Which install_files rejects rather than writing somewhere arbitrary.
		require.Equal(t, "/x", d.expand_container_path(e, "${NOPE}/x"))
	})
}

func TestHashSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cert.pem")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0o600))

	hash := func(p string, mode int64) string {
		fi, err := os.Stat(p)
		require.NoError(t, err)
		s, err := hash_source(p, fi, mode)
		require.NoError(t, err)
		return s
	}

	t.Run("changes with content", func(t *testing.T) {
		before := hash(file, 0o600)
		require.NoError(t, os.WriteFile(file, []byte("rotated"), 0o600))
		require.NotEqual(t, before, hash(file, 0o600))
	})

	t.Run("changes with mode", func(t *testing.T) {
		// A permission change alone must still re-place the file.
		require.NotEqual(t, hash(file, 0o600), hash(file, 0o644))
	})

	t.Run("is stable for unchanged input", func(t *testing.T) {
		require.Equal(t, hash(file, 0o600), hash(file, 0o600))
	})

	t.Run("covers a whole tree", func(t *testing.T) {
		tree := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(tree, "a"), []byte("1"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(tree, "sub"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(tree, "sub", "b"), []byte("2"), 0o600))
		before := hash(tree, 0o600)

		require.NoError(t, os.WriteFile(filepath.Join(tree, "sub", "b"), []byte("3"), 0o600))
		require.NotEqual(t, before, hash(tree, 0o600), "a nested change must be seen")

		require.NoError(t, os.WriteFile(filepath.Join(tree, "sub", "b"), []byte("2"), 0o600))
		require.Equal(t, before, hash(tree, 0o600), "restoring must restore the digest")
	})

	t.Run("distinguishes layout from content", func(t *testing.T) {
		// Same bytes, different names: the placement differs, so must the hash.
		one, two := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(one, "a"), []byte("x"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(two, "b"), []byte("x"), 0o600))
		require.NotEqual(t, hash(one, 0o600), hash(two, 0o600))
	})
}

func TestMarkerName(t *testing.T) {
	// Keyed by destination, but never able to escape the marker directory.
	require.NotEqual(t, marker_name("/home/dev/a"), marker_name("/home/dev/b"))
	require.Equal(t, marker_name("/home/dev/a"), marker_name("/home/dev/a"))
	require.NotContains(t, marker_name("/home/../etc/passwd"), "/")
}

func TestHostPath(t *testing.T) {
	t.Run("resolves through the host-home mount", func(t *testing.T) {
		cfg := &config.Config{HostHome: "/host-home"}
		require.Equal(t, "/host-home/.docker/ca.pem", cfg.HostPath("~/.docker/ca.pem"))
	})

	t.Run("rejects anything not under the home", func(t *testing.T) {
		cfg := &config.Config{HostHome: "/host-home"}
		require.Equal(t, "", cfg.HostPath("/etc/docker/ca.pem"))
		require.Equal(t, "", cfg.HostPath("relative"))
	})
}
