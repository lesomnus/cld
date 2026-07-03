package daemon

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMergeVSCodeProfile(t *testing.T) {
	parse := func(t *testing.T, b []byte) map[string]any {
		var v map[string]any
		require.NoError(t, json.Unmarshal(b, &v))
		return v
	}
	profiles := func(t *testing.T, b []byte) map[string]any {
		return parse(t, b)[vscodeProfileKey].(map[string]any)
	}

	t.Run("adds the profile to empty settings", func(t *testing.T) {
		out, changed, err := merge_vscode_profile(nil)
		require.NoError(t, err)
		require.True(t, changed)
		claude := profiles(t, out)["claude"].(map[string]any)
		require.Equal(t, "cld", claude["path"])
		require.Equal(t, []any{"it"}, claude["args"])
	})

	t.Run("preserves other settings and profiles", func(t *testing.T) {
		in := []byte(`{
			"editor.fontSize": 14,
			"terminal.integrated.profiles.linux": {
				"bash": {"path": "bash"}
			}
		}`)
		out, changed, err := merge_vscode_profile(in)
		require.NoError(t, err)
		require.True(t, changed)

		doc := parse(t, out)
		require.Equal(t, float64(14), doc["editor.fontSize"])
		p := profiles(t, out)
		require.Contains(t, p, "bash") // existing profile kept
		require.Contains(t, p, "claude")
	})

	t.Run("idempotent once present", func(t *testing.T) {
		first, _, err := merge_vscode_profile(nil)
		require.NoError(t, err)
		_, changed, err := merge_vscode_profile(first)
		require.NoError(t, err)
		require.False(t, changed) // no rewrite when already correct
	})

	t.Run("rejects invalid json", func(t *testing.T) {
		_, _, err := merge_vscode_profile([]byte("nope"))
		require.Error(t, err)
	})
}
