package devc_test

import (
	"testing"

	"github.com/lesomnus/cld/internal/devc"
	"github.com/stretchr/testify/require"
)

func TestDisplayName(t *testing.T) {
	t.Run("basename of the local folder", func(t *testing.T) {
		require.Equal(t, "cld", devc.DisplayName("/home/hypnos/workspaces/cld"))
	})
	t.Run("trailing slash", func(t *testing.T) {
		require.Equal(t, "cld", devc.DisplayName("/home/hypnos/workspaces/cld/"))
	})
}

func TestSessionName(t *testing.T) {
	t.Run("prefixed", func(t *testing.T) {
		require.Equal(t, "cld-foo", devc.SessionName("foo"))
	})
	t.Run("tmux target characters are replaced", func(t *testing.T) {
		require.Equal(t, "cld-foo_bar_baz", devc.SessionName("foo.bar:baz"))
	})
}

func TestRemoteUser(t *testing.T) {
	t.Run("last remoteUser wins", func(t *testing.T) {
		v := devc.RemoteUser(`[{"remoteUser":"root"},{"foo":1},{"remoteUser":"vscode"}]`)
		require.Equal(t, "vscode", v)
	})
	t.Run("absent", func(t *testing.T) {
		require.Equal(t, "", devc.RemoteUser(`[{"foo":1}]`))
	})
	t.Run("empty or invalid", func(t *testing.T) {
		require.Equal(t, "", devc.RemoteUser(""))
		require.Equal(t, "", devc.RemoteUser("not json"))
	})
}

func TestWorkspaceFolder(t *testing.T) {
	mounts := []devc.Mount{
		{Source: "/home/hypnos/.claude", Destination: "/home/hypnos/.claude"},
		{Source: "/home/hypnos/workspaces/cld", Destination: "/workspace"},
	}
	t.Run("explicit workspaceFolder wins", func(t *testing.T) {
		config := []byte(`{
			// Comments are fine in devcontainer.json.
			"workspaceFolder": "/workspaces/custom",
		}`)
		v := devc.WorkspaceFolder(config, "/home/hypnos/workspaces/cld", mounts)
		require.Equal(t, "/workspaces/custom", v)
	})
	t.Run("falls back to the mount destination", func(t *testing.T) {
		v := devc.WorkspaceFolder([]byte(`{"service":"dev"}`), "/home/hypnos/workspaces/cld", mounts)
		require.Equal(t, "/workspace", v)
	})
	t.Run("no match", func(t *testing.T) {
		v := devc.WorkspaceFolder(nil, "/somewhere/else", mounts)
		require.Equal(t, "", v)
	})
	t.Run("expands localWorkspaceFolderBasename", func(t *testing.T) {
		config := []byte(`{"workspaceFolder": "/workspaces/${localWorkspaceFolderBasename}"}`)
		v := devc.WorkspaceFolder(config, "/home/hypnos/workspaces/cld", mounts)
		require.Equal(t, "/workspaces/cld", v)
	})
	t.Run("unresolvable variable falls through to the mount", func(t *testing.T) {
		config := []byte(`{"workspaceFolder": "${containerEnv:SOMETHING}/app"}`)
		v := devc.WorkspaceFolder(config, "/home/hypnos/workspaces/cld", mounts)
		require.Equal(t, "/workspace", v)
	})
}
