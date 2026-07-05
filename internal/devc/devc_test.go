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

func TestProjectName(t *testing.T) {
	t.Run("reads name", func(t *testing.T) {
		require.Equal(t, "lesomnus/cld", devc.ProjectName([]byte(`{
			// devcontainer.json
			"name": "lesomnus/cld",
			"service": "dev",
		}`)))
	})
	t.Run("absent", func(t *testing.T) {
		require.Equal(t, "", devc.ProjectName([]byte(`{"service":"dev"}`)))
	})
	t.Run("empty or invalid", func(t *testing.T) {
		require.Equal(t, "", devc.ProjectName(nil))
		require.Equal(t, "", devc.ProjectName([]byte("nope")))
	})
}

func TestSlug(t *testing.T) {
	t.Run("keeps safe characters", func(t *testing.T) {
		require.Equal(t, "my_app-1.2", devc.Slug("my_app-1.2"))
	})
	t.Run("collapses unsafe runs to a single dash", func(t *testing.T) {
		require.Equal(t, "lesomnus-cld", devc.Slug("lesomnus/cld"))
		require.Equal(t, "a-b", devc.Slug("a  //  b"))
	})
	t.Run("trims leading and trailing separators", func(t *testing.T) {
		require.Equal(t, "app", devc.Slug("  /app/  "))
	})
	t.Run("empty when nothing survives", func(t *testing.T) {
		require.Equal(t, "", devc.Slug("///"))
	})
}

func TestAlias(t *testing.T) {
	t.Run("short name is its own alias", func(t *testing.T) {
		require.Equal(t, "cld", devc.Alias("cld"))
		require.Equal(t, "webapi", devc.Alias("webapi"))
	})
	t.Run("multi-word name becomes its initials", func(t *testing.T) {
		require.Equal(t, "mwa", devc.Alias("my-web-app"))
		require.Equal(t, "op", devc.Alias("observability-platform"))
		require.Equal(t, "svlt", devc.Alias("some_very_long_thing"))
	})
	t.Run("long single word is truncated", func(t *testing.T) {
		require.Equal(t, "really", devc.Alias("reallylongsingleword"))
	})
	t.Run("lowercased and slugged", func(t *testing.T) {
		require.Equal(t, "op", devc.Alias("Observability/Platform"))
	})
	t.Run("empty when nothing survives", func(t *testing.T) {
		require.Equal(t, "", devc.Alias("///"))
	})
}

func TestFingerprint(t *testing.T) {
	t.Run("deterministic and non-empty", func(t *testing.T) {
		a := devc.Fingerprint("/home/me/work/cld")
		require.Equal(t, a, devc.Fingerprint("/home/me/work/cld"))
		require.NotEmpty(t, a)
	})
	t.Run("different seeds differ", func(t *testing.T) {
		require.NotEqual(t,
			devc.Fingerprint("/home/me/work/cld"),
			devc.Fingerprint("/home/me/other/cld"),
		)
	})
	t.Run("only crockford-lower characters", func(t *testing.T) {
		fp := devc.Fingerprint("/some/path")
		require.NotEmpty(t, fp)
		for _, r := range fp {
			require.Contains(t, "0123456789abcdefghjkmnpqrstvwxyz", string(r))
		}
	})
	t.Run("empty seed still yields a valid digest", func(t *testing.T) {
		// FNV-1a of "" is the nonzero offset basis, so this is a normal digest.
		require.NotEmpty(t, devc.Fingerprint(""))
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
